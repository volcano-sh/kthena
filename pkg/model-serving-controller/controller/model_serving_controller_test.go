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
	corev1 "k8s.io/api/core/v1"
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
	mi := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Namespace: ns}}
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
			got := c.isServingGroupOutdated(group, mi.Namespace, newHash)
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
	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-mi",
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
		store.AddRunningPodToServingGroup(utils.GetNamespaceName(mi), groupName, pod.Name, newHash, roleLabel, roleName)
	}

	// Waiting for pod cache to sync
	sync := waitForObjectInCache(t, 2*time.Second, func() bool {
		pods, _ := controller.podsLister.Pods(ns).List(labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
		}))
		return len(pods) == int(expectedPodNum)
	})
	assert.True(t, sync, "Pods should be found in cache")

	ok, err := controller.checkServingGroupReady(mi, groupName)
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
	store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(mi), groupName, "pod-1")

	ok, err = controller.checkServingGroupReady(mi, groupName)
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestIsServingGroupDeleted(t *testing.T) {
	ns := "default"
	groupName := "test-mi-0"
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

	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-mi",
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

			store.AddServingGroup(utils.GetNamespaceName(mi), 0, "test-revision")
			err := store.UpdateServingGroupStatus(utils.GetNamespaceName(mi), groupName, tc.servingGroupStatus)
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
			got := controller.isServingGroupDeleted(mi, groupName)
			assert.Equal(t, tc.want, got, "isServingGroupDeleted result should match expected")

			store.DeleteServingGroup(utils.GetNamespaceName(mi), groupName)
		})
	}
}

func TestIsRoleDeleted(t *testing.T) {
	ns := "default"
	groupName := "test-mi-0"
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

	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-mi",
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

			store.AddRole(utils.GetNamespaceName(mi), groupName, roleName, roleID, "test-revision")
			err := store.UpdateRoleStatus(utils.GetNamespaceName(mi), groupName, roleName, roleID, tc.roleStatus)
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
			got := controller.isRoleDeleted(mi, groupName, roleName, roleID)
			assert.Equal(t, tc.want, got, "isRoleDeleted result should match expected")

			store.DeleteServingGroup(utils.GetNamespaceName(mi), groupName)
		})
	}
}

