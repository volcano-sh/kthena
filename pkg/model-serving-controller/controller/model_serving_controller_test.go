/*
Copyright The Volcano Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type resourceSpec struct {
	name   string
	labels map[string]string
}

func TestIsServingGroupOutdated(t *testing.T) {
	ns := "test-ns"
	groupName := "test-group"
	group := datastore.ServingGroup{Name: groupName}
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
	newHash := "hash123"

	kubeClient := kubefake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	err := podInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)
	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	c := &ModelServingController{
		podsLister:   podInformer.Lister(),
		podsInformer: podInformer.Informer(),
	}

	cases := []struct {
		name string
		pods []resourceSpec
		want bool
	}{
		{
			name: "no pods",
			pods: nil,
			want: false,
		},
		{
			name: "no revision label",
			pods: []resourceSpec{
				{name: "pod1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			want: true,
		},
		{
			name: "revision not match",
			pods: []resourceSpec{
				{name: "pod2", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName, workloadv1alpha1.RevisionLabelKey: "oldhash"}},
			},
			want: true,
		},
		{
			name: "revision match",
			pods: []resourceSpec{
				{name: "pod3", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName, workloadv1alpha1.RevisionLabelKey: newHash}},
			},
			want: false,
		},
	}

	indexer := podInformer.Informer().GetIndexer()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// clean indexer
			for _, obj := range indexer.List() {
				err := indexer.Delete(obj)
				assert.NoError(t, err)
			}
			for _, p := range tc.pods {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      p.name,
						Labels:    p.labels,
					},
				}
				err := indexer.Add(pod)
				assert.NoError(t, err)
			}
			got := c.isServingGroupOutdated(group, ms.Namespace, newHash)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCheckServingGroupReady(t *testing.T) {
	ns := "default"
	groupName := "test-group"
	newHash := "hash123"
	roleLabel := "prefill"
	roleName := "prefill-0"

	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	err := podInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)
	store := datastore.New()
	// build controller
	controller := &ModelServingController{
		podsInformer: podInformer.Informer(),
		podsLister:   podInformer.Lister(),
		store:        store,
	}
	stop := make(chan struct{})
	defer close(stop)
	kubeInformerFactory.Start(stop)
	kubeInformerFactory.WaitForCacheSync(stop)

	// build ModelServing
	var expectedPodNum int32 = 2
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-ms",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Replicas: &expectedPodNum,
					},
				},
			},
		},
	}

	indexer := podInformer.Informer().GetIndexer()
	// Add 2 pods with labels matching group
	for i := 0; i < int(expectedPodNum); i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      fmt.Sprintf("pod-%d", i),
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
				},
			},
		}
		err := indexer.Add(pod)
		assert.NoError(t, err)
		store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), groupName, pod.Name, newHash, roleLabel, roleName)
	}

	// Waiting for pod cache to sync
	sync := waitForObjectInCache(t, 2*time.Second, func() bool {
		pods, _ := controller.podsLister.Pods(ns).List(labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
		}))
		return len(pods) == int(expectedPodNum)
	})
	assert.True(t, sync, "Pods should be found in cache")

	ok, err := controller.checkServingGroupReady(ms, groupName)
	assert.NoError(t, err)
	assert.True(t, ok)

	// case2: Pod quantity mismatch
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "pod-1",
			Labels: map[string]string{
				workloadv1alpha1.GroupNameLabelKey: groupName,
			},
		},
	}
	err = indexer.Delete(pod)
	assert.NoError(t, err)
	sync = waitForObjectInCache(t, 2*time.Second, func() bool {
		pods, _ := controller.podsLister.Pods(ns).List(labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
		}))
		return len(pods) == int(expectedPodNum)-1
	})
	assert.True(t, sync, "Pods should be found in cache after deletion")
	store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), groupName, "pod-1")

	ok, err = controller.checkServingGroupReady(ms, groupName)
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestIsServingGroupDeleted(t *testing.T) {
	ns := "default"
	groupName := "test-ms-0"
	otherGroupName := "other-group"

	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	serviceInformer := kubeInformerFactory.Core().V1().Services()

	err := podInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)

	err = serviceInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)

	store := datastore.New()
	controller := &ModelServingController{
		podsInformer:     podInformer.Informer(),
		servicesInformer: serviceInformer.Informer(),
		podsLister:       podInformer.Lister(),
		servicesLister:   serviceInformer.Lister(),
		store:            store,
	}

	stop := make(chan struct{})
	defer close(stop)
	kubeInformerFactory.Start(stop)
	kubeInformerFactory.WaitForCacheSync(stop)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-ms",
		},
	}

	cases := []struct {
		name               string
		pods               []resourceSpec
		services           []resourceSpec
		servingGroupStatus datastore.ServingGroupStatus
		want               bool
	}{
		{
			name:               "ServingGroup status is not Deleting - should return false",
			pods:               nil,
			services:           nil,
			servingGroupStatus: datastore.ServingGroupCreating,
			want:               false,
		},
		{
			name:               "ServingGroup status is Deleting - no resources - should return true",
			pods:               nil,
			services:           nil,
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               true,
		},
		{
			name: "ServingGroup status is Deleting - target group pods exist - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			services:           nil,
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               false,
		},
		{
			name: "ServingGroup status is Deleting - target group services exist - should return false",
			pods: nil,
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               false,
		},
		{
			name: "ServingGroup status is Deleting - both target group resources exist - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               false,
		},
		{
			name: "ServingGroup status is Deleting - only other group resources exist - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: otherGroupName}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: otherGroupName}},
			},
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               true,
		},
		{
			name: "ServingGroup status is Deleting - mixed group resources - target group exists - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
				{name: "pod-2", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: otherGroupName}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: otherGroupName}},
			},
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               false,
		},
		{
			name: "ServingGroup status is Deleting - multiple target group resources - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
				{name: "pod-2", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
				{name: "svc-2", labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: groupName}},
			},
			servingGroupStatus: datastore.ServingGroupDeleting,
			want:               false,
		},
	}

	podIndexer := podInformer.Informer().GetIndexer()
	serviceIndexer := serviceInformer.Informer().GetIndexer()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clean indexers before each test
			for _, obj := range podIndexer.List() {
				err := podIndexer.Delete(obj)
				assert.NoError(t, err)
			}
			for _, obj := range serviceIndexer.List() {
				err := serviceIndexer.Delete(obj)
				assert.NoError(t, err)
			}

			store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			err := store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, tc.servingGroupStatus)
			assert.NoError(t, err)

			// Add test pods
			for _, p := range tc.pods {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      p.name,
						Labels:    p.labels,
					},
				}
				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Add test services
			for _, s := range tc.services {
				service := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      s.name,
						Labels:    s.labels,
					},
				}
				err := serviceIndexer.Add(service)
				assert.NoError(t, err)
			}

			// Wait for cache to sync
			sync := waitForObjectInCache(t, 2*time.Second, func() bool {
				pods, _ := controller.podsLister.Pods(ns).List(labels.Everything())
				services, _ := controller.servicesLister.Services(ns).List(labels.Everything())
				return len(pods) == len(tc.pods) && len(services) == len(tc.services)
			})
			assert.True(t, sync, "Resources should be synced in cache")

			// Test the function
			got := controller.isServingGroupDeleted(ms, groupName)
			assert.Equal(t, tc.want, got, "isServingGroupDeleted result should match expected")

			store.DeleteServingGroup(utils.GetNamespaceName(ms), groupName)
		})
	}
}

func TestIsRoleDeleted(t *testing.T) {
	ns := "default"
	groupName := "test-ms-0"
	roleName := "prefill"
	roleID := "prefill-0"

	otherGroupName := "other-group"
	otherRoleName := "decode"
	otherRoleID := "decode-0"

	kubeClient := kubefake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	serviceInformer := kubeInformerFactory.Core().V1().Services()

	err := podInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)

	err = serviceInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
		RoleIDKey:    utils.RoleIDIndexFunc,
	})
	assert.NoError(t, err)

	store := datastore.New()
	controller := &ModelServingController{
		podsInformer:     podInformer.Informer(),
		servicesInformer: serviceInformer.Informer(),
		podsLister:       podInformer.Lister(),
		servicesLister:   serviceInformer.Lister(),
		store:            store,
	}

	stop := make(chan struct{})
	defer close(stop)
	kubeInformerFactory.Start(stop)
	kubeInformerFactory.WaitForCacheSync(stop)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-ms",
		},
	}

	cases := []struct {
		name       string
		pods       []resourceSpec
		services   []resourceSpec
		roleStatus datastore.RoleStatus
		want       bool
	}{
		{
			name:       "role status is not Deleting - should return false",
			pods:       nil,
			services:   nil,
			roleStatus: datastore.RoleCreating,
			want:       false,
		},
		{
			name:       "role status is Deleting - no resources - should return true",
			pods:       nil,
			services:   nil,
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
		{
			name: "role status is Deleting - target role pods exist - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			services:   nil,
			roleStatus: datastore.RoleDeleting,
			want:       false,
		},
		{
			name: "role status is Deleting - target role services exist - should return false",
			pods: nil,
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       false,
		},
		{
			name: "role status is Deleting - both target role resources exist - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       false,
		},
		{
			name: "role status is Deleting - only other group resources exist - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: otherGroupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: otherGroupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
		{
			name: "role status is Deleting - only other role resources exist - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      otherRoleName,
					workloadv1alpha1.RoleIDKey:         otherRoleID,
				}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      otherRoleName,
					workloadv1alpha1.RoleIDKey:         otherRoleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
		{
			name: "role status is Deleting - same group and roleName but different roleID - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         "prefill-1", // different roleID
				}},
			},
			services:   nil,
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
		{
			name: "role status is Deleting - mixed resources - target role exists - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
				{name: "pod-2", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      otherRoleName,
					workloadv1alpha1.RoleIDKey:         otherRoleID,
				}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: otherGroupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       false,
		},
		{
			name: "role status is Deleting - multiple target role resources - should return false",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
				{name: "pod-2", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			services: []resourceSpec{
				{name: "svc-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
				{name: "svc-2", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					workloadv1alpha1.RoleIDKey:         roleID,
				}},
			},
			roleStatus: datastore.RoleDeleting,
			want:       false,
		},
		{
			name: "role status is Deleting - incomplete label matching - missing RoleIDKey - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					workloadv1alpha1.RoleLabelKey:      roleName,
					// missing RoleIDKey
				}},
			},
			services:   nil,
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
		{
			name: "role status is Deleting - incomplete label matching - missing RoleLabelKey - should return true",
			pods: []resourceSpec{
				{name: "pod-1", labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: groupName,
					// missing RoleLabelKey
					workloadv1alpha1.RoleIDKey: roleID,
				}},
			},
			services:   nil,
			roleStatus: datastore.RoleDeleting,
			want:       true,
		},
	}

	podIndexer := podInformer.Informer().GetIndexer()
	serviceIndexer := serviceInformer.Informer().GetIndexer()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clean indexers before each test
			for _, obj := range podIndexer.List() {
				err := podIndexer.Delete(obj)
				assert.NoError(t, err)
			}
			for _, obj := range serviceIndexer.List() {
				err := serviceIndexer.Delete(obj)
				assert.NoError(t, err)
			}

			store.AddRole(utils.GetNamespaceName(ms), groupName, roleName, roleID, "test-revision")
			err := store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, roleName, roleID, tc.roleStatus)
			assert.NoError(t, err)

			// Add test pods
			for _, p := range tc.pods {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      p.name,
						Labels:    p.labels,
					},
				}
				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Add test services
			for _, s := range tc.services {
				service := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      s.name,
						Labels:    s.labels,
					},
				}
				err := serviceIndexer.Add(service)
				assert.NoError(t, err)
			}

			// Wait for cache to sync
			sync := waitForObjectInCache(t, 2*time.Second, func() bool {
				pods, _ := controller.podsLister.Pods(ns).List(labels.Everything())
				services, _ := controller.servicesLister.Services(ns).List(labels.Everything())
				return len(pods) == len(tc.pods) && len(services) == len(tc.services)
			})
			assert.True(t, sync, "Resources should be synced in cache")

			// Test the function
			got := controller.isRoleDeleted(ms, groupName, roleName, roleID)
			assert.Equal(t, tc.want, got, "isRoleDeleted result should match expected")

			store.DeleteServingGroup(utils.GetNamespaceName(ms), groupName)
		})
	}
}

func TestModelServingControllerModelServingLifecycle(t *testing.T) {
	// Create fake clients
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

	// Create informer factories
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	// Create controller
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
	assert.NoError(t, err)

	stop := make(chan struct{})
	defer close(stop)

	go controller.Run(context.Background(), 1)

	// Start informers
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	// Wait for cache sync
	cache.WaitForCacheSync(stop,
		controller.modelServingsInformer.HasSynced,
		controller.podsInformer.HasSynced,
		controller.servicesInformer.HasSynced,
	)

	// Test Case 1: ModelServing Creation
	t.Run("ModelServingCreate", func(t *testing.T) {
		ms := createStandardModelServing("test-ms", 2, 3)
		// Add ModelServing to fake client
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-ms")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Simulate controller processing the creation
		err = controller.syncModelServing(context.Background(), "default/test-ms")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(ms) * int(*ms.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, ms, 2)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, ms, 2)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, ms, 2)
	})

	// Test Case 2: ModelServing Scale Up
	t.Run("ModelServingScaleUp", func(t *testing.T) {
		ms := createStandardModelServing("test-ms-scale-up", 1, 2)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-ms-scale-up")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-ms-scale-up")
		assert.NoError(t, err)

		// Verify ServingGroups initial state
		verifyServingGroups(t, controller, ms, 1)

		// Update ModelServing to scale up
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](3) // Scale up to 3 ServingGroups

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-ms-scale-up")
			return err == nil && *ms.Spec.Replicas == 3
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update
		err = controller.syncModelServing(context.Background(), "default/test-ms-scale-up")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(updatedMI) * int(*updatedMI.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMI, 3)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, updatedMI, 3)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, updatedMI, 3)
	})

	// Test Case 3: ModelServing Update - Scale Down Replicas
	t.Run("ModelServingUpdateScaleDown", func(t *testing.T) {
		ms := createStandardModelServing("test-ms-scale-down", 3, 2)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-ms-scale-down")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-ms-scale-down")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(ms) * int(*ms.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, ms, 3)
		verifyPodCount(t, controller, ms, 3)
		verifyRoles(t, controller, ms, 3)

		// Update ModelServing to scale down
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](1) // Scale up to 1 ServingGroups

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-ms-scale-down")
			return err == nil && *ms.Spec.Replicas == 1
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update
		err = controller.syncModelServing(context.Background(), "default/test-ms-scale-down")
		assert.NoError(t, err)

		requirement, err := labels.NewRequirement(
			workloadv1alpha1.GroupNameLabelKey,
			selection.In,
			[]string{"test-ms-scale-down-1", "test-ms-scale-down-2"},
		)
		assert.NoError(t, err)

		selector := labels.NewSelector().Add(*requirement)
		podsToDelete, err := controller.podsLister.Pods("default").List(selector)
		assert.NoError(t, err)
		servicesToDelete, err := controller.servicesLister.Services("default").List(selector)
		assert.NoError(t, err)

		// Get the indexer of the Service Informer for simulating deletion
		svcIndexer := controller.servicesInformer.GetIndexer()

		// Simulate the deletion process of each Service
		for _, svc := range servicesToDelete {
			// Delete the Service from the indexer (simulating the Service disappearing from the cluster)
			err = svcIndexer.Delete(svc)
			assert.NoError(t, err)
		}

		// Get the indexer of the Pod Informer for simulating deletion
		podIndexer := controller.podsInformer.GetIndexer()

		// Simulate the deletion of each Pod
		for _, pod := range podsToDelete {
			// Delete the Pod from the indexer (simulating the Pod disappearing from the cluster)
			err = podIndexer.Delete(pod)
			assert.NoError(t, err)
			controller.deletePod(pod)
		}

		// Wait for pods to be created and synced to cache
		expectedPodCount = utils.ExpectedPodNum(updatedMI) * int(*updatedMI.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMI, 1)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, updatedMI, 1)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, updatedMI, 1)
	})

	// Test Case 4: ModelServing Update - Role Replicas Scale Up
	t.Run("ModelServingRoleReplicasScaleUp", func(t *testing.T) {
		ms := createStandardModelServing("test-role-scale-up", 2, 1)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-up")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-role-scale-up")
		assert.NoError(t, err)

		// Verify ServingGroups initial state
		verifyServingGroups(t, controller, ms, 2)

		// Update ModelServing to role scale down
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Template.Roles[0].Replicas = ptr.To[int32](3) // Scale up to 3 roles

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-up")
			return err == nil && *ms.Spec.Template.Roles[0].Replicas == 3
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(updatedMI) * int(*updatedMI.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMI, 2)

		// Verify total number of roles across all groups matches spec (role scaling doesn't guarantee per-group equality)
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMI))
		assert.NoError(t, err)
		totalRoles := 0
		for _, g := range servingGroups {
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMI), g.Name, "prefill")
			assert.NoError(t, err)
			totalRoles += len(roles)
		}
		// Spec says 3 replicas per group, across 2 groups that's 6 total
		assert.Equal(t, 6, totalRoles, "total prefill roles across all groups should match spec")

		// Verify total pods match expected count
		selector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
		})
		pods, err := controller.podsLister.Pods("default").List(selector)
		assert.NoError(t, err)
		assert.Equal(t, expectedPodCount, len(pods), "total pods across all groups should match expected count")
	})

	// Test Case 5: ModelServing Update - Role Replicas Scale Down
	t.Run("ModelServingRoleReplicasScaleDown", func(t *testing.T) {
		ms := createStandardModelServing("test-role-scale-down", 2, 3)

		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-down")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-role-scale-down")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(ms) * int(*ms.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, ms, 2)
		verifyPodCount(t, controller, ms, 2)
		verifyRoles(t, controller, ms, 2)

		// Update ModelServing to role scale down
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) // Scale down to 1 role

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-down")
			return err == nil && *ms.Spec.Template.Roles[0].Replicas == 1
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update - may need multiple syncs for role scaling
		err = controller.syncModelServing(context.Background(), "default/test-role-scale-down")
		assert.NoError(t, err)

		requirement, err := labels.NewRequirement(
			workloadv1alpha1.RoleIDKey,
			selection.In,
			[]string{"prefill-1", "prefill-2"},
		)
		assert.NoError(t, err)

		selector := labels.NewSelector().Add(*requirement)
		podsToDelete, err := controller.podsLister.Pods("default").List(selector)
		assert.NoError(t, err)
		servicesToDelete, err := controller.servicesLister.Services("default").List(selector)
		assert.NoError(t, err)

		// Get the indexer of the Service Informer for simulating deletion
		svcIndexer := controller.servicesInformer.GetIndexer()

		// Simulate the deletion process of each Service
		for _, svc := range servicesToDelete {
			// Delete the Service from the indexer (simulating the Service disappearing from the cluster)
			err = svcIndexer.Delete(svc)
			assert.NoError(t, err)
		}

		// Get the indexer of the Pod Informer for simulating deletion
		podIndexer := controller.podsInformer.GetIndexer()

		// Simulate the deletion of each Pod
		for _, pod := range podsToDelete {
			// Delete the Pod from the indexer (simulating the Pod disappearing from the cluster)
			err = podIndexer.Delete(pod)
			assert.NoError(t, err)
			controller.deletePod(pod)
		}

		// Wait for pods to be created and synced to cache
		expectedPodCount = utils.ExpectedPodNum(updatedMI) * int(*updatedMI.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMI, 2)

		// Verify total number of roles across all groups matches spec (role scaling doesn't guarantee per-group equality)
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMI))
		assert.NoError(t, err)
		totalRoles := 0
		for _, g := range servingGroups {
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMI), g.Name, "prefill")
			assert.NoError(t, err)
			totalRoles += len(roles)
		}
		// After scale down, specRole.Replicas == 1 per group, 2 groups total => 2 roles
		assert.Equal(t, 2, totalRoles, "total prefill roles across all groups should match spec after scale down")

		// Verify total pods match expected count
		podSelector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: updatedMI.Name,
		})
		allPods, err := controller.podsLister.Pods("default").List(podSelector)
		assert.NoError(t, err)
		assert.Equal(t, expectedPodCount, len(allPods), "total pods across all groups should match expected count after scale down")
	})

	// Test Case 6: ModelServing Scale Down with BinPack Strategy
	t.Run("ModelServingBinPackScaleDown", func(t *testing.T) {
		// ModelServing with PodDeletionCost annotation - BinPack Scale
		ms := createStandardModelServing("test-binpack-scale", 4, 1)

		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-binpack-scale")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-binpack-scale")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(ms) * int(*ms.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, ms, 4)
		verifyPodCount(t, controller, ms, 4)
		verifyRoles(t, controller, ms, 4)

		// Add PodDelectionCost annotations to pods
		// Get all pods and add different deletion costs
		pods, err := controller.podsLister.Pods("default").List(labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		}))
		assert.NoError(t, err)

		// Assign different deletion costs to pods in different ServingGroups
		for _, pod := range pods {
			// Extract ServingGroup index from pod name or labels
			groupName, exists := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
			assert.True(t, exists, "Pod should have GroupName label")

			// Determine ServingGroup index from name (e.g., test-binpack-scale-0, test-binpack-scale-1, etc.)
			var groupIndex int
			switch groupName {
			case "test-binpack-scale-0":
				groupIndex = 0
			case "test-binpack-scale-1":
				groupIndex = 1
			case "test-binpack-scale-2":
				groupIndex = 2
			case "test-binpack-scale-3":
				groupIndex = 3
			default:
				groupIndex = 0
			}

			// Add PodDelectionCost annotation - higher cost for group 0, lower for group 2
			cost := groupIndex * 30 // Group 0: 0, Group 1: 30, Group 2: 60, Group 3: 90
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations[PodDeletionCostAnnotation] = fmt.Sprintf("%d", cost)

			// Update pod in indexer to simulate annotation addition
			podIndexer := controller.podsInformer.GetIndexer()
			err = podIndexer.Update(pod)
			assert.NoError(t, err)
		}

		// Update ModelServing to scale down from 4 to 1 ServingGroup
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](1) // Scale down to 1 ServingGroup

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-binpack-scale")
			return err == nil && *ms.Spec.Replicas == 1
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update
		err = controller.syncModelServing(context.Background(), "default/test-binpack-scale")
		assert.NoError(t, err)

		// Identify ServingGroups to be deleted (with lower deletion cost)
		// Based on our cost assignment: Group 0 (cost 0) Group 1 (cost 30) and Group 2 (cost 60) should be deleted first
		requirement, err := labels.NewRequirement(
			workloadv1alpha1.GroupNameLabelKey,
			selection.In,
			[]string{"test-binpack-scale-0", "test-binpack-scale-1", "test-binpack-scale-2"},
		)
		assert.NoError(t, err)

		selector := labels.NewSelector().Add(*requirement)
		podsToDelete, err := controller.podsLister.Pods("default").List(selector)
		assert.NoError(t, err)
		servicesToDelete, err := controller.servicesLister.Services("default").List(selector)
		assert.NoError(t, err)

		// Get the indexer of the Service Informer for simulating deletion
		svcIndexer := controller.servicesInformer.GetIndexer()

		// Simulate the deletion process of each Service
		for _, svc := range servicesToDelete {
			// Delete the Service from the indexer (simulating the Service disappearing from the cluster)
			err = svcIndexer.Delete(svc)
			assert.NoError(t, err)
		}

		// Get the indexer of the Pod Informer for simulating deletion
		podIndexer := controller.podsInformer.GetIndexer()

		// Simulate the deletion of each Pod
		for _, pod := range podsToDelete {
			// Delete the Pod from the indexer (simulating the Pod disappearing from the cluster)
			err = podIndexer.Delete(pod)
			assert.NoError(t, err)
			controller.deletePod(pod)
		}

		// Wait for controller to process deletions
		time.Sleep(100 * time.Millisecond)

		// Instead of using generic helpers (verifyServingGroups/verifyRoles/verifyPodCount) which
		// assume contiguous, fully-populated groups, perform targeted checks for the binpack case.
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMI))
		assert.NoError(t, err)
		assert.Equal(t, 1, len(servingGroups))

		remainingRoles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMI), servingGroups[0].Name, "prefill")
		assert.NoError(t, err)
		assert.Equal(t, 1, len(remainingRoles))
		// We only assert that a single role remains; the exact ordinal is implementation-dependent.
		assert.NotEmpty(t, remainingRoles[0].Name)
	})

	// case 7: ModelServing with gang policy and PodGroups
	// This test verifies that when gang scheduling is enabled, PodGroups are created and updated
	// appropriately during the ModelServing lifecycle.
	t.Run("ModelServingGangPolicyPodGroups", func(t *testing.T) {
		// Create a ModelServing with gang policy enabled
		ms := createGangModelServing("test-gang-ms", 2, 2)

		// Add ModelServing to fake client
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{},
		)
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-gang-ms")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, ms, 2)

		// Wait until both PodGroups for this MI are created
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			pgList, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil {
				return false
			}
			pgForMI := 0
			for _, pg := range pgList.Items {
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name {
					pgForMI++
				}
			}
			return pgForMI == 2
		})
		assert.True(t, found, "two PodGroups should be created for two ServingGroups of this ModelServing")
		// Verify PodGroup spec fields
		pgList, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
			context.Background(), metav1.ListOptions{},
		)
		assert.NoError(t, err)
		for _, pg := range pgList.Items {
			if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] != ms.Name {
				continue
			}
			// Check MinMember equals per-group pod count for this MI
			expectedMinMember := int32(utils.ExpectedPodNum(ms))
			assert.Equal(t, expectedMinMember, pg.Spec.MinMember, "PodGroup MinMember should match expected per-servinggroup pod count")
		}

		t.Logf("Scaling up ModelServing replicas to trigger PodGroup updates")
		// Scale up ModelServing replicas to trigger PodGroup updates
		updatedMI := ms.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](3)

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{},
		)
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-gang-ms")
			return err == nil && *ms.Spec.Replicas == 3
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update - may need multiple syncs to create new PodGroup
		err = controller.syncModelServing(context.Background(), "default/test-gang-ms")
		assert.NoError(t, err)

		// Wait until three PodGroups for this MI are created after scale up
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			pgList, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil {
				return false
			}
			count := 0
			for _, pg := range pgList.Items {
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == updatedMI.Name {
					count++
				}
			}
			return count == 3
		})
		assert.True(t, found, "three PodGroups should exist after scaling up to three ServingGroups for this ModelServing")

		// Verify PodGroup spec fields after scale up
		pgListScaleUp, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
			context.Background(), metav1.ListOptions{},
		)
		assert.NoError(t, err)
		for _, pg := range pgListScaleUp.Items {
			if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] != updatedMI.Name {
				continue
			}
			// Check MinMember equals per-group pod count for this MI
			expectedMinMember := int32(utils.ExpectedPodNum(updatedMI))
			assert.Equal(t, expectedMinMember, pg.Spec.MinMember, "PodGroup MinMember should match expected per-servinggroup pod count for updated MI")
		}
	})

	// case 8: ModelServing gang policy disabled should cleanup PodGroups
	t.Run("ModelServingGangPolicyCleanupPodGroups", func(t *testing.T) {
		ms := createGangModelServing("test-gang-cleanup", 2, 1)

		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), ms, metav1.CreateOptions{},
		)
		assert.NoError(t, err)

		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-gang-cleanup")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Wait until both PodGroups for this MI are created
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			pgList, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil {
				return false
			}
			count := 0
			for _, pg := range pgList.Items {
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name {
					count++
				}
			}
			return count == 2
		})
		assert.True(t, found, "two PodGroups should exist for this ModelServing")
	})
}

// waitForObjectInCache waits for a specific object to appear in the cache
func waitForObjectInCache(t *testing.T, timeout time.Duration, checkFunc func() bool) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Logf("Object not found in cache after %v timeout", timeout)
			return false
		case <-ticker.C:
			if checkFunc() {
				return true
			}
		}
	}
}

// createStandardModelServing Create a standard ModelServing
func createStandardModelServing(name string, replicas int32, roleReplicas int32) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](replicas),
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](roleReplicas),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "prefill-container",
										Image: "test-image:latest",
									},
								},
							},
						},
					},
				},
			},
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
		},
	}
}

// createGangModelServing creates a ModelServing with gang policy
func createGangModelServing(name string, replicas int32, roleReplicas int32) *workloadv1alpha1.ModelServing {
	ms := createStandardModelServing(name, replicas, roleReplicas)
	ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{
		MinRoleReplicas: map[string]int32{
			"prefill": roleReplicas,
		},
	}
	return ms
}

// verifyServingGroups Verify the number and name of ServingGroup
func verifyServingGroups(t *testing.T, controller *ModelServingController, ms *workloadv1alpha1.ModelServing, expectedCount int) {
	groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	assert.NoError(t, err)
	assert.Equal(t, expectedCount, len(groups), fmt.Sprintf("Should have %d ServingGroups", expectedCount))

	// Verify that the ServingGroup name follows the expected pattern
	expectedGroupNames := make([]string, expectedCount)
	for i := 0; i < expectedCount; i++ {
		expectedGroupNames[i] = fmt.Sprintf("%s-%d", ms.Name, i)
	}

	actualGroupNames := make([]string, len(groups))
	for i, group := range groups {
		actualGroupNames[i] = group.Name
	}
	assert.Equal(t, expectedGroupNames, actualGroupNames, "ServingGroup names should follow expected pattern")
}

// verifyPodCount Verify the number of Pods in each ServingGroup
func verifyPodCount(t *testing.T, controller *ModelServingController, ms *workloadv1alpha1.ModelServing, expectedGroups int) {
	expectPodNum := utils.ExpectedPodNum(ms)
	for i := 0; i < expectedGroups; i++ {
		groupName := fmt.Sprintf("%s-%d", ms.Name, i)
		groupSelector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
		})

		groupPods, err := controller.podsLister.Pods(ms.Namespace).List(groupSelector)
		assert.NoError(t, err)
		assert.Equal(t, expectPodNum, len(groupPods), fmt.Sprintf("ServingGroup %s should have %d pods", groupName, expectPodNum))
	}
}

// verifyRoles Verify the number and name of Role
func verifyRoles(t *testing.T, controller *ModelServingController, ms *workloadv1alpha1.ModelServing, expectedGroups int) {
	// Traverse each ServingGroup
	servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	assert.NoError(t, err)
	for _, group := range servingGroups {
		groupName := group.Name

		// Traverse each role defined in the ModelServing spec
		for _, specRole := range ms.Spec.Template.Roles {
			roleName := specRole.Name
			expectedRoleReplicas := int(*specRole.Replicas)

			// Get all instances of the role from the store
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(ms), groupName, roleName)
			assert.NoError(t, err, fmt.Sprintf("Should be able to get role list for %s in group %s", roleName, groupName))

			// Verify the number of roles
			assert.Equal(t, expectedRoleReplicas, len(roles),
				fmt.Sprintf("Group %s should have %d replicas of role %s", groupName, expectedRoleReplicas, roleName))

			// Verify role ID naming conventions
			expectedRoleIDs := make([]string, expectedRoleReplicas)
			for j := 0; j < expectedRoleReplicas; j++ {
				expectedRoleIDs[j] = fmt.Sprintf("%s-%d", roleName, j)
			}

			actualRoleIDs := make([]string, len(roles))
			for j, role := range roles {
				actualRoleIDs[j] = role.Name
			}

			assert.ElementsMatch(t, expectedRoleIDs, actualRoleIDs,
				fmt.Sprintf("Role IDs in group %s for role %s should follow expected pattern", groupName, roleName))
		}
	}
}

// TestScaleUpServingGroups tests the scaleUpServingGroups function with various scenarios
func TestScaleUpServingGroups(t *testing.T) {
	tests := []struct {
		name               string
		existingIndices    []int // Indices of existing ServingGroups
		expectedCount      int   // Target count for scale up
		expectedNewIndices []int // Expected indices for newly created groups
		expectNoCreation   bool  // Whether no new groups should be created
	}{
		{
			name:               "scale up from 0 to 2 groups",
			existingIndices:    []int{},
			expectedCount:      2,
			expectedNewIndices: []int{0, 1},
			expectNoCreation:   false,
		},
		{
			name:               "scale up from 1 to 3 groups with continuous indices",
			existingIndices:    []int{0},
			expectedCount:      3,
			expectedNewIndices: []int{1, 2},
			expectNoCreation:   false,
		},
		{
			name:               "scale up with gap in indices - should use increasing indices from max",
			existingIndices:    []int{0, 5}, // Gap: indices 1-4 missing
			expectedCount:      4,
			expectedNewIndices: []int{6, 7}, // Should continue from max index (5) + 1
			expectNoCreation:   false,
		},
		{
			name:               "scale up with only high index existing",
			existingIndices:    []int{10},
			expectedCount:      3,
			expectedNewIndices: []int{11, 12}, // Should continue from max index (10) + 1
			expectNoCreation:   false,
		},
		{
			name:               "no scale up needed - validCount equals expectedCount",
			existingIndices:    []int{0, 1},
			expectedCount:      2,
			expectedNewIndices: []int{},
			expectNoCreation:   true,
		},
		{
			name:               "no scale up needed - validCount exceeds expectedCount",
			existingIndices:    []int{0, 1, 2},
			expectedCount:      2,
			expectedNewIndices: []int{},
			expectNoCreation:   true,
		},
		{
			name:               "scale up from single group",
			existingIndices:    []int{0},
			expectedCount:      5,
			expectedNewIndices: []int{1, 2, 3, 4},
			expectNoCreation:   false,
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fresh fake clients for each test to ensure isolation
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset()

			// Create controller without running it to avoid background sync interference
			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
			assert.NoError(t, err)

			// Create a unique ModelServing for this test
			miName := fmt.Sprintf("test-scaleup-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](int32(tt.expectedCount)),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			// Pre-populate the store with existing ServingGroups
			for _, ordinal := range tt.existingIndices {
				controller.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, "test-revision")
			}

			// Build the servingGroupList to pass to scaleUpServingGroups
			existingGroups := make([]datastore.ServingGroup, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingGroups[i] = datastore.ServingGroup{
					Name: utils.GenerateServingGroupName(miName, ordinal),
				}
			}

			// Call scaleUpServingGroups directly (not through syncModelServing)
			err = controller.scaleUpServingGroups(context.Background(), ms, existingGroups, tt.expectedCount, "new-revision")
			assert.NoError(t, err)

			// Verify the results
			groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			assert.NoError(t, err)

			if tt.expectNoCreation {
				// Verify no new groups were created
				assert.Equal(t, len(tt.existingIndices), len(groups), "No new groups should be created")
			} else {
				// Verify new indices are as expected
				for _, expectedIdx := range tt.expectedNewIndices {
					expectedName := utils.GenerateServingGroupName(miName, expectedIdx)
					found := false
					for _, g := range groups {
						if g.Name == expectedName {
							found = true
							break
						}
					}
					assert.True(t, found, "Expected group %s to be created", expectedName)
				}

				// Verify total groups count
				expectedTotal := len(tt.existingIndices) + len(tt.expectedNewIndices)
				assert.Equal(t, expectedTotal, len(groups), "Total group count should match expected")
			}
		})
	}
}

// TestScaleUpRoles tests the scaleUpRoles function with various scenarios
func TestScaleUpRoles(t *testing.T) {
	tests := []struct {
		name string

		existingIndices    []int // Indices of existing Roles
		expectedCount      int   // Target count for scale up
		expectedNewIndices []int // Expected indices for newly created roles

		expectNoCreation bool // Whether no new roles should be created
	}{
		{
			name:               "scale up from 0 to 2 roles",
			existingIndices:    []int{},
			expectedCount:      2,
			expectedNewIndices: []int{0, 1},
			expectNoCreation:   false,
		},
		{
			name:               "scale up from 1 to 3 roles with continuous indices",
			existingIndices:    []int{0},
			expectedCount:      3,
			expectedNewIndices: []int{1, 2},
			expectNoCreation:   false,
		},
		{
			name:               "scale up with gap in indices - should use increasing indices from max",
			existingIndices:    []int{0, 5}, // Gap: indices 1-4 missing
			expectedCount:      4,
			expectedNewIndices: []int{6, 7}, // Should continue from max index (5) + 1
			expectNoCreation:   false,
		},
		{
			name:               "scale up with only high index existing",
			existingIndices:    []int{10},
			expectedCount:      3,
			expectedNewIndices: []int{11, 12}, // Should continue from max index (10) + 1
			expectNoCreation:   false,
		},
		{
			name:               "no scale up needed - validCount equals expectedCount",
			existingIndices:    []int{0, 1},
			expectedCount:      2,
			expectedNewIndices: []int{},
			expectNoCreation:   true,
		},
		{
			name:               "no scale up needed - validCount exceeds expectedCount",
			existingIndices:    []int{0, 1, 2},
			expectedCount:      2,
			expectedNewIndices: []int{},
			expectNoCreation:   true,
		},
		{
			name:               "scale up from single role",
			existingIndices:    []int{0},
			expectedCount:      5,
			expectedNewIndices: []int{1, 2, 3, 4},
			expectNoCreation:   false,
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fresh fake clients for each test to ensure isolation
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset()

			// Create controller without running it to avoid background sync interference
			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
			assert.NoError(t, err)

			// Create a unique ModelServing for this test
			miName := fmt.Sprintf("test-scaleup-roles-%d", idx)
			roleName := "prefill"
			groupName := utils.GenerateServingGroupName(miName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](1),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     roleName,
								Replicas: ptr.To[int32](int32(tt.expectedCount)),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			// Pre-populate the store with ServingGroup and Roles
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			for _, ordinal := range tt.existingIndices {
				controller.store.AddRole(utils.GetNamespaceName(ms), groupName, "prefill", utils.GenerateRoleID("prefill", ordinal), "test-revision")
			}

			// Build the roleList to pass to scaleUpRoles
			existingRoles := make([]datastore.Role, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingRoles[i] = datastore.Role{
					Name: utils.GenerateRoleID("prefill", ordinal),
				}
			}

			targetRole := ms.Spec.Template.Roles[0]

			// Call scaleUpRoles directly
			controller.scaleUpRoles(context.Background(), ms, groupName, targetRole, existingRoles, tt.expectedCount, 0, "new-revision")

			// Verify the results
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(ms), groupName, "prefill")
			assert.NoError(t, err)

			if tt.expectNoCreation {
				// Verify no new roles were created
				assert.Equal(t, len(tt.existingIndices), len(roles), "No new roles should be created")
			} else {
				// Verify new indices are as expected
				for _, expectedIdx := range tt.expectedNewIndices {
					expectedName := utils.GenerateRoleID("prefill", expectedIdx)
					found := false
					for _, r := range roles {
						if r.Name == expectedName {
							found = true
							break
						}
					}
					assert.True(t, found, "Expected role %s to be created", expectedName)
				}

				// Verify total roles count
				expectedTotal := len(tt.existingIndices) + len(tt.expectedNewIndices)
				assert.Equal(t, expectedTotal, len(roles), "Total role count should match expected")
			}
		})
	}
}

// TestScaleDownServingGroups tests the scaleDownServingGroups function with various scenarios
func TestScaleDownServingGroups(t *testing.T) {
	tests := []struct {
		name                   string
		existingIndices        []int    // Indices of existing ServingGroups
		expectedCount          int      // Target count after scale down
		expectedRemainingNames []string // Expected remaining ServingGroup names (without test prefix)
	}{
		{
			name:                   "scale down from 4 to 2 - delete highest indices",
			existingIndices:        []int{0, 1, 2, 3},
			expectedCount:          2,
			expectedRemainingNames: []string{"0", "1"}, // Higher indices deleted first
		},
		{
			name:                   "scale down from 3 to 1",
			existingIndices:        []int{0, 1, 2},
			expectedCount:          1,
			expectedRemainingNames: []string{"0"},
		},
		{
			name:                   "scale down from 5 to 3",
			existingIndices:        []int{0, 1, 2, 3, 4},
			expectedCount:          3,
			expectedRemainingNames: []string{"0", "1", "2"},
		},
		{
			name:                   "no scale down needed - equal count",
			existingIndices:        []int{0, 1},
			expectedCount:          2,
			expectedRemainingNames: []string{"0", "1"},
		},
		{
			name:                   "scale down with non-continuous indices",
			existingIndices:        []int{0, 2, 5, 8},
			expectedCount:          2,
			expectedRemainingNames: []string{"0", "2"}, // Higher indices (5, 8) deleted first
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-scaledown-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](int32(tt.expectedCount)),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			// Pre-populate the store with existing ServingGroups
			for _, ordinal := range tt.existingIndices {
				controller.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, "test-revision")
			}

			// Build the servingGroupList to pass to scaleDownServingGroups
			existingGroups := make([]datastore.ServingGroup, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingGroups[i] = datastore.ServingGroup{
					Name: utils.GenerateServingGroupName(miName, ordinal),
				}
			}

			// Call scaleDownServingGroups directly (no binpack - all scores are 0)
			err = controller.scaleDownServingGroups(context.Background(), ms, existingGroups, tt.expectedCount)
			assert.NoError(t, err)

			// Verify the results
			groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			assert.NoError(t, err)

			// Verify remaining group count
			assert.Equal(t, tt.expectedCount, len(groups), "Remaining group count should match expected")

			// Verify remaining group names
			actualNames := make([]string, len(groups))
			for i, g := range groups {
				// Extract just the index suffix from the name
				_, idx := utils.GetParentNameAndOrdinal(g.Name)
				actualNames[i] = fmt.Sprintf("%d", idx)
			}
			assert.ElementsMatch(t, tt.expectedRemainingNames, actualNames, "Remaining group indices should match expected")
		})
	}
}

// TestScaleDownServingGroupsWithPriorityAndDeletionCost tests the scaleDownServingGroups function with priority and deletion cost scenarios
func TestScaleDownServingGroupsWithPriorityAndDeletionCost(t *testing.T) {
	tests := []struct {
		name                   string
		existingIndices        []int
		expectedCount          int
		groupStatuses          map[int]datastore.ServingGroupStatus // Index -> Status
		podDeletionCosts       map[int]int                          // Index -> DeletionCost
		expectedRemainingNames []string
		description            string
	}{
		{
			name:            "not_ready_groups_deleted_first",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupCreating, // Not ready - should be deleted first
				3: datastore.ServingGroupRunning,
			},
			podDeletionCosts:       map[int]int{},
			expectedRemainingNames: []string{"0", "1"}, // Group 2 (not ready) and highest ready index (3) deleted
			description:            "Groups in non-running state should be deleted first regardless of index",
		},
		{
			name:            "lower_deletion_cost_deleted_first_among_ready",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100, // High cost - protected
				1: 50,  // Medium cost
				2: 0,   // Low cost - delete first
				3: 75,  // Medium-high cost
			},
			expectedRemainingNames: []string{"0", "3"}, // Groups 2 (cost 0) and 1 (cost 50) deleted, keeping 0 and 3
			description:            "Among ready groups, lower deletion cost should be deleted first",
		},
		{
			name:            "not_ready_status_has_priority_over_deletion_cost",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupCreating, // Not ready - deleted first despite high cost
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			podDeletionCosts: map[int]int{
				0: 10,
				1: 1000, // Very high cost but not ready - still deleted
				2: 20,
				3: 30,
			},
			expectedRemainingNames: []string{"2", "3"}, // Group 1 (not ready) and Group 0 (lowest cost among ready) deleted
			description:            "Not-ready status should take priority over deletion cost",
		},
		{
			name:            "mixed_status_and_deletion_cost",
			existingIndices: []int{0, 1, 2, 3, 4},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupScaling, // Not ready
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupDeleting, // Not ready
				4: datastore.ServingGroupRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100,
				1: 0,
				2: 50,
				3: 0,
				4: 200, // Highest cost among ready groups
			},
			expectedRemainingNames: []string{"0", "4"}, // Groups 1,3 (not ready) and 2 (lowest cost among ready) deleted
			description:            "Complex scenario with mixed status and costs",
		},
		{
			name:            "all_groups_not_ready_delete_by_index",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupCreating,
				1: datastore.ServingGroupScaling,
				2: datastore.ServingGroupCreating,
				3: datastore.ServingGroupScaling,
			},
			podDeletionCosts:       map[int]int{},
			expectedRemainingNames: []string{"0", "1"}, // All not ready, delete by index
			description:            "When all groups are not ready, fall back to index-based deletion",
		},
		{
			name:            "same_deletion_cost_use_index_as_tiebreaker",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			podDeletionCosts: map[int]int{
				0: 50,
				1: 50, // Same cost as 0
				2: 50,
				3: 50,
			},
			expectedRemainingNames: []string{"0", "1"}, // All same cost, delete by index
			description:            "When deletion costs are equal, use index as tiebreaker",
		},
		{
			name:            "negative_deletion_cost_prioritized_for_deletion",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100,
				1: -100, // Negative cost - high deletion priority
				2: 50,
				3: 75,
			},
			expectedRemainingNames: []string{"0", "3"}, // Group 1 (negative cost) and 2 (low positive cost) deleted
			description:            "Negative deletion cost should prioritize deletion",
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-priority-scaledown-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](int32(tt.expectedCount)),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			podIndexer := controller.podsInformer.GetIndexer()

			// Pre-populate the store with existing ServingGroups and set their statuses
			for _, ordinal := range tt.existingIndices {
				groupName := utils.GenerateServingGroupName(miName, ordinal)
				controller.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, "test-revision")
				if status, exists := tt.groupStatuses[ordinal]; exists {
					controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, status)
				}

				// Create a mock pod for each group with deletion cost annotation
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ms.Namespace,
						Name:      fmt.Sprintf("pod-%s", groupName),
						Labels: map[string]string{
							workloadv1alpha1.ModelServingNameLabelKey: miName,
							workloadv1alpha1.GroupNameLabelKey:        groupName,
							workloadv1alpha1.RoleLabelKey:             "prefill",
							workloadv1alpha1.RoleIDKey:                "prefill-0",
						},
					},
				}

				// Add deletion cost annotation if specified
				if cost, exists := tt.podDeletionCosts[ordinal]; exists {
					if pod.Annotations == nil {
						pod.Annotations = make(map[string]string)
					}
					pod.Annotations[PodDeletionCostAnnotation] = fmt.Sprintf("%d", cost)
				}

				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Build the servingGroupList to pass to scaleDownServingGroups
			existingGroups := make([]datastore.ServingGroup, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingGroups[i] = datastore.ServingGroup{
					Name: utils.GenerateServingGroupName(miName, ordinal),
				}
			}

			// Call scaleDownServingGroups with priority and deletion cost
			err = controller.scaleDownServingGroups(context.Background(), ms, existingGroups, tt.expectedCount)
			assert.NoError(t, err)

			// Manually delete ServingGroups that are marked as Deleting from the store
			// This simulates the deletion process that would happen in the real controller
			for _, ordinal := range tt.existingIndices {
				groupName := utils.GenerateServingGroupName(miName, ordinal)
				status := controller.store.GetServingGroupStatus(utils.GetNamespaceName(ms), groupName)
				if status == datastore.ServingGroupDeleting {
					// Simulate pods and services being deleted
					selector := labels.SelectorFromSet(map[string]string{
						workloadv1alpha1.GroupNameLabelKey: groupName,
					})
					pods, _ := controller.podsLister.Pods(ms.Namespace).List(selector)
					for _, pod := range pods {
						podIndexer.Delete(pod)
					}

					// Check if ServingGroup is fully deleted and remove from store
					if controller.isServingGroupDeleted(ms, groupName) {
						controller.store.DeleteServingGroup(utils.GetNamespaceName(ms), groupName)
					}
				}
			}

			// Verify the results
			groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			assert.NoError(t, err)

			// Verify remaining group count
			assert.Equal(t, tt.expectedCount, len(groups),
				fmt.Sprintf("[%s] Remaining group count should match expected", tt.description))

			// Verify remaining group names
			actualNames := make([]string, len(groups))
			for i, g := range groups {
				_, idx := utils.GetParentNameAndOrdinal(g.Name)
				actualNames[i] = fmt.Sprintf("%d", idx)
			}
			assert.ElementsMatch(t, tt.expectedRemainingNames, actualNames,
				fmt.Sprintf("[%s] Remaining group indices should match expected. Got: %v, Want: %v",
					tt.description, actualNames, tt.expectedRemainingNames))
		})
	}
}

// TestScaleDownRoles tests the scaleDownRoles function with various scenarios
func TestScaleDownRoles(t *testing.T) {
	tests := []struct {
		name                   string
		existingIndices        []int    // Indices of existing Roles
		expectedCount          int      // Target count after scale down
		expectedRemainingNames []string // Expected remaining Role names (without test prefix)
	}{
		{
			name:                   "scale down from 4 to 2 - delete highest indices",
			existingIndices:        []int{0, 1, 2, 3},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-1"}, // Higher indices deleted first
		},
		{
			name:                   "scale down from 3 to 1",
			existingIndices:        []int{0, 1, 2},
			expectedCount:          1,
			expectedRemainingNames: []string{"prefill-0"},
		},
		{
			name:                   "scale down from 5 to 3",
			existingIndices:        []int{0, 1, 2, 3, 4},
			expectedCount:          3,
			expectedRemainingNames: []string{"prefill-0", "prefill-1", "prefill-2"},
		},
		{
			name:                   "no scale down needed - equal count",
			existingIndices:        []int{0, 1},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-1"},
		},
		{
			name:                   "scale down with non-continuous indices",
			existingIndices:        []int{0, 2, 5, 8},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-2"}, // Higher indices (5, 8) deleted first
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-role-scaledown-%d", idx)
			groupName := utils.GenerateServingGroupName(miName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](1),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](int32(tt.expectedCount)),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			targetRole := ms.Spec.Template.Roles[0]

			// Pre-populate the store with ServingGroup and Roles
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			for _, ordinal := range tt.existingIndices {
				controller.store.AddRole(utils.GetNamespaceName(ms), groupName, "prefill", utils.GenerateRoleID("prefill", ordinal), "test-revision")
			}

			// Build the roleList to pass to scaleDownRoles
			existingRoles := make([]datastore.Role, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingRoles[i] = datastore.Role{
					Name: utils.GenerateRoleID("prefill", ordinal),
				}
			}

			// Call scaleDownRoles directly (no binpack - all scores are 0)
			controller.scaleDownRoles(context.Background(), ms, groupName, targetRole, existingRoles, tt.expectedCount)

			// Verify the results
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(ms), groupName, "prefill")
			assert.NoError(t, err)

			// Verify remaining role count
			assert.Equal(t, tt.expectedCount, len(roles), "Remaining role count should match expected")

			// Verify remaining role names
			actualNames := make([]string, len(roles))
			for i, r := range roles {
				actualNames[i] = r.Name
			}
			assert.ElementsMatch(t, tt.expectedRemainingNames, actualNames, "Remaining role names should match expected")
		})
	}
}

// TestScaleDownRolesWithPriorityAndDeletionCost tests the scaleDownRoles function with priority and deletion cost scenarios
func TestScaleDownRolesWithPriorityAndDeletionCost(t *testing.T) {
	tests := []struct {
		name                   string
		existingIndices        []int
		expectedCount          int
		roleStatuses           map[int]datastore.RoleStatus // Index -> Status
		podDeletionCosts       map[int]int                  // Index -> DeletionCost
		expectedRemainingNames []string
		description            string
	}{
		{
			name:            "not_ready_roles_deleted_first",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleCreating, // Not ready - should be deleted first
				3: datastore.RoleRunning,
			},
			podDeletionCosts:       map[int]int{},
			expectedRemainingNames: []string{"prefill-0", "prefill-1"}, // Role 2 (not ready) and highest ready index (3) deleted
			description:            "Roles in non-running state should be deleted first regardless of index",
		},
		{
			name:            "lower_deletion_cost_deleted_first_among_ready",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100, // High cost - protected
				1: 50,  // Medium cost
				2: 0,   // Low cost - delete first
				3: 75,  // Medium-high cost
			},
			expectedRemainingNames: []string{"prefill-0", "prefill-3"}, // Roles 2 (cost 0) and 1 (cost 50) deleted, keeping 0 and 3
			description:            "Among ready roles, lower deletion cost should be deleted first",
		},
		{
			name:            "not_ready_status_has_priority_over_deletion_cost",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleCreating, // Not ready - deleted first despite high cost
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 10,
				1: 1000, // Very high cost but not ready - still deleted
				2: 20,
				3: 30,
			},
			expectedRemainingNames: []string{"prefill-2", "prefill-3"}, // Role 1 (not ready) and Role 0 (lowest cost among ready) deleted
			description:            "Not-ready status should take priority over deletion cost",
		},
		{
			name:            "mixed_status_and_deletion_cost",
			existingIndices: []int{0, 1, 2, 3, 4},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleNotFound, // Not ready
				2: datastore.RoleRunning,
				3: datastore.RoleDeleting, // Not ready
				4: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100,
				1: 0,
				2: 50,
				3: 0,
				4: 200, // Highest cost among ready roles
			},
			expectedRemainingNames: []string{"prefill-0", "prefill-4"}, // Roles 1,3 (not ready) and 2 (lowest cost among ready) deleted
			description:            "Complex scenario with mixed status and costs",
		},
		{
			name:            "all_roles_not_ready_delete_by_index",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleCreating,
				1: datastore.RoleCreating,
				2: datastore.RoleNotFound,
				3: datastore.RoleCreating,
			},
			podDeletionCosts:       map[int]int{},
			expectedRemainingNames: []string{"prefill-0", "prefill-1"}, // All not ready, delete by index
			description:            "When all roles are not ready, fall back to index-based deletion",
		},
		{
			name:            "same_deletion_cost_use_index_as_tiebreaker",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 50,
				1: 50, // Same cost as 0
				2: 50,
				3: 50,
			},
			expectedRemainingNames: []string{"prefill-0", "prefill-1"}, // All same cost, delete by index
			description:            "When deletion costs are equal, use index as tiebreaker",
		},
		{
			name:            "negative_deletion_cost_prioritized_for_deletion",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100,
				1: -100, // Negative cost - high deletion priority
				2: 50,
				3: 75,
			},
			expectedRemainingNames: []string{"prefill-0", "prefill-3"}, // Role 1 (negative cost) and 2 (low positive cost) deleted
			description:            "Negative deletion cost should prioritize deletion",
		},
		{
			name:            "partial_deletion_costs_use_index_for_unspecified",
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			podDeletionCosts: map[int]int{
				0: 100, // High cost
				2: 50,  // Medium cost
				// Roles 1 and 3 have no explicit cost (default to 0)
			},
			expectedRemainingNames: []string{"prefill-0", "prefill-2"}, // Roles 1 and 3 (default cost 0) deleted, keeping 0 and 2
			description:            "Roles without explicit deletion cost should default to 0",
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-role-priority-scaledown-%d", idx)
			groupName := utils.GenerateServingGroupName(miName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      miName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](1),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](int32(tt.expectedCount)),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "prefill-container",
												Image: "test-image:latest",
											},
										},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			targetRole := ms.Spec.Template.Roles[0]
			podIndexer := controller.podsInformer.GetIndexer()

			// Pre-populate the store with ServingGroup and Roles
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			for _, ordinal := range tt.existingIndices {
				roleID := utils.GenerateRoleID("prefill", ordinal)
				controller.store.AddRole(utils.GetNamespaceName(ms), groupName, "prefill", roleID, "test-revision")
				if status, exists := tt.roleStatuses[ordinal]; exists {
					controller.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, "prefill", roleID, status)
				}

				// Create a mock pod for each role with deletion cost annotation
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ms.Namespace,
						Name:      fmt.Sprintf("pod-%s", roleID),
						Labels: map[string]string{
							workloadv1alpha1.ModelServingNameLabelKey: miName,
							workloadv1alpha1.GroupNameLabelKey:        groupName,
							workloadv1alpha1.RoleLabelKey:             "prefill",
							workloadv1alpha1.RoleIDKey:                roleID,
						},
					},
				}

				// Add deletion cost annotation if specified
				if cost, exists := tt.podDeletionCosts[ordinal]; exists {
					if pod.Annotations == nil {
						pod.Annotations = make(map[string]string)
					}
					pod.Annotations[PodDeletionCostAnnotation] = fmt.Sprintf("%d", cost)
				}

				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Build the roleList to pass to scaleDownRoles
			existingRoles := make([]datastore.Role, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingRoles[i] = datastore.Role{
					Name: utils.GenerateRoleID("prefill", ordinal),
				}
			}

			// Call scaleDownRoles with priority and deletion cost
			controller.scaleDownRoles(context.Background(), ms, groupName, targetRole, existingRoles, tt.expectedCount)

			// Manually delete Roles that are marked as Deleting from the store
			// This simulates the deletion process that would happen in the real controller
			for _, ordinal := range tt.existingIndices {
				roleID := utils.GenerateRoleID("prefill", ordinal)
				status := controller.store.GetRoleStatus(utils.GetNamespaceName(ms), groupName, "prefill", roleID)
				if status == datastore.RoleDeleting {
					// Simulate pods and services being deleted
					selector := labels.SelectorFromSet(map[string]string{
						workloadv1alpha1.GroupNameLabelKey: groupName,
						workloadv1alpha1.RoleLabelKey:      "prefill",
						workloadv1alpha1.RoleIDKey:         roleID,
					})
					pods, _ := controller.podsLister.Pods(ms.Namespace).List(selector)
					for _, pod := range pods {
						podIndexer.Delete(pod)
					}

					// Check if Role is fully deleted and remove from store
					if controller.isRoleDeleted(ms, groupName, "prefill", roleID) {
						controller.store.DeleteRole(utils.GetNamespaceName(ms), groupName, "prefill", roleID)
					}
				}
			}

			// Verify the results
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(ms), groupName, "prefill")
			assert.NoError(t, err)

			// Verify remaining role count
			assert.Equal(t, tt.expectedCount, len(roles),
				fmt.Sprintf("[%s] Remaining role count should match expected", tt.description))

			// Verify remaining role names
			actualNames := make([]string, len(roles))
			for i, r := range roles {
				actualNames[i] = r.Name
			}
			assert.ElementsMatch(t, tt.expectedRemainingNames, actualNames,
				fmt.Sprintf("[%s] Remaining role names should match expected. Got: %v, Want: %v",
					tt.description, actualNames, tt.expectedRemainingNames))
		})
	}
}

// TestCalculateRoleScore tests the priority-based scoring for role scale-down
func TestCalculateRoleScore(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()

	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
	assert.NoError(t, err)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-scoring",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](1),
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "prefill-container",
										Image: "test-image:latest",
									},
								},
							},
						},
					},
				},
			},
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
		},
	}

	groupName := utils.GenerateServingGroupName(ms.Name, 0)

	tests := []struct {
		name             string
		roleStatus       datastore.RoleStatus
		podDeletionCost  int
		expectedPriority int
		description      string
	}{
		{
			name:             "creating_status",
			roleStatus:       datastore.RoleCreating,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "Creating status should not be ready",
		},
		{
			name:             "notfound_status",
			roleStatus:       datastore.RoleNotFound,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "NotFound status should not be ready",
		},
		{
			name:             "running_status",
			roleStatus:       datastore.RoleRunning,
			podDeletionCost:  0,
			expectedPriority: 1, // PriorityHealthy
			description:      "Running status should be ready",
		},
		{
			name:             "deleting_status",
			roleStatus:       datastore.RoleDeleting,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "Deleting status should not be ready",
		},
		{
			name:             "positive_deletion_cost",
			roleStatus:       datastore.RoleCreating,
			podDeletionCost:  50,
			expectedPriority: 0, // PriorityUnhealthy // Creating status is not ready
			description:      "Positive deletion cost with Creating status",
		},
		{
			name:             "large_deletion_cost",
			roleStatus:       datastore.RoleRunning,
			podDeletionCost:  500, // No longer capped
			expectedPriority: 1,   // PriorityReady // Running status is ready
			description:      "Large deletion cost with Running status",
		},
		{
			name:             "negative_deletion_cost",
			roleStatus:       datastore.RoleCreating,
			podDeletionCost:  -500, // No longer capped
			expectedPriority: 0,    // PriorityNotReady // Creating status is not ready
			description:      "Negative deletion cost with Creating status",
		},
		{
			name:             "extra_positive_deletion_cost",
			roleStatus:       datastore.RoleRunning,
			podDeletionCost:  99, //
			expectedPriority: 1,  // PriorityReady // Running status is ready
			description:      "Positive deletion cost with Running status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset store for each test
			controller.store = datastore.New()

			// Pre-populate the store with ServingGroup and Role
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			controller.store.AddRole(utils.GetNamespaceName(ms), groupName, "prefill", "prefill-0", "test-revision")
			controller.store.UpdateRoleStatus(utils.GetNamespaceName(ms), groupName, "prefill", "prefill-0", tt.roleStatus)

			// Create a mock pod with deletion cost
			podIndexer := controller.podsInformer.GetIndexer()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "pod-prefill-0",
					Labels: map[string]string{
						workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
						workloadv1alpha1.GroupNameLabelKey:        groupName,
						workloadv1alpha1.RoleLabelKey:             "prefill",
						workloadv1alpha1.RoleIDKey:                "prefill-0",
						workloadv1alpha1.EntryLabelKey:            utils.Entry,
					},
					Annotations: map[string]string{
						PodDeletionCostAnnotation: fmt.Sprintf("%d", tt.podDeletionCost),
					},
				},
			}
			err := podIndexer.Add(pod)
			assert.NoError(t, err)

			// Calculate score
			score := controller.calculateRoleScore(ms, groupName, "prefill", "prefill-0")

			// Verify the Priority field
			assert.Equal(t, tt.expectedPriority, score.Priority,
				fmt.Sprintf("%s: expected Priority %d, got %d", tt.description, tt.expectedPriority, score.Priority))

			// Verify the deletion cost matches expected
			assert.Equal(t, tt.podDeletionCost, score.DeletionCost,
				fmt.Sprintf("%s: expected deletion cost %d, got %d", tt.description, tt.podDeletionCost, score.DeletionCost))
		})
	}
}

// TestCalculateServingGroupScore tests the priority-based scoring for serving group scale-down
func TestCalculateServingGroupScore(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()

	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
	assert.NoError(t, err)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-scoring",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](1),
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "prefill-container",
										Image: "test-image:latest",
									},
								},
							},
						},
					},
				},
			},
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
		},
	}

	tests := []struct {
		name             string
		groupStatus      datastore.ServingGroupStatus
		podDeletionCost  int
		expectedPriority int
		description      string
	}{
		{
			name:             "creating_status",
			groupStatus:      datastore.ServingGroupCreating,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "Creating status should not be ready",
		},
		{
			name:             "scaling_status",
			groupStatus:      datastore.ServingGroupScaling,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "Scaling status should not be ready",
		},
		{
			name:             "notfound_status",
			groupStatus:      datastore.ServingGroupNotFound,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "NotFound status should not be ready",
		},
		{
			name:             "running_status",
			groupStatus:      datastore.ServingGroupRunning,
			podDeletionCost:  0,
			expectedPriority: 1, // PriorityHealthy
			description:      "Running status should be ready",
		},
		{
			name:             "deleting_status",
			groupStatus:      datastore.ServingGroupDeleting,
			podDeletionCost:  0,
			expectedPriority: 0, // PriorityUnhealthy
			description:      "Deleting status should not be ready",
		},
		{
			name:             "positive_deletion_cost",
			groupStatus:      datastore.ServingGroupCreating,
			podDeletionCost:  50,
			expectedPriority: 0, // PriorityUnhealthy // Creating status is not ready
			description:      "Positive deletion cost with Creating status",
		},
		{
			name:             "large_deletion_cost",
			groupStatus:      datastore.ServingGroupRunning,
			podDeletionCost:  500, // No longer capped
			expectedPriority: 1,   // PriorityReady // Running status is ready
			description:      "Large deletion cost with Running status",
		},
		{
			name:             "negative_deletion_cost",
			groupStatus:      datastore.ServingGroupCreating,
			podDeletionCost:  -500, // No longer capped
			expectedPriority: 0,    // PriorityNotReady // Creating status is not ready
			description:      "Negative deletion cost with Creating status",
		},
		{
			name:             "extra_positive_deletion_cost",
			groupStatus:      datastore.ServingGroupRunning,
			podDeletionCost:  99, //
			expectedPriority: 1,  // PriorityReady // Running status is ready
			description:      "Positive deletion cost with Running status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset store for each test
			controller.store = datastore.New()

			// Pre-populate the store with ServingGroup
			groupName := utils.GenerateServingGroupName(ms.Name, 0)
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, "test-revision")
			controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, tt.groupStatus)

			// Create a mock pod with deletion cost
			podIndexer := controller.podsInformer.GetIndexer()
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "pod-test",
					Labels: map[string]string{
						workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
						workloadv1alpha1.GroupNameLabelKey:        groupName,
						workloadv1alpha1.RoleLabelKey:             "prefill",
						workloadv1alpha1.RoleIDKey:                "prefill-0",
						workloadv1alpha1.EntryLabelKey:            utils.Entry,
					},
					Annotations: map[string]string{
						PodDeletionCostAnnotation: fmt.Sprintf("%d", tt.podDeletionCost),
					},
				},
			}
			err := podIndexer.Add(pod)
			assert.NoError(t, err)

			// Calculate score
			score := controller.calculateServingGroupScore(ms, groupName)

			// Verify the Priority field
			assert.Equal(t, tt.expectedPriority, score.Priority,
				fmt.Sprintf("%s: expected Priority %d, got %d", tt.description, tt.expectedPriority, score.Priority))

			// Verify the deletion cost matches expected
			assert.Equal(t, tt.podDeletionCost, score.DeletionCost,
				fmt.Sprintf("%s: expected deletion cost %d, got %d", tt.description, tt.podDeletionCost, score.DeletionCost))
		})
	}
}

// TestCheckRoleReady tests the role readiness check functionality
func TestCheckRoleReady(t *testing.T) {
	// Create a test ModelServing with a role that has 1 replica and 2 worker replicas
	// Expected pods per role replica = 1 entry + 2 workers = 3 pods per replica
	// Total expected = 3 * 1 = 3 pods
	workerReplicas := int32(2)
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model-serving",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:           "prefill",
						WorkerReplicas: workerReplicas,
						Replicas:       ptr.To[int32](1),
					},
				},
			},
		},
	}

	tests := []struct {
		name          string
		roleName      string
		roleID        string
		podCount      int
		podPhase      corev1.PodPhase
		podReady      bool
		expectedReady bool
		description   string
	}{
		{
			name:          "all_pods_running_and_ready",
			roleName:      "prefill",
			roleID:        "prefill-0",
			podCount:      3, // 1 entry + 2 workers
			podPhase:      corev1.PodRunning,
			podReady:      true,
			expectedReady: true,
			description:   "Role should be ready when all expected pods are running and ready",
		},
		{
			name:          "not_all_pods_ready",
			roleName:      "prefill",
			roleID:        "prefill-0",
			podCount:      2, // Missing 1 pod
			podPhase:      corev1.PodRunning,
			podReady:      true,
			expectedReady: false,
			description:   "Role should not be ready when not all expected pods are running",
		},
		{
			name:          "pods_running_but_not_ready",
			roleName:      "prefill",
			roleID:        "prefill-0",
			podCount:      3,
			podPhase:      corev1.PodRunning,
			podReady:      false,
			expectedReady: false,
			description:   "Role should not be ready when pods are running but not ready",
		},
		{
			name:          "pods_pending",
			roleName:      "prefill",
			roleID:        "prefill-0",
			podCount:      3,
			podPhase:      corev1.PodPending,
			podReady:      false,
			expectedReady: false,
			description:   "Role should not be ready when pods are pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create fake clients
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
			assert.NoError(t, err)

			groupName := utils.GenerateServingGroupName(ms.Name, 0)

			// Create pods for the role
			podIndexer := controller.podsInformer.GetIndexer()
			for i := 0; i < tt.podCount; i++ {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ms.Namespace,
						Name:      fmt.Sprintf("%s-%s-%d", tt.roleID, tt.roleName, i),
						Labels: map[string]string{
							workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
							workloadv1alpha1.GroupNameLabelKey:        groupName,
							workloadv1alpha1.RoleLabelKey:             tt.roleName,
							workloadv1alpha1.RoleIDKey:                tt.roleID,
						},
					},
				}

				// Set pod phase and ready condition
				pod.Status.Phase = tt.podPhase
				if tt.podReady {
					pod.Status.Conditions = []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					}
				}

				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Check role readiness
			ready, err := controller.checkRoleReady(ms, groupName, tt.roleName, tt.roleID)

			// Verify results
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedReady, ready,
				fmt.Sprintf("%s: expected ready=%v, got ready=%v", tt.description, tt.expectedReady, ready))
		})
	}
}

func TestManageHeadlessService(t *testing.T) {
	tests := []struct {
		name                 string
		modelServing         *workloadv1alpha1.ModelServing
		existingRoles        []datastore.Role
		existingServices     []*corev1.Service
		expectedServiceCount int
		expectServiceCreated bool
	}{
		{
			name: "create headless service when none exists",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "prefill",
								Replicas:       ptr.To[int32](2),
								WorkerReplicas: 2,
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "container",
												Image: "nginx",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			existingRoles: []datastore.Role{
				{Name: "prefill-0", Status: datastore.RoleRunning, Revision: "v1"},
				{Name: "prefill-1", Status: datastore.RoleRunning, Revision: "v1"},
			},
			existingServices:     []*corev1.Service{},
			expectedServiceCount: 2, // One for each role
			expectServiceCreated: true,
		},
		{
			name: "do not create headless service when one already exists",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "prefill",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 2,
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "container",
												Image: "nginx",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			existingRoles: []datastore.Role{
				{Name: "prefill-0", Status: datastore.RoleRunning, Revision: "v1"},
			},
			existingServices: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-ms-0-prefill-0-0",
						Namespace: "default",
						Labels: map[string]string{
							workloadv1alpha1.GroupNameLabelKey: "test-ms-0",
							workloadv1alpha1.RoleLabelKey:      "prefill",
							workloadv1alpha1.RoleIDKey:         "prefill-0",
						},
					},
				},
			},
			expectedServiceCount: 1,
			expectServiceCreated: false,
		},
		{
			name: "skip creating service when WorkerTemplate is nil",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "prefill",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 0,
								WorkerTemplate: nil, // No worker template
							},
						},
					},
				},
			},
			existingRoles: []datastore.Role{
				{Name: "prefill-0", Status: datastore.RoleRunning, Revision: "v1"},
			},
			existingServices:     []*corev1.Service{},
			expectedServiceCount: 0,
			expectServiceCreated: false,
		},
		{
			name: "skip creating service for deleting roles",
			modelServing: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           "prefill",
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: 2,
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{
												Name:  "container",
												Image: "nginx",
											},
										},
									},
								},
							},
						},
					},
				},
			},
			existingRoles: []datastore.Role{
				{Name: "prefill-0", Status: datastore.RoleDeleting, Revision: "v1"}, // Role is deleting
			},
			existingServices:     []*corev1.Service{},
			expectedServiceCount: 0,
			expectServiceCreated: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup fake clients
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextClient := apiextfake.NewSimpleClientset()

			// Create controller
			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
			assert.NoError(t, err)

			// Setup the datastore with serving groups and roles
			groupName := utils.GenerateServingGroupName(tt.modelServing.Name, 0)
			controller.store.AddServingGroup(utils.GetNamespaceName(tt.modelServing), 0, "v1")

			for _, role := range tt.existingRoles {
				controller.store.AddRole(utils.GetNamespaceName(tt.modelServing), groupName, "prefill", role.Name, role.Revision)
				controller.store.UpdateRoleStatus(utils.GetNamespaceName(tt.modelServing), groupName, "prefill", role.Name, role.Status)
			}

			// Add existing services to the fake client
			for _, svc := range tt.existingServices {
				_, err := kubeClient.CoreV1().Services(svc.Namespace).Create(context.TODO(), svc, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			// Call the function being tested
			err = controller.manageHeadlessService(context.TODO(), tt.modelServing)
			assert.NoError(t, err)

			// Verify the expected number of services exist
			svcList, err := kubeClient.CoreV1().Services("default").List(context.TODO(), metav1.ListOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedServiceCount, len(svcList.Items))

			// Check if services were created based on the expected outcome
			if tt.expectServiceCreated {
				// If we expect services to be created, verify they have the correct labels
				for _, item := range svcList.Items {
					assert.Contains(t, item.Labels, workloadv1alpha1.GroupNameLabelKey)
					assert.Contains(t, item.Labels, workloadv1alpha1.RoleLabelKey)
					assert.Contains(t, item.Labels, workloadv1alpha1.RoleIDKey)
				}
			}
		})
	}
}