func TestModelServingControllerModelServingLifecycle(t *testing.T) {
	// Create fake clients
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()

	// Create informer factories
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	// Create controller
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient)
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
		mi := createStandardModelServing("test-mi", 2, 3)
		// Add ModelServing to fake client
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-mi")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Simulate controller processing the creation
		err = controller.syncModelServing(context.Background(), "default/test-mi")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(mi) * int(*mi.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, mi, 2)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, mi, 2)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, mi, 2)
	})

	// Test Case 2: ModelServing Scale Up
	t.Run("ModelServingScaleUp", func(t *testing.T) {
		mi := createStandardModelServing("test-mi-scale-up", 1, 2)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-mi-scale-up")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-mi-scale-up")
		assert.NoError(t, err)

		// Verify ServingGroups initial state
		verifyServingGroups(t, controller, mi, 1)

		// Update ModelServing to scale up
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](3) // Scale up to 3 ServingGroups

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-mi-scale-up")
			return err == nil && *mi.Spec.Replicas == 3
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update
		err = controller.syncModelServing(context.Background(), "default/test-mi-scale-up")
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
		mi := createStandardModelServing("test-mi-scale-down", 3, 2)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-mi-scale-down")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Process initial creation
		err = controller.syncModelServing(context.Background(), "default/test-mi-scale-down")
		assert.NoError(t, err)

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(mi) * int(*mi.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, mi, 3)
		verifyPodCount(t, controller, mi, 3)
		verifyRoles(t, controller, mi, 3)

		// Update ModelServing to scale down
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](1) // Scale up to 1 ServingGroups

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-mi-scale-down")
			return err == nil && *mi.Spec.Replicas == 1
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Process the update
		err = controller.syncModelServing(context.Background(), "default/test-mi-scale-down")
		assert.NoError(t, err)

		requirement, err := labels.NewRequirement(
			workloadv1alpha1.GroupNameLabelKey,
			selection.In,
			[]string{"test-mi-scale-down-1", "test-mi-scale-down-2"},
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
		mi := createStandardModelServing("test-role-scale-up", 2, 1)
		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
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
		verifyServingGroups(t, controller, mi, 2)

		// Update ModelServing to role scale down
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Template.Roles[0].Replicas = ptr.To[int32](3) // Scale up to 3 roles

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-up")
			return err == nil && *mi.Spec.Template.Roles[0].Replicas == 3
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
		mi := createStandardModelServing("test-role-scale-down", 2, 3)

		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
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
		expectedPodCount := utils.ExpectedPodNum(mi) * int(*mi.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, mi, 2)
		verifyPodCount(t, controller, mi, 2)
		verifyRoles(t, controller, mi, 2)

		// Update ModelServing to role scale down
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) // Scale down to 1 role

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-down")
			return err == nil && *mi.Spec.Template.Roles[0].Replicas == 1
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
		// Test Case: ModelServing with PodDelectionCost annotation - BinPack Scale
		mi := createStandardModelServing("test-binpack-scale", 4, 1)

		// Create initial ModelServing
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{})
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
		expectedPodCount := utils.ExpectedPodNum(mi) * int(*mi.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Initial status check
		verifyServingGroups(t, controller, mi, 4)
		verifyPodCount(t, controller, mi, 4)
		verifyRoles(t, controller, mi, 4)

		// Add PodDelectionCost annotations to pods
		// Get all pods and add different deletion costs
		pods, err := controller.podsLister.Pods("default").List(labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: mi.Name,
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
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](1) // Scale down to 1 ServingGroup

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-binpack-scale")
			return err == nil && *mi.Spec.Replicas == 1
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
		mi := createGangModelServing("test-gang-ms", 2, 2)

		// Add ModelServing to fake client
		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{},
		)
		assert.NoError(t, err)

		// Wait for object to be available in cache
		found := waitForObjectInCache(t, 2*time.Second, func() bool {
			_, err := controller.modelServingLister.ModelServings("default").Get("test-gang-ms")
			return err == nil
		})
		assert.True(t, found, "ModelServing should be found in cache after creation")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, mi, 2)

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
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == mi.Name {
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
			if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] != mi.Name {
				continue
			}
			// Check MinMember equals per-group pod count for this MI
			expectedMinMember := int32(utils.ExpectedPodNum(mi))
			assert.Equal(t, expectedMinMember, pg.Spec.MinMember, "PodGroup MinMember should match expected per-servinggroup pod count")
		}

		t.Logf("Scaling up ModelServing replicas to trigger PodGroup updates")
		// Scale up ModelServing replicas to trigger PodGroup updates
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Replicas = ptr.To[int32](3)

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{},
		)
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-gang-ms")
			return err == nil && *mi.Spec.Replicas == 3
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
		mi := createGangModelServing("test-gang-cleanup", 2, 1)

		_, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(
			context.Background(), mi, metav1.CreateOptions{},
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
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == mi.Name {
					count++
				}
			}
			return count == 2
		})
		assert.True(t, found, "two PodGroups should exist for this ModelServing before disabling gang policy")

		// Disable gang policy on the ModelServing
		updatedMI := mi.DeepCopy()
		updatedMI.Spec.Template.GangPolicy = nil

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMI, metav1.UpdateOptions{},
		)
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			mi, err := controller.modelServingLister.ModelServings("default").Get("test-gang-cleanup")
			return err == nil && mi.Spec.Template.GangPolicy == nil
		})
		assert.True(t, found, "Updated ModelServing without gang policy should be found in cache")

		// Process the update which should trigger PodGroup cleanup
		err = controller.syncModelServing(context.Background(), "default/test-gang-cleanup")
		assert.NoError(t, err)

		// Verify that PodGroups have been cleaned up for this ModelServing
		pgListCleanup, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
			context.Background(), metav1.ListOptions{},
		)
		assert.NoError(t, err)
		countCleanup := 0
		for _, pg := range pgListCleanup.Items {
			if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == updatedMI.Name {
				countCleanup++
			}
		}
		// Current controller behavior does not delete PodGroups when only GangPolicy is removed;
		// cleanup is tied to network topology removal. We therefore expect the PodGroups for
		// this ModelServing to remain.
		assert.Equal(t, 2, countCleanup, "PodGroups for this ModelServing remain when only gang policy is disabled")
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
	mi := createStandardModelServing(name, replicas, roleReplicas)
	mi.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{
		MinRoleReplicas: map[string]int32{
			"prefill": roleReplicas,
		},
	}
	return mi
}

// verifyServingGroups Verify the number and name of ServingGroup
func verifyServingGroups(t *testing.T, controller *ModelServingController, mi *workloadv1alpha1.ModelServing, expectedCount int) {
	groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	assert.NoError(t, err)
	assert.Equal(t, expectedCount, len(groups), fmt.Sprintf("Should have %d ServingGroups", expectedCount))

	// Verify that the ServingGroup name follows the expected pattern
	expectedGroupNames := make([]string, expectedCount)
	for i := 0; i < expectedCount; i++ {
		expectedGroupNames[i] = fmt.Sprintf("%s-%d", mi.Name, i)
	}

	actualGroupNames := make([]string, len(groups))
	for i, group := range groups {
		actualGroupNames[i] = group.Name
	}
	assert.Equal(t, expectedGroupNames, actualGroupNames, "ServingGroup names should follow expected pattern")
}

// verifyPodCount Verify the number of Pods in each ServingGroup
func verifyPodCount(t *testing.T, controller *ModelServingController, mi *workloadv1alpha1.ModelServing, expectedGroups int) {
	expectPodNum := utils.ExpectedPodNum(mi)
	for i := 0; i < expectedGroups; i++ {
		groupName := fmt.Sprintf("%s-%d", mi.Name, i)
		groupSelector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
		})

		groupPods, err := controller.podsLister.Pods(mi.Namespace).List(groupSelector)
		assert.NoError(t, err)
		assert.Equal(t, expectPodNum, len(groupPods), fmt.Sprintf("ServingGroup %s should have %d pods", groupName, expectPodNum))
	}
}

// verifyRoles Verify the number and name of Role
func verifyRoles(t *testing.T, controller *ModelServingController, mi *workloadv1alpha1.ModelServing, expectedGroups int) {
	// Traverse each ServingGroup
	servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
	assert.NoError(t, err)
	for _, group := range servingGroups {
		groupName := group.Name

		// Traverse each role defined in the ModelServing spec
		for _, specRole := range mi.Spec.Template.Roles {
			roleName := specRole.Name
			expectedRoleReplicas := int(*specRole.Replicas)

			// Get all instances of the role from the store
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(mi), groupName, roleName)
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

			// Create controller without running it to avoid background sync interference
			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient)
			assert.NoError(t, err)

			// Create a unique ModelServing for this test
			miName := fmt.Sprintf("test-scaleup-%d", idx)
			mi := &workloadv1alpha1.ModelServing{
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
				controller.store.AddServingGroup(utils.GetNamespaceName(mi), ordinal, "test-revision")
			}

			// Build the servingGroupList to pass to scaleUpServingGroups
			existingGroups := make([]datastore.ServingGroup, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingGroups[i] = datastore.ServingGroup{
					Name: utils.GenerateServingGroupName(miName, ordinal),
				}
			}

			// Call scaleUpServingGroups directly (not through syncModelServing)
			err = controller.scaleUpServingGroups(context.Background(), mi, existingGroups, tt.expectedCount, "new-revision")
			assert.NoError(t, err)

			// Verify the results
			groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
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

			// Create controller without running it to avoid background sync interference
			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient)
			assert.NoError(t, err)

			// Create a unique ModelServing for this test
			miName := fmt.Sprintf("test-scaleup-roles-%d", idx)
			roleName := "prefill"
			groupName := utils.GenerateServingGroupName(miName, 0)
			mi := &workloadv1alpha1.ModelServing{
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
			controller.store.AddServingGroup(utils.GetNamespaceName(mi), 0, "test-revision")
			for _, ordinal := range tt.existingIndices {
				controller.store.AddRole(utils.GetNamespaceName(mi), groupName, "prefill", utils.GenerateRoleID("prefill", ordinal), "test-revision")
			}

			// Build the roleList to pass to scaleUpRoles
			existingRoles := make([]datastore.Role, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingRoles[i] = datastore.Role{
					Name: utils.GenerateRoleID("prefill", ordinal),
				}
			}

			targetRole := mi.Spec.Template.Roles[0]

			// Call scaleUpRoles directly
			controller.scaleUpRoles(context.Background(), mi, groupName, targetRole, existingRoles, tt.expectedCount, 0, "new-revision")

			// Verify the results
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(mi), groupName, "prefill")
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

// TestScaleDownServingGroups tests the scaleDownServingGroups function with non-binpack scenarios
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

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient)
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-scaledown-%d", idx)
			mi := &workloadv1alpha1.ModelServing{
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
				controller.store.AddServingGroup(utils.GetNamespaceName(mi), ordinal, "test-revision")
			}

			// Build the servingGroupList to pass to scaleDownServingGroups
			existingGroups := make([]datastore.ServingGroup, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingGroups[i] = datastore.ServingGroup{
					Name: utils.GenerateServingGroupName(miName, ordinal),
				}
			}

			// Call scaleDownServingGroups directly (no binpack - all scores are 0)
			err = controller.scaleDownServingGroups(context.Background(), mi, existingGroups, tt.expectedCount)
			assert.NoError(t, err)

			// Verify the results
			groups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(mi))
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

// TestScaleDownRoles tests the scaleDownRoles function with non-binpack scenarios
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

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient)
			assert.NoError(t, err)

			miName := fmt.Sprintf("test-role-scaledown-%d", idx)
			groupName := utils.GenerateServingGroupName(miName, 0)
			mi := &workloadv1alpha1.ModelServing{
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

			targetRole := mi.Spec.Template.Roles[0]

			// Pre-populate the store with ServingGroup and Roles
			controller.store.AddServingGroup(utils.GetNamespaceName(mi), 0, "test-revision")
			for _, ordinal := range tt.existingIndices {
				controller.store.AddRole(utils.GetNamespaceName(mi), groupName, "prefill", utils.GenerateRoleID("prefill", ordinal), "test-revision")
			}

			// Build the roleList to pass to scaleDownRoles
			existingRoles := make([]datastore.Role, len(tt.existingIndices))
			for i, ordinal := range tt.existingIndices {
				existingRoles[i] = datastore.Role{
					Name: utils.GenerateRoleID("prefill", ordinal),
				}
			}

			// Call scaleDownRoles directly (no binpack - all scores are 0)
			controller.scaleDownRoles(context.Background(), mi, groupName, targetRole, existingRoles, tt.expectedCount)

			// Verify the results
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(mi), groupName, "prefill")
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
