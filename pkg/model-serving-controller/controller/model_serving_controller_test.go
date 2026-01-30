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
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/assert"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	volcanoinformers "volcano.sh/apis/pkg/client/informers/externalversions"

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

func TestCreateOrUpdatePodGroupByServingGroupRequeue(t *testing.T) {
	controller := &ModelServingController{
		podGroupManager: &podgroupmanager.Manager{},
	}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms",
			Namespace: "default",
		},
	}

	called := false
	var delay time.Duration
	patches := gomonkey.NewPatches()
	patches.ApplyMethod(reflect.TypeOf(controller.podGroupManager), "CreateOrUpdatePodGroup", func(_ *podgroupmanager.Manager, _ context.Context, _ *workloadv1alpha1.ModelServing, _ string) (error, time.Duration) {
		return fmt.Errorf("retry"), 2 * time.Second
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "enqueueModelServingAfter", func(_ *ModelServingController, _ *workloadv1alpha1.ModelServing, duration time.Duration) {
		called = true
		delay = duration
	})
	defer patches.Reset()

	err := controller.createOrUpdatePodGroupByServingGroup(context.Background(), ms, "ms-0")
	assert.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, 2*time.Second, delay)
}

func TestCreatePodAlreadyExistsRequeues(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()

	stopCh := make(chan struct{})
	defer close(stopCh)
	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	controller := &ModelServingController{
		kubeClientSet: kubeClient,
		podsLister:    podInformer.Lister(),
		podsInformer:  podInformer.Informer(),
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms",
			Namespace: "default",
			UID:       types.UID("new-uid"),
		},
	}

	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms-entry-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
					Kind:       workloadv1alpha1.ModelServingKind.Kind,
					UID:        types.UID("old-uid"),
				},
			},
		},
	}

	_, err := kubeClient.CoreV1().Pods("default").Create(context.Background(), existing, metav1.CreateOptions{})
	assert.NoError(t, err)
	assert.NoError(t, podInformer.Informer().GetIndexer().Add(existing))

	newPod := existing.DeepCopy()
	newPod.OwnerReferences = []metav1.OwnerReference{
		{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
			Kind:       workloadv1alpha1.ModelServingKind.Kind,
			UID:        ms.UID,
		},
	}

	called := false
	patches := gomonkey.NewPatches()
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "enqueueModelServingAfter", func(_ *ModelServingController, _ *workloadv1alpha1.ModelServing, _ time.Duration) {
		called = true
	})
	defer patches.Reset()

	err = controller.createPod(context.Background(), ms, "ms-0", "role", "role-0", newPod, true, nil, "entry")
	assert.NoError(t, err)
	assert.True(t, called)
}

func TestDeletePodGroupEnqueues(t *testing.T) {
	controller := &ModelServingController{}
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms",
			Namespace: "default",
			UID:       types.UID("ms-uid"),
		},
	}
	podGroup := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ms-0",
			Namespace: "default",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        "ms-0",
			},
		},
	}

	called := false
	patches := gomonkey.NewPatches()
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "getModelServingAndResourceDetails", func(_ *ModelServingController, _ metav1.Object) (*workloadv1alpha1.ModelServing, string, string, string) {
		return ms, "ms-0", "", ""
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "shouldSkipHandling", func(_ *ModelServingController, _ *workloadv1alpha1.ModelServing, _ string, _ metav1.Object) bool {
		return false
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "handleDeletionInProgress", func(_ *ModelServingController, _ *workloadv1alpha1.ModelServing, _ string, _ string, _ string) bool {
		return false
	})
	patches.ApplyPrivateMethod(reflect.TypeOf(controller), "enqueueModelServing", func(_ *ModelServingController, _ *workloadv1alpha1.ModelServing) {
		called = true
	})
	defer patches.Reset()

	controller.deletePodGroup(podGroup)
	assert.True(t, called)
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

	// TODO: Add a Test Helper to setup controller test environment
	kubeClient := kubefake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	serviceInformer := kubeInformerFactory.Core().V1().Services()
	podGroupInformerFactory := volcanoinformers.NewSharedInformerFactory(volcanoClient, 0)
	podGroupInformer := podGroupInformerFactory.Scheduling().V1beta1().PodGroups()

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

	err = podGroupInformer.Informer().AddIndexers(cache.Indexers{
		GroupNameKey: utils.GroupNameIndexFunc,
	})
	assert.NoError(t, err)

	store := datastore.New()
	manager := podgroupmanager.NewManager(kubeClient, volcanoClient, apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD()), store, nil)
	if manager != nil {
		manager.PodGroupInformer = podGroupInformer.Informer()
		manager.PodGroupLister = podGroupInformer.Lister()
	}
	controller := &ModelServingController{
		podsInformer:     podInformer.Informer(),
		servicesInformer: serviceInformer.Informer(),
		podsLister:       podInformer.Lister(),
		servicesLister:   serviceInformer.Lister(),
		podGroupManager:  manager,
		store:            store,
	}

	stop := make(chan struct{})
	defer close(stop)
	kubeInformerFactory.Start(stop)
	kubeInformerFactory.WaitForCacheSync(stop)
	podGroupInformerFactory.Start(stop)
	podGroupInformerFactory.WaitForCacheSync(stop)

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
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Replicas = ptr.To[int32](3) // Scale up to 3 ServingGroups

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{})
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
		expectedPodCount := utils.ExpectedPodNum(updatedMS) * int(*updatedMS.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMS, 3)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, updatedMS, 3)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, updatedMS, 3)
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
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Replicas = ptr.To[int32](1) // Scale down to 1 ServingGroup

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{})
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
		expectedPodCount = utils.ExpectedPodNum(updatedMS) * int(*updatedMS.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMS, 1)
		// Verify each ServingGroup has correct roles
		verifyRoles(t, controller, updatedMS, 1)
		// Verify each ServingGroup has correct pods
		verifyPodCount(t, controller, updatedMS, 1)
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
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Template.Roles[0].Replicas = ptr.To[int32](3) // Scale up to 3 roles

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{})
		assert.NoError(t, err)

		// Wait for update to be available in cache
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			ms, err := controller.modelServingLister.ModelServings("default").Get("test-role-scale-up")
			return err == nil && *ms.Spec.Template.Roles[0].Replicas == 3
		})
		assert.True(t, found, "Updated ModelServing should be found in cache")

		// Wait for pods to be created and synced to cache
		expectedPodCount := utils.ExpectedPodNum(updatedMS) * int(*updatedMS.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMS, 2)

		// Verify total number of roles across all groups matches spec (role scaling doesn't guarantee per-group equality)
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMS))
		assert.NoError(t, err)
		totalRoles := 0
		for _, g := range servingGroups {
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMS), g.Name, "prefill")
			assert.NoError(t, err)
			totalRoles += len(roles)
		}
		// Spec says 3 replicas per group, across 2 groups that's 6 total
		assert.Equal(t, 6, totalRoles, "total prefill roles across all groups should match spec")

		// Verify total pods match expected count
		selector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
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
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) // Scale down to 1 role

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{})
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
		expectedPodCount = utils.ExpectedPodNum(updatedMS) * int(*updatedMS.Spec.Replicas)
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
			})
			pods, _ := controller.podsLister.Pods("default").List(selector)
			return len(pods) == expectedPodCount
		})
		assert.True(t, found, "Pods should be created and synced to cache")

		// Verify ServingGroups were created in store
		verifyServingGroups(t, controller, updatedMS, 2)

		// Verify total number of roles across all groups matches spec (role scaling doesn't guarantee per-group equality)
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMS))
		assert.NoError(t, err)
		totalRoles := 0
		for _, g := range servingGroups {
			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMS), g.Name, "prefill")
			assert.NoError(t, err)
			totalRoles += len(roles)
		}
		// After scale down, specRole.Replicas == 1 per group, 2 groups total => 2 roles
		assert.Equal(t, 2, totalRoles, "total prefill roles across all groups should match spec after scale down")

		// Verify total pods match expected count
		podSelector := labels.SelectorFromSet(map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: updatedMS.Name,
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
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Replicas = ptr.To[int32](1) // Scale down to 1 ServingGroup

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{})
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
		servingGroups, err := controller.store.GetServingGroupByModelServing(utils.GetNamespaceName(updatedMS))
		assert.NoError(t, err)
		assert.Equal(t, 1, len(servingGroups))

		remainingRoles, err := controller.store.GetRoleList(utils.GetNamespaceName(updatedMS), servingGroups[0].Name, "prefill")
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

		// Wait until both PodGroups for this ModelServing are created
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
			// Check MinMember equals per-group pod count for this ModelServing
			expectedMinMember := int32(utils.ExpectedPodNum(ms))
			assert.Equal(t, expectedMinMember, pg.Spec.MinMember, "PodGroup MinMember should match expected per-servinggroup pod count")
		}

		t.Logf("Scaling up ModelServing replicas to trigger PodGroup updates")
		// Scale up ModelServing replicas to trigger PodGroup updates
		updatedMS := ms.DeepCopy()
		updatedMS.Spec.Replicas = ptr.To[int32](3)

		_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Update(
			context.Background(), updatedMS, metav1.UpdateOptions{},
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

		// Wait until three PodGroups for this ModelServing are created after scale up
		found = waitForObjectInCache(t, 2*time.Second, func() bool {
			pgList, err := volcanoClient.SchedulingV1beta1().PodGroups("default").List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil {
				return false
			}
			count := 0
			for _, pg := range pgList.Items {
				if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] == updatedMS.Name {
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
			if pg.Labels[workloadv1alpha1.ModelServingNameLabelKey] != updatedMS.Name {
				continue
			}
			// Check MinMember equals per-group pod count for this ModelServing
			expectedMinMember := int32(utils.ExpectedPodNum(updatedMS))
			assert.Equal(t, expectedMinMember, pg.Spec.MinMember, "PodGroup MinMember should match expected per-servinggroup pod count for updated ModelServing")
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

		// Wait until both PodGroups for this ModelServing are created
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
			msName := fmt.Sprintf("test-scaleup-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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
					Name: utils.GenerateServingGroupName(msName, ordinal),
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
					expectedName := utils.GenerateServingGroupName(msName, expectedIdx)
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
			msName := fmt.Sprintf("test-scaleup-roles-%d", idx)
			roleName := "prefill"
			groupName := utils.GenerateServingGroupName(msName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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

func TestManageRoleReplicas(t *testing.T) {
	tests := []struct {
		name             string
		roleReplicas     int32
		workerReplicas   int32
		initialRoleIDs   []int
		addEntryPod      bool
		mismatchOwnerUID bool
		expectedRoleSize int
		expectedPodCount int
		expectRequeue    bool
	}{
		{
			name:             "recreate missing pods when role count matches",
			roleReplicas:     1,
			workerReplicas:   1,
			initialRoleIDs:   []int{0},
			addEntryPod:      true,
			expectedRoleSize: 1,
			expectedPodCount: 2,
			expectRequeue:    false,
		},
		{
			name:             "scale up when role replicas are less than expected",
			roleReplicas:     2,
			workerReplicas:   0,
			initialRoleIDs:   []int{0},
			addEntryPod:      false,
			expectedRoleSize: 2,
			expectedPodCount: 2,
			expectRequeue:    false,
		},
		{
			name:             "scale down when role replicas are more than expected",
			roleReplicas:     1,
			workerReplicas:   0,
			initialRoleIDs:   []int{0, 1},
			addEntryPod:      false,
			expectedRoleSize: 1,
			expectedPodCount: 2,
			expectRequeue:    false,
		},
		{
			name:             "reenqueue when pod owner UID mismatches",
			roleReplicas:     1,
			workerReplicas:   0,
			initialRoleIDs:   []int{0},
			addEntryPod:      true,
			mismatchOwnerUID: true,
			expectedRoleSize: 1,
			expectedPodCount: 1,
			expectRequeue:    true,
		},
	}

	for idx, tt := range tests {
		// set klog verbsity to error to reduce test output noise

		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
			assert.NoError(t, err)

			roleName := "default"
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      fmt.Sprintf("test-manage-role-%d", idx),
					UID:       types.UID(fmt.Sprintf("ms-uid-%d", idx)),
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           roleName,
								Replicas:       ptr.To[int32](tt.roleReplicas),
								WorkerReplicas: tt.workerReplicas,
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{
											Name:  "entry-container",
											Image: "test-image:latest",
										}},
									},
								},
								WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{
											Name:  "worker-container",
											Image: "test-image:latest",
										}},
									},
								},
							},
						},
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
				},
			}

			groupName := utils.GenerateServingGroupName(ms.Name, 0)
			revision := "rev-1"
			controller.store.AddServingGroup(utils.GetNamespaceName(ms), 0, revision)
			for _, roleID := range tt.initialRoleIDs {
				controller.store.AddRole(utils.GetNamespaceName(ms), groupName, roleName, utils.GenerateRoleID(roleName, roleID), revision)
			}

			if tt.addEntryPod {
				entryPod := utils.GenerateEntryPod(ms.Spec.Template.Roles[0], ms, groupName, 0, revision)
				if tt.mismatchOwnerUID && len(entryPod.OwnerReferences) > 0 {
					entryPod.OwnerReferences[0].UID = types.UID("mismatched-uid")
				}
				_, err = kubeClient.CoreV1().Pods(ms.Namespace).Create(context.Background(), entryPod, metav1.CreateOptions{})
				assert.NoError(t, err)
				assert.NoError(t, controller.podsInformer.GetIndexer().Add(entryPod))
			}

			controller.manageRoleReplicas(context.Background(), ms, groupName, ms.Spec.Template.Roles[0], 0, revision)

			roles, err := controller.store.GetRoleList(utils.GetNamespaceName(ms), groupName, roleName)
			assert.NoError(t, err)
			assert.Len(t, roles, tt.expectedRoleSize, "role list should match expected count")

			//if tt.expectedPodCount > 0 {
			selector := labels.SelectorFromSet(map[string]string{
				workloadv1alpha1.GroupNameLabelKey: groupName,
				workloadv1alpha1.RoleLabelKey:      roleName,
			})
			pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector.String()})
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedPodCount, len(pods.Items), "pod count should match expected")
			//}

			if tt.expectRequeue {
				requeued := waitForObjectInCache(t, 2*time.Second, func() bool {
					return controller.workqueue.Len() > 0
				})
				assert.True(t, requeued, "model serving should be requeued for owner UID mismatch")
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

			msName := fmt.Sprintf("test-scaledown-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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
					Name: utils.GenerateServingGroupName(msName, ordinal),
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

			msName := fmt.Sprintf("test-priority-scaledown-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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
				groupName := utils.GenerateServingGroupName(msName, ordinal)
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
							workloadv1alpha1.ModelServingNameLabelKey: msName,
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
					Name: utils.GenerateServingGroupName(msName, ordinal),
				}
			}

			// Call scaleDownServingGroups with priority and deletion cost
			err = controller.scaleDownServingGroups(context.Background(), ms, existingGroups, tt.expectedCount)
			assert.NoError(t, err)

			// Manually delete ServingGroups that are marked as Deleting from the store
			// This simulates the deletion process that would happen in the real controller
			for _, ordinal := range tt.existingIndices {
				groupName := utils.GenerateServingGroupName(msName, ordinal)
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

// TestScaleDownServingGroupsWithPartition tests the scaleDownServingGroups function with partition protection
// This test verifies that when partition is set, only ServingGroups with ordinal >= partition
// are considered for deletion, protecting partition-protected replicas.
func TestScaleDownServingGroupsWithPartition(t *testing.T) {
	tests := []struct {
		name                   string
		partition              *int32
		existingIndices        []int
		expectedCount          int
		podDeletionCosts       map[int]int // Index -> DeletionCost
		groupStatuses          map[int]datastore.ServingGroupStatus
		expectedRemainingNames []string
		description            string
	}{
		{
			name:            "partition=3, protect replicas below partition",
			partition:       ptr.To[int32](3),
			existingIndices: []int{0, 1, 2, 3, 4},
			expectedCount:   3,
			podDeletionCosts: map[int]int{
				0: 0,   // Low cost but protected (ordinal < partition)
				1: 0,   // Low cost but protected (ordinal < partition)
				2: 0,   // Low cost but protected (ordinal < partition)
				3: 100, // High cost, not protected (ordinal >= partition)
				4: 50,  // Medium cost, not protected (ordinal >= partition)
			},
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
				4: datastore.ServingGroupRunning,
			},
			expectedRemainingNames: []string{"0", "1", "2"}, // R-3, R-4 deleted (ordinal >= partition), R-0, R-1, R-2 protected
			description:            "Partition-protected replicas (R-0, R-1, R-2) should never be deleted even with low deletion cost",
		},
		{
			name:             "partition=3, not-ready groups below partition still protected",
			partition:        ptr.To[int32](3),
			existingIndices:  []int{0, 1, 2, 3, 4},
			expectedCount:    3,
			podDeletionCosts: map[int]int{},
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupCreating, // Not ready but protected (ordinal < partition)
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
				4: datastore.ServingGroupRunning,
			},
			expectedRemainingNames: []string{"0", "1", "2"}, // R-3, R-4 deleted, R-1 protected even though not ready
			description:            "Partition-protected replicas should never be deleted even if not ready",
		},
		{
			name:            "no partition, all replicas can be deleted",
			partition:       nil,
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2,
			podDeletionCosts: map[int]int{
				0: 100, // High cost
				1: 50,  // Medium cost
				2: 0,   // Low cost - should be deleted
				3: 75,  // Medium-high cost
			},
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			expectedRemainingNames: []string{"0", "3"}, // R-2 (low cost) and R-1 (medium cost) deleted
			description:            "Without partition, all replicas are candidates for deletion based on binpack scoring",
		},
		{
			name:            "partition=5, all replicas protected",
			partition:       ptr.To[int32](5),
			existingIndices: []int{0, 1, 2, 3},
			expectedCount:   2, // Scale down to trigger deletion of protected replicas
			podDeletionCosts: map[int]int{
				0: 100, // High cost, protected
				1: 50,  // Medium cost, protected
				2: 0,   // Low cost, protected (should be deleted first)
				3: 200, // Very high cost, protected (should be deleted last)
			},
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
			},
			expectedRemainingNames: []string{"0", "3"}, // R-2 (lowest cost) and R-1 (medium cost) deleted based on binpack scoring
			description:            "When partition exceeds all replica indices, all replicas are classified as protected and deletion is based on binpack scoring within protected list",
		},
		{
			name:            "partition=3, scale down below partition - delete protected after non-protected",
			partition:       ptr.To[int32](3),
			existingIndices: []int{0, 1, 2, 3, 4},
			expectedCount:   2, // Scale down below partition value
			podDeletionCosts: map[int]int{
				0: 100, // High cost, protected
				1: 50,  // Medium cost, protected
				2: 0,   // Low cost, protected
				3: 200, // Very high cost, not protected
				4: 150, // High cost, not protected
			},
			groupStatuses: map[int]datastore.ServingGroupStatus{
				0: datastore.ServingGroupRunning,
				1: datastore.ServingGroupRunning,
				2: datastore.ServingGroupRunning,
				3: datastore.ServingGroupRunning,
				4: datastore.ServingGroupRunning,
			},
			expectedRemainingNames: []string{"0", "1"}, // First delete R-3, R-4 (non-protected), then R-2 (protected, lowest cost)
			description:            "When scaling down below partition, delete non-protected first, then protected",
		},
	}

	for idx, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake.NewSimpleClientset())
			assert.NoError(t, err)

			msName := fmt.Sprintf("test-partition-scaledown-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition: tt.partition,
						},
					},
				},
			}

			podIndexer := controller.podsInformer.GetIndexer()

			// Pre-populate the store with existing ServingGroups and set their statuses
			for _, ordinal := range tt.existingIndices {
				groupName := utils.GenerateServingGroupName(msName, ordinal)
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
							workloadv1alpha1.ModelServingNameLabelKey: msName,
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
					Name: utils.GenerateServingGroupName(msName, ordinal),
				}
			}

			// Call scaleDownServingGroups with partition protection
			err = controller.scaleDownServingGroups(context.Background(), ms, existingGroups, tt.expectedCount)
			assert.NoError(t, err)

			// Manually delete ServingGroups that are marked as Deleting from the store
			// This simulates the deletion process that would happen in the real controller
			for _, ordinal := range tt.existingIndices {
				groupName := utils.GenerateServingGroupName(msName, ordinal)
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

			// Verify partition protection: protected groups should only be deleted after all non-protected groups are deleted
			if tt.partition != nil && *tt.partition > 0 {
				// Count how many non-protected groups existed
				nonProtectedCount := 0
				for _, ordinal := range tt.existingIndices {
					if ordinal >= int(*tt.partition) {
						nonProtectedCount++
					}
				}
				// Count how many non-protected groups remain
				remainingNonProtectedCount := 0
				for _, g := range groups {
					_, ordinal := utils.GetParentNameAndOrdinal(g.Name)
					if ordinal >= int(*tt.partition) {
						remainingNonProtectedCount++
					}
				}
				// If there are remaining non-protected groups, protected groups should not be deleted
				if remainingNonProtectedCount > 0 {
					for _, ordinal := range tt.existingIndices {
						if ordinal < int(*tt.partition) {
							groupName := utils.GenerateServingGroupName(msName, ordinal)
							group := controller.store.GetServingGroup(utils.GetNamespaceName(ms), groupName)
							assert.NotNil(t, group,
								fmt.Sprintf("[%s] Partition-protected replica R-%d should not be deleted when non-protected groups still exist", tt.description, ordinal))
						}
					}
				}
			}
		})
	}
}

// TestModelServingVersionControl tests the version control functionality for ModelServing
// This test verifies that when partition is set, deleted servingGroups below partition
// can be recreated with their historical revision instead of the new revision.
// The test directly calls scaleUpServingGroups to verify its behavior.
func TestModelServingVersionControl(t *testing.T) {
	tests := []struct {
		name                    string
		partition               *int32
		initialReplicas         int32
		initialRevision         string
		existingGroups          []int // Ordinals of existing groups before scale up
		scaleUpTo               int32
		expectedRecreatedRevs   map[int]string // ordinal -> expected revision for recreated groups
		expectedCurrentRevision string
		expectedUpdateRevision  string
	}{
		{
			name:            "partition=2, create new group above partition should use new revision",
			partition:       ptr.To[int32](2),
			initialReplicas: 2, // R-0, R-1 (both < partition=2, protected)
			initialRevision: "revision-v1",
			existingGroups:  []int{0, 1}, // Existing groups
			scaleUpTo:       4,           // Create new groups R-2, R-3 (both >= partition=2, not protected)
			expectedRecreatedRevs: map[int]string{
				2: "revision-v2", // Should use new revision (ordinal >= partition)
				3: "revision-v2", // Should use new revision (ordinal >= partition)
			},
			expectedCurrentRevision: "revision-v1",
			expectedUpdateRevision:  "revision-v2",
		},
		{
			name:            "partition=2, recreate protected group should use historical revision",
			partition:       ptr.To[int32](2),
			initialReplicas: 3, // R-0, R-1, R-2 (R-0, R-1 < partition=2, R-2 >= partition=2)
			initialRevision: "revision-v1",
			existingGroups:  []int{0, 2}, // R-1 was deleted, needs to be recreated
			scaleUpTo:       4,           // Recreate R-1 and create R-3
			expectedRecreatedRevs: map[int]string{
				1: "revision-v1", // Should use historical revision (ordinal < partition, protected)
				3: "revision-v2", // Should use new revision (ordinal >= partition, new group)
			},
			expectedCurrentRevision: "revision-v1",
			expectedUpdateRevision:  "revision-v2",
		},
		{
			name:            "no partition, recreated group should use new revision",
			partition:       nil,
			initialReplicas: 3,
			initialRevision: "revision-v1",
			existingGroups:  []int{0, 1}, // R-2 was deleted
			scaleUpTo:       3,           // Recreate R-2
			expectedRecreatedRevs: map[int]string{
				2: "revision-v2", // Recreated group, should use new revision (no partition, no history)
			},
			expectedCurrentRevision: "revision-v1",
			expectedUpdateRevision:  "revision-v2",
		},
		{
			name:            "partition=3, recreate multiple groups below partition",
			partition:       ptr.To[int32](3),
			initialReplicas: 5,
			initialRevision: "revision-v1",
			existingGroups:  []int{0, 3, 4}, // R-1 and R-2 were deleted
			scaleUpTo:       5,              // Recreate R-1 and R-2
			expectedRecreatedRevs: map[int]string{
				1: "revision-v1", // Should use historical revision
				2: "revision-v1", // Should use historical revision
			},
			expectedCurrentRevision: "revision-v1",
			expectedUpdateRevision:  "revision-v2",
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

			msName := fmt.Sprintf("test-version-control-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](tt.scaleUpTo),
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
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition: tt.partition,
						},
					},
				},
				Status: workloadv1alpha1.ModelServingStatus{
					CurrentRevision: tt.initialRevision,
				},
			}

			// Create ControllerRevision for historical revision if partition is set
			// This simulates the scenario where a partition-protected group was deleted and its revision was recorded
			if tt.partition != nil {
				_, err := utils.CreateControllerRevision(context.Background(), kubeClient, ms, tt.initialRevision, ms.Spec.Template.Roles)
				assert.NoError(t, err, "Failed to create ControllerRevision for initial revision")
			}

			// Set up existing groups in store (simulating the state before scale up)
			existingGroupsList := make([]datastore.ServingGroup, 0, len(tt.existingGroups))
			for _, ordinal := range tt.existingGroups {
				groupName := utils.GenerateServingGroupName(msName, ordinal)
				controller.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, tt.initialRevision)
				existingGroupsList = append(existingGroupsList, datastore.ServingGroup{
					Name:     groupName,
					Revision: tt.initialRevision,
				})
			}

			// Create ModelServing in API server first
			_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(context.Background(), ms, metav1.CreateOptions{})
			assert.NoError(t, err)

			// Add to informer indexer so lister can find it
			err = controller.modelServingsInformer.GetIndexer().Add(ms)
			assert.NoError(t, err)

			// Call scaleUpServingGroups directly to test its behavior
			newRevision := "revision-v2"
			err = controller.scaleUpServingGroups(context.Background(), ms, existingGroupsList, int(tt.scaleUpTo), newRevision)
			assert.NoError(t, err)

			// Verify created/recreated groups have correct revisions
			for ordinal, expectedRevision := range tt.expectedRecreatedRevs {
				groupName := utils.GenerateServingGroupName(msName, ordinal)
				group := controller.store.GetServingGroup(utils.GetNamespaceName(ms), groupName)
				assert.NotNil(t, group, "Group at ordinal %d should exist", ordinal)
				if group != nil {
					assert.Equal(t, expectedRevision, group.Revision,
						"Group at ordinal %d should have revision %s, got %s", ordinal, expectedRevision, group.Revision)
				}
			}

			// Verify status revisions
			err = controller.UpdateModelServingStatus(ms, newRevision)
			assert.NoError(t, err)

			// Get updated ModelServing to check status
			updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Get(context.Background(), msName, metav1.GetOptions{})
			assert.NoError(t, err)
			if tt.expectedCurrentRevision != "" {
				assert.Equal(t, tt.expectedCurrentRevision, updatedMS.Status.CurrentRevision,
					"CurrentRevision should match expected")
			}
			if tt.expectedUpdateRevision != "" {
				assert.Equal(t, tt.expectedUpdateRevision, updatedMS.Status.UpdateRevision,
					"UpdateRevision should match expected")
			}
		})
	}
}

// TestScaleUpServingGroups_TemplateRecovery tests that for ordinal < partition:
// 1. Priority: use template from ControllerRevision (recovery scenario)
// 2. Fallback: use ms.Spec.Template.Roles if ControllerRevision doesn't exist (first startup scenario)
func TestScaleUpServingGroups_TemplateRecovery(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name                   string
		partition              int32
		ordinal                int
		hasControllerRevision  bool
		expectedTemplateSource string // "ControllerRevision" or "ms.Spec.Template.Roles"
		currentRevision        string
		initialTemplateRoles   []workloadv1alpha1.Role
		recoveryTemplateRoles  []workloadv1alpha1.Role // Template stored in ControllerRevision
		currentTemplateRoles   []workloadv1alpha1.Role // Current ms.Spec.Template.Roles
	}{
		{
			name:                   "recovery_with_controller_revision",
			partition:              3,
			ordinal:                1,
			hasControllerRevision:  true,
			expectedTemplateSource: "ControllerRevision",
			currentRevision:        "revision-v1",
			initialTemplateRoles: []workloadv1alpha1.Role{
				{
					Name:     "prefill",
					Replicas: ptr.To[int32](2),
				},
			},
			recoveryTemplateRoles: []workloadv1alpha1.Role{
				{
					Name:     "prefill",
					Replicas: ptr.To[int32](2),
				},
				{
					Name:     "decode",
					Replicas: ptr.To[int32](1),
				},
			},
			currentTemplateRoles: []workloadv1alpha1.Role{
				{
					Name:     "prefill",
					Replicas: ptr.To[int32](3),
				},
			},
		},
		{
			name:                   "first_startup_without_controller_revision",
			partition:              3,
			ordinal:                1,
			hasControllerRevision:  false,
			expectedTemplateSource: "ms.Spec.Template.Roles",
			currentRevision:        "revision-v1",
			initialTemplateRoles: []workloadv1alpha1.Role{
				{
					Name:     "prefill",
					Replicas: ptr.To[int32](2),
				},
			},
			currentTemplateRoles: []workloadv1alpha1.Role{
				{
					Name:     "prefill",
					Replicas: ptr.To[int32](2),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextfake)
			assert.NoError(t, err)

			// Use short name to avoid Kubernetes label length limits
			msName := fmt.Sprintf("test-tmpl-rec-%d", len(tt.name))
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To[int32](5),
					SchedulerName: "volcano",
					Template: workloadv1alpha1.ServingGroup{
						Roles: tt.currentTemplateRoles,
					},
					RecoveryPolicy: workloadv1alpha1.RoleRecreate,
					RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
						Type: workloadv1alpha1.ServingGroupRollingUpdate,
						RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
							Partition: ptr.To[int32](tt.partition),
						},
					},
				},
				Status: workloadv1alpha1.ModelServingStatus{
					CurrentRevision: tt.currentRevision,
				},
			}

			// Create ControllerRevision with recovery template if needed
			if tt.hasControllerRevision {
				_, err := utils.CreateControllerRevision(ctx, kubeClient, ms, tt.currentRevision, tt.recoveryTemplateRoles)
				assert.NoError(t, err, "Failed to create ControllerRevision")
			}

			// Verify ControllerRevision exists or doesn't exist as expected
			cr, _ := utils.GetControllerRevision(ctx, kubeClient, ms, tt.currentRevision)
			if tt.hasControllerRevision {
				assert.NotNil(t, cr, "ControllerRevision should exist")
				// Verify the template stored in ControllerRevision
				recoveredRoles, err := utils.GetRolesFromControllerRevision(cr)
				assert.NoError(t, err)
				assert.Equal(t, len(tt.recoveryTemplateRoles), len(recoveredRoles), "Recovered roles count should match")
				for i, expectedRole := range tt.recoveryTemplateRoles {
					assert.Equal(t, expectedRole.Name, recoveredRoles[i].Name, "Recovered role name should match")
				}
			} else {
				// ControllerRevision should not exist (GetControllerRevision returns nil, nil for NotFound)
				assert.Nil(t, cr, "ControllerRevision should be nil when not found")
			}

			// Test scaleUpServingGroups with missing ordinal
			// Create existing groups but skip the target ordinal
			existingGroups := []datastore.ServingGroup{}
			for i := 0; i < int(tt.partition); i++ {
				if i != tt.ordinal {
					existingGroups = append(existingGroups, datastore.ServingGroup{
						Name:     utils.GenerateServingGroupName(msName, i),
						Revision: tt.currentRevision,
					})
					controller.store.AddServingGroup(utils.GetNamespaceName(ms), i, tt.currentRevision)
				}
			}

			newRevision := "revision-v2"
			err = controller.scaleUpServingGroups(ctx, ms, existingGroups, int(tt.partition), newRevision)
			assert.NoError(t, err)

			// Verify the group was created with correct revision
			groupName := utils.GenerateServingGroupName(msName, tt.ordinal)
			group := controller.store.GetServingGroup(utils.GetNamespaceName(ms), groupName)
			assert.NotNil(t, group, "Group should be created")
			if group != nil {
				// For ordinal < partition, it should use CurrentRevision
				assert.Equal(t, tt.currentRevision, group.Revision,
					"Group at ordinal %d should use CurrentRevision %s", tt.ordinal, tt.currentRevision)
			}

			// Verify which template was used by checking the created pods
			// Get pods for this group
			pods, err := kubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s", workloadv1alpha1.GroupNameLabelKey, groupName),
			})
			assert.NoError(t, err)

			if len(pods.Items) > 0 && tt.hasControllerRevision {
				// Check which roles exist in the pods to determine which template was used
				// If recovery template has "decode" role and current template doesn't,
				// pods with decode role indicate ControllerRevision template was used
				recoveryHasDecode := false
				currentHasDecode := false
				for _, role := range tt.recoveryTemplateRoles {
					if role.Name == "decode" {
						recoveryHasDecode = true
						break
					}
				}
				for _, role := range tt.currentTemplateRoles {
					if role.Name == "decode" {
						currentHasDecode = true
						break
					}
				}

				if recoveryHasDecode && !currentHasDecode {
					// Count roles by checking pod labels
					roleNames := make(map[string]bool)
					for _, pod := range pods.Items {
						roleName := pod.Labels[workloadv1alpha1.RoleLabelKey]
						if roleName != "" {
							roleNames[roleName] = true
						}
					}

					// If recovery template has decode but current doesn't, and we see decode pods,
					// it means ControllerRevision template was used
					if tt.expectedTemplateSource == "ControllerRevision" {
						assert.True(t, roleNames["decode"], "Should have decode role pods if ControllerRevision template was used")
					}
				}
			}
		})
	}
}

// TestUpdateModelServingStatusRevisionFields tests the CurrentRevision and UpdateRevision logic
// following StatefulSet's behavior
func TestUpdateModelServingStatusRevisionFields(t *testing.T) {
	tests := []struct {
		name                    string
		existingGroups          map[int]string // ordinal -> revision
		statusCurrentRevision   string         // Existing CurrentRevision in status
		newRevision             string         // New revision being applied
		expectedCurrentRevision string
		expectedUpdateRevision  string
		description             string
	}{
		{
			name: "no existing CurrentRevision, compute from groups",
			existingGroups: map[int]string{
				0: "revision-v1",
				1: "revision-v1",
				2: "revision-v1",
			},
			statusCurrentRevision:   "",
			newRevision:             "revision-v2",
			expectedCurrentRevision: "revision-v1", // Most common non-updated revision
			expectedUpdateRevision:  "revision-v2",
			description:             "When Status.CurrentRevision is empty, should compute from current groups",
		},
		{
			name: "existing CurrentRevision is valid, should keep it",
			existingGroups: map[int]string{
				0: "revision-v1",
				1: "revision-v1",
				2: "revision-v2", // Updated
			},
			statusCurrentRevision:   "revision-v1",
			newRevision:             "revision-v2",
			expectedCurrentRevision: "revision-v1", // Should keep existing CurrentRevision
			expectedUpdateRevision:  "revision-v2",
			description:             "When Status.CurrentRevision exists and is still valid, should keep it",
		},
		{
			name: "all groups updated, CurrentRevision should equal UpdateRevision",
			existingGroups: map[int]string{
				0: "revision-v2", // All updated
				1: "revision-v2",
				2: "revision-v2",
			},
			statusCurrentRevision:   "revision-v1", // Invalid, not used by any group
			newRevision:             "revision-v2",
			expectedCurrentRevision: "revision-v2", // Should equal UpdateRevision when all updated
			expectedUpdateRevision:  "revision-v2",
			description:             "When all groups are updated, CurrentRevision should equal UpdateRevision (invalid CurrentRevision should be recomputed)",
		},
		{
			name: "multiple old revisions, should use most common",
			existingGroups: map[int]string{
				0: "revision-v1",
				1: "revision-v1",
				2: "revision-v0", // Less common
				3: "revision-v2", // Updated
			},
			statusCurrentRevision:   "",
			newRevision:             "revision-v2",
			expectedCurrentRevision: "revision-v1", // Most common (2 groups)
			expectedUpdateRevision:  "revision-v2",
			description:             "When multiple old revisions exist, should use the most common one",
		},
		{
			name:                    "no groups exist",
			existingGroups:          map[int]string{},
			statusCurrentRevision:   "",
			newRevision:             "revision-v1",
			expectedCurrentRevision: "revision-v1", // Should equal UpdateRevision
			expectedUpdateRevision:  "revision-v1",
			description:             "When no groups exist, CurrentRevision should equal UpdateRevision",
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

			msName := fmt.Sprintf("test-revision-fields-%d", idx)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas:      ptr.To(int32(len(tt.existingGroups))),
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
				Status: workloadv1alpha1.ModelServingStatus{
					CurrentRevision: tt.statusCurrentRevision,
				},
			}

			// Create ModelServing in API server
			_, err = kthenaClient.WorkloadV1alpha1().ModelServings("default").Create(context.Background(), ms, metav1.CreateOptions{})
			assert.NoError(t, err)

			// Add to informer indexer so lister can find it
			err = controller.modelServingsInformer.GetIndexer().Add(ms)
			assert.NoError(t, err)

			// Create servingGroups with specified revisions
			for ordinal, revision := range tt.existingGroups {
				controller.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, revision)
				// Mark groups as Running to simulate real scenario
				groupName := utils.GenerateServingGroupName(msName, ordinal)
				controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), groupName, datastore.ServingGroupRunning)
			}

			// Call UpdateModelServingStatus
			err = controller.UpdateModelServingStatus(ms, tt.newRevision)
			assert.NoError(t, err)

			// Get updated ModelServing to check status
			updatedMS, err := kthenaClient.WorkloadV1alpha1().ModelServings("default").Get(context.Background(), msName, metav1.GetOptions{})
			assert.NoError(t, err)

			assert.Equal(t, tt.expectedCurrentRevision, updatedMS.Status.CurrentRevision,
				"CurrentRevision: %s", tt.description)
			assert.Equal(t, tt.expectedUpdateRevision, updatedMS.Status.UpdateRevision,
				"UpdateRevision: %s", tt.description)
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

			msName := fmt.Sprintf("test-role-scaledown-%d", idx)
			groupName := utils.GenerateServingGroupName(msName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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

			msName := fmt.Sprintf("test-role-priority-scaledown-%d", idx)
			groupName := utils.GenerateServingGroupName(msName, 0)
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      msName,
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
							workloadv1alpha1.ModelServingNameLabelKey: msName,
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

// TestScaleDownRolesRunningStatusDeprioritized tests that roles with RoleRunning status
// are deprioritized (deleted last) during scale-down operations compared to roles
// in RoleCreating or RoleNotFound states.
func TestScaleDownRolesRunningStatusDeprioritized(t *testing.T) {
	tests := []struct {
		name                   string
		existingIndices        []int
		roleStatuses           map[int]datastore.RoleStatus
		expectedCount          int
		expectedRemainingNames []string
		description            string
	}{
		{
			name:            "running_roles_kept_over_creating_roles",
			existingIndices: []int{0, 1, 2},
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleCreating, // Not ready - should be deleted first
				2: datastore.RoleRunning,
			},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-2"},
			description:            "RoleCreating (index 1) should be deleted before RoleRunning roles",
		},
		{
			name:            "running_roles_kept_over_notfound_roles",
			existingIndices: []int{0, 1, 2},
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleNotFound, // Not ready - should be deleted first
				2: datastore.RoleRunning,
			},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-2"},
			description:            "RoleNotFound (index 1) should be deleted before RoleRunning roles",
		},
		{
			name:            "multiple_running_roles_higher_index_deleted_first",
			existingIndices: []int{0, 1, 2, 3},
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleRunning,
				1: datastore.RoleRunning,
				2: datastore.RoleRunning,
				3: datastore.RoleRunning,
			},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-0", "prefill-1"},
			description:            "Among all RoleRunning roles, higher indices (3, 2) should be deleted first",
		},
		{
			name:            "mixed_statuses_not_ready_deleted_first",
			existingIndices: []int{0, 1, 2, 3},
			roleStatuses: map[int]datastore.RoleStatus{
				0: datastore.RoleCreating, // Not ready - delete first
				1: datastore.RoleRunning,
				2: datastore.RoleNotFound, // Not ready - delete first
				3: datastore.RoleRunning,
			},
			expectedCount:          2,
			expectedRemainingNames: []string{"prefill-1", "prefill-3"},
			description:            "Both not-ready roles (0, 2) should be deleted, keeping both Running roles (1, 3)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup fake clients
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
			assert.NoError(t, err)

			// Create ModelServing
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "prefill",
								Replicas: ptr.To[int32](int32(tt.expectedCount)),
							},
						},
					},
				},
			}

			groupName := "test-ms-0"
			nsn := utils.GetNamespaceName(ms)

			// Add serving group
			controller.store.AddServingGroup(nsn, 0, "test-revision")

			// Create roles with specified statuses
			var roleList []datastore.Role
			for _, idx := range tt.existingIndices {
				roleID := fmt.Sprintf("prefill-%d", idx)
				controller.store.AddRole(nsn, groupName, "prefill", roleID, "test-revision")
				controller.store.UpdateRoleStatus(nsn, groupName, "prefill", roleID, tt.roleStatuses[idx])
				roleList = append(roleList, datastore.Role{
					Name:     roleID, // In datastore.Role, Name holds the roleID
					Revision: "test-revision",
					Status:   tt.roleStatuses[idx],
				})
			}

			// Create mock pods for each role
			podIndexer := controller.podsInformer.GetIndexer()
			for _, idx := range tt.existingIndices {
				roleID := fmt.Sprintf("prefill-%d", idx)
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      fmt.Sprintf("pod-%s", roleID),
						Labels: map[string]string{
							workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
							workloadv1alpha1.GroupNameLabelKey:        groupName,
							workloadv1alpha1.RoleLabelKey:             "prefill",
							workloadv1alpha1.RoleIDKey:                roleID,
							workloadv1alpha1.EntryLabelKey:            utils.Entry,
						},
						Annotations: map[string]string{
							PodDeletionCostAnnotation: "0",
						},
					},
				}
				err := podIndexer.Add(pod)
				assert.NoError(t, err)
			}

			// Track which roles are deleted (targeted for deletion)
			var deletedRoleIDs []string
			patch := gomonkey.NewPatches()
			patch.ApplyMethod(reflect.TypeOf(controller), "DeleteRole", func(_ *ModelServingController, ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName, roleName, roleID string) {
				deletedRoleIDs = append(deletedRoleIDs, roleID)
			})
			defer patch.Reset()

			// Target role (using first role's spec)
			targetRole := workloadv1alpha1.Role{
				Name:     "prefill",
				Replicas: ptr.To[int32](int32(tt.expectedCount)),
			}

			// Run scaleDownRoles
			controller.scaleDownRoles(context.Background(), ms, groupName, targetRole, roleList, tt.expectedCount)

			// Calculate expected deleted roles (all roles except expectedRemainingNames)
			allRoleIDs := make(map[string]bool)
			for _, idx := range tt.existingIndices {
				allRoleIDs[fmt.Sprintf("prefill-%d", idx)] = true
			}
			for _, remaining := range tt.expectedRemainingNames {
				delete(allRoleIDs, remaining)
			}
			var expectedDeletedRoleIDs []string
			for id := range allRoleIDs {
				expectedDeletedRoleIDs = append(expectedDeletedRoleIDs, id)
			}

			// Verify correct number of deletions
			numToDelete := len(tt.existingIndices) - tt.expectedCount
			assert.Equal(t, numToDelete, len(deletedRoleIDs),
				"Expected %d deletions, got %d", numToDelete, len(deletedRoleIDs))

			// Verify the correct roles were targeted for deletion
			sort.Strings(deletedRoleIDs)
			sort.Strings(expectedDeletedRoleIDs)
			assert.Equal(t, expectedDeletedRoleIDs, deletedRoleIDs,
				"%s: Expected deleted roles %v, got %v", tt.description, expectedDeletedRoleIDs, deletedRoleIDs)
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
		servingGroupStatus   datastore.ServingGroupStatus
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
			servingGroupStatus:   datastore.ServingGroupRunning,
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
			servingGroupStatus:   datastore.ServingGroupRunning,
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
			servingGroupStatus:   datastore.ServingGroupRunning,
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
			servingGroupStatus:   datastore.ServingGroupRunning,
			expectedServiceCount: 0,
			expectServiceCreated: false,
		},
		{
			name: "skip creating service for deleting serving group",
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
								WorkerReplicas: 1,
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
			existingServices:     []*corev1.Service{},
			servingGroupStatus:   datastore.ServingGroupDeleting,
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
			err = controller.store.UpdateServingGroupStatus(utils.GetNamespaceName(tt.modelServing), groupName, tt.servingGroupStatus)
			assert.NoError(t, err)

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

// TestSyncAllWithFailedPods tests that failed pods at startup are properly handled
// after initial sync completes. This tests the fix for the bug where failed pods
// were silently ignored during controller startup.
func TestSyncAllWithFailedPods(t *testing.T) {
	ns := "default"
	msName := "test-ms"
	groupName := "test-ms-0"
	roleName := "prefill"
	roleID := "prefill-0"
	revision := "hash123"

	// Setup fake clients
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset()

	// Create controller first to get access to informers
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	assert.NoError(t, err)

	// Create the ModelServing resource with a UID for owner reference
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      msName,
			UID:       "test-ms-uid-123",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     roleName,
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
							},
						},
					},
				},
			},
		},
	}

	// Create failed pod - this simulates a pod that is already in Failed state
	// when the controller starts (e.g., after controller restart)
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-failed",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                roleID,
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed, // Pod is in Failed state
		},
	}

	// Add resources directly to informer indexers for listers to find them
	err = controller.podsInformer.GetIndexer().Add(failedPod)
	assert.NoError(t, err)
	err = controller.modelServingsInformer.GetIndexer().Add(ms)
	assert.NoError(t, err)

	// Verify initialSync is false before syncAll
	assert.False(t, controller.initialSync, "initialSync should be false before syncAll")

	// Call syncAll - this should handle the failed pod properly after the fix
	controller.syncAll()

	// Verify initialSync is true after syncAll
	assert.True(t, controller.initialSync, "initialSync should be true after syncAll")

	// Verify the failed pod was added to graceMap (this happens in handleErrorPod)
	_, existsInGraceMap := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      failedPod.Name,
	})
	assert.True(t, existsInGraceMap, "Failed pod should be added to graceMap after syncAll processes it")
}

// TestSyncAllWithContainerRestartedPods tests that pods with restarted containers
// at startup are properly handled after initial sync completes.
func TestSyncAllWithContainerRestartedPods(t *testing.T) {
	ns := "default"
	msName := "test-ms"
	groupName := "test-ms-0"
	roleName := "prefill"
	roleID := "prefill-0"
	revision := "hash123"

	// Setup fake clients
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset()

	// Create controller first to get access to informers
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	assert.NoError(t, err)

	// Create the ModelServing resource
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      msName,
			UID:       "test-ms-uid-456",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     roleName,
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
							},
						},
					},
				},
			},
		},
	}

	// Create pod with restarted container - this simulates a CrashLoopBackOff scenario
	restartedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-restarted",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                roleID,
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					RestartCount: 5, // Container has restarted multiple times
				},
			},
		},
	}

	// Add resources directly to informer indexers for listers to find them
	err = controller.podsInformer.GetIndexer().Add(restartedPod)
	assert.NoError(t, err)
	err = controller.modelServingsInformer.GetIndexer().Add(ms)
	assert.NoError(t, err)

	// Call syncAll
	controller.syncAll()

	// Verify the restarted container pod was added to graceMap
	_, existsInGraceMap := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      restartedPod.Name,
	})
	assert.True(t, existsInGraceMap, "Pod with restarted container should be added to graceMap after syncAll")
}

// TestSyncAllWithMixedPods tests that syncAll properly handles a mix of
// running, failed, and restarted pods at startup.
func TestSyncAllWithMixedPods(t *testing.T) {
	ns := "default"
	msName := "test-ms"
	groupName := "test-ms-0"
	roleName := "prefill"
	revision := "hash123"

	// Setup fake clients
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset()

	// Create controller first to get access to informers
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	assert.NoError(t, err)

	// Create the ModelServing resource
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      msName,
			UID:       "test-ms-uid-789",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     roleName,
						Replicas: ptr.To[int32](3),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
							},
						},
					},
				},
			},
		},
	}

	// Create a running pod (healthy)
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-running",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                "prefill-0",
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	// Create a failed pod
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-failed",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                "prefill-1",
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}

	// Create a pod with restarted container
	restartedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-restarted",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                "prefill-2",
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:         "main",
					RestartCount: 3,
				},
			},
		},
	}

	// Add resources directly to informer indexers for listers to find them
	err = controller.podsInformer.GetIndexer().Add(runningPod)
	assert.NoError(t, err)
	err = controller.podsInformer.GetIndexer().Add(failedPod)
	assert.NoError(t, err)
	err = controller.podsInformer.GetIndexer().Add(restartedPod)
	assert.NoError(t, err)
	err = controller.modelServingsInformer.GetIndexer().Add(ms)
	assert.NoError(t, err)

	// Call syncAll
	controller.syncAll()

	// Verify initialSync is true
	assert.True(t, controller.initialSync, "initialSync should be true after syncAll")

	// Verify running pod is NOT in graceMap (it's healthy)
	_, runningInGraceMap := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      runningPod.Name,
	})
	assert.False(t, runningInGraceMap, "Running pod should NOT be in graceMap")

	// Verify failed pod IS in graceMap
	_, failedInGraceMap := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      failedPod.Name,
	})
	assert.True(t, failedInGraceMap, "Failed pod should be in graceMap")

	// Verify restarted pod IS in graceMap
	_, restartedInGraceMap := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      restartedPod.Name,
	})
	assert.True(t, restartedInGraceMap, "Restarted pod should be in graceMap")

	// Verify all pods have their serving groups tracked
	servingGroups, err := controller.store.GetServingGroupByModelServing(types.NamespacedName{
		Namespace: ns,
		Name:      msName,
	})
	assert.NoError(t, err)
	assert.NotEmpty(t, servingGroups, "ServingGroups should exist in store")
}

// TestSyncAllBeforeFixBehavior documents the previous buggy behavior where
// failed pods at startup were silently ignored when initialSync was false.
// This test verifies that the fix properly addresses this issue.
func TestSyncAllBeforeFixBehavior(t *testing.T) {
	// This test verifies that before the fix, the updatePod function would
	// return early for failed pods when initialSync=false, causing them to
	// never be processed. After the fix, syncAll defers failed pods and
	// processes them after setting initialSync=true.

	ns := "default"
	msName := "test-ms"
	groupName := "test-ms-0"
	roleName := "prefill"
	roleID := "prefill-0"
	revision := "hash123"

	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset()

	// Create controller first to get access to informers
	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	assert.NoError(t, err)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      msName,
			UID:       "test-ms-uid-abc",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     roleName,
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
							},
						},
					},
				},
			},
		},
	}

	// Create a failed pod
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      "test-pod-failed",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: msName,
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             roleName,
				workloadv1alpha1.RoleIDKey:                roleID,
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: workloadv1alpha1.GroupVersion.String(),
					Kind:       "ModelServing",
					Name:       msName,
					UID:        ms.UID,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
		},
	}

	// Add resources directly to informer indexers for listers to find them
	err = controller.podsInformer.GetIndexer().Add(failedPod)
	assert.NoError(t, err)
	err = controller.modelServingsInformer.GetIndexer().Add(ms)
	assert.NoError(t, err)

	// Verify before syncAll, initialSync is false
	assert.False(t, controller.initialSync)

	// The key test: Before the fix, calling addPod directly with initialSync=false
	// for a failed pod would return early without processing.
	// After the fix, syncAll defers these pods and processes them correctly.

	// Call syncAll which should now properly handle the failed pod
	controller.syncAll()

	// After the fix, the failed pod should be in graceMap
	// This proves the fix works - before the fix, this would be empty
	_, exists := controller.graceMap.Load(types.NamespacedName{
		Namespace: ns,
		Name:      failedPod.Name,
	})
	assert.True(t, exists,
		"After fix: Failed pod should be in graceMap. "+
			"Before fix: This would be false because updatePod returned early when initialSync=false")
}

// TestUpdateModelServingWithNilGangPolicy tests that updateModelServing does not panic
// when NetworkTopology is removed and GangPolicy is nil.
func TestUpdateModelServingWithNilGangPolicy(t *testing.T) {
	tests := []struct {
		name  string
		oldMS *workloadv1alpha1.ModelServing
		newMS *workloadv1alpha1.ModelServing
	}{
		{
			name: "remove NetworkTopology with nil GangPolicy should not panic",
			oldMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-ms",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						NetworkTopology: &workloadv1alpha1.NetworkTopology{},
						GangPolicy:      nil, // GangPolicy is nil
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "inference",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "model", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
			newMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-ms",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						NetworkTopology: nil, // NetworkTopology removed
						GangPolicy:      nil, // GangPolicy is still nil
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "inference",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "model", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "remove NetworkTopology with GangPolicy set should not panic",
			oldMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-ms-2",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						NetworkTopology: &workloadv1alpha1.NetworkTopology{},
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{"inference": 1},
						},
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "inference",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "model", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
			newMS: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "test-ms-2",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						NetworkTopology: nil, // NetworkTopology removed
						GangPolicy: &workloadv1alpha1.GangPolicy{
							MinRoleReplicas: map[string]int32{"inference": 1},
						},
						Roles: []workloadv1alpha1.Role{
							{
								Name:     "inference",
								Replicas: ptr.To[int32](1),
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{
											{Name: "model", Image: "nginx"},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeClient := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextClient := apiextfake.NewSimpleClientset()

			controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
			assert.NoError(t, err)

			// This should not panic - before fix it would panic with nil pointer dereference
			assert.NotPanics(t, func() {
				controller.updateModelServing(tt.oldMS, tt.newMS)
			}, "updateModelServing should not panic when GangPolicy is nil")
		})
	}
}

func TestDeleteRoleRollbackOnFailure(t *testing.T) {
	tests := []struct {
		name                 string
		initialRoleStatus    datastore.RoleStatus
		podDeletionError     error
		serviceDeletionError error
		expectedFinalStatus  datastore.RoleStatus
		expectEnqueueCalled  bool
		description          string
	}{
		{
			name:                 "pod_deletion_fails_with_rollback",
			initialRoleStatus:    datastore.RoleRunning,
			podDeletionError:     fmt.Errorf("failed to delete pods"),
			serviceDeletionError: nil,
			expectedFinalStatus:  datastore.RoleRunning,
			expectEnqueueCalled:  true,
			description:          "failed to delete pods, should rollback to original status and re-enqueue",
		},
		{
			name:                 "service_deletion_fails_with_rollback",
			initialRoleStatus:    datastore.RoleCreating,
			podDeletionError:     nil,
			serviceDeletionError: fmt.Errorf("failed to delete services"),
			expectedFinalStatus:  datastore.RoleCreating,
			expectEnqueueCalled:  true,
			description:          "failed to delete services, should rollback to original status and re-enqueue",
		},
		{
			name:                 "both_operations_success_no_rollback",
			initialRoleStatus:    datastore.RoleRunning,
			podDeletionError:     nil,
			serviceDeletionError: nil,
			expectedFinalStatus:  datastore.RoleDeleting,
			expectEnqueueCalled:  false,
			description:          "are deletions succeed, no rollback needed",
		},
		{
			name:                 "pod_api_error_no_rollback",
			initialRoleStatus:    datastore.RoleNotFound,
			podDeletionError:     apierrors.NewInternalError(fmt.Errorf("internal error")),
			serviceDeletionError: nil,
			expectedFinalStatus:  datastore.RoleNotFound,
			expectEnqueueCalled:  true,
			description:          "pod API error, should re-enqueue",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

			// Create informer factories
			kubeInformerFactory := informers.NewSharedInformerFactory(client, 0)
			kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

			// Create controller
			controller, err := NewModelServingController(client, kthenaClient, volcanoClient, apiextfake)
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

			if tt.podDeletionError != nil {
				client.PrependReactor("delete-collection", "pods", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, tt.podDeletionError
				})
			}

			if tt.serviceDeletionError != nil {
				client.PrependReactor("delete", "services", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, tt.serviceDeletionError
				})
			}

			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model-serving",
					Namespace: "default",
				},
			}

			groupName := "test-group"
			roleName := "test-role"
			roleID := "test-role-id"

			nsn := utils.GetNamespaceName(ms)
			controller.store.AddRole(nsn, groupName, roleName, roleID, "test-revision")
			controller.store.UpdateRoleStatus(nsn, groupName, roleName, roleID, tt.initialRoleStatus)

			initialStatus := controller.store.GetRoleStatus(nsn, groupName, roleName, roleID)
			assert.Equal(t, tt.initialRoleStatus, initialStatus)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						workloadv1alpha1.GroupNameLabelKey: groupName,
						workloadv1alpha1.RoleLabelKey:      roleName,
						workloadv1alpha1.RoleIDKey:         roleID,
					},
				},
			}

			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
					Labels: map[string]string{
						workloadv1alpha1.GroupNameLabelKey: groupName,
						workloadv1alpha1.RoleLabelKey:      roleName,
						workloadv1alpha1.RoleIDKey:         roleID,
					},
				},
			}

			_, err = client.CoreV1().Pods("default").Create(context.TODO(), pod, metav1.CreateOptions{})
			assert.NoError(t, err)

			_, err = client.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			assert.NoError(t, err)
			err = controller.servicesInformer.GetIndexer().Add(service)
			assert.NoError(t, err)

			queue := []string{}
			patch := gomonkey.NewPatches()
			patch.ApplyPrivateMethod(reflect.TypeOf(&ModelServingController{}), "enqueueModelServing", func(ms *workloadv1alpha1.ModelServing, duration time.Duration) {
				queue = append(queue, ms.Name)
			})
			defer patch.Reset()

			controller.DeleteRole(context.Background(), ms, groupName, roleName, roleID)

			finalStatus := controller.store.GetRoleStatus(nsn, groupName, roleName, roleID)
			assert.Equal(t, tt.expectedFinalStatus, finalStatus)

			queueLen := len(queue)
			if tt.expectEnqueueCalled {
				assert.True(t, queueLen > 0, "should enqueue")
			} else {
				assert.Equal(t, 0, queueLen, "should not enqueue")
			}
		})
	}
}

// TestHandleReadyPodRoleStatusUpdate tests that handleReadyPod correctly updates
// role status to Running when all pods in the role are ready.
// This tests the fix for the bug where role status was never set to RoleRunning,
// which broke the scale-down priority protection for healthy roles.
func TestHandleReadyPodRoleStatusUpdate(t *testing.T) {
	ns := "default"
	msName := "test-ms"
	groupName := "test-ms-0"
	roleName := "prefill"
	roleID := "prefill-0"
	revision := "hash123"

	tests := []struct {
		description    string
		workerReplicas int32
		existingPods   []struct {
			name     string
			isReady  bool
			isEntry  bool
			workerID int // 0 for entry, 1+ for workers
		}
		newPodIsEntry      bool
		newPodWorkerID     int
		initialRoleStatus  datastore.RoleStatus
		expectedRoleStatus datastore.RoleStatus
	}{
		{
			description:    "single entry pod becomes ready - role should transition to Running",
			workerReplicas: 0,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{},
			newPodIsEntry:      true,
			newPodWorkerID:     0,
			initialRoleStatus:  datastore.RoleCreating,
			expectedRoleStatus: datastore.RoleRunning,
		},
		{
			description:    "entry pod ready but workers not ready - role should stay Creating",
			workerReplicas: 2,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{
				{name: groupName + "-" + roleName + "-1", isReady: false, isEntry: false, workerID: 1},
				{name: groupName + "-" + roleName + "-2", isReady: false, isEntry: false, workerID: 2},
			},
			newPodIsEntry:      true,
			newPodWorkerID:     0,
			initialRoleStatus:  datastore.RoleCreating,
			expectedRoleStatus: datastore.RoleCreating,
		},
		{
			description:    "all pods ready including new worker - role should transition to Running",
			workerReplicas: 2,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{
				{name: groupName + "-" + roleName + "-0", isReady: true, isEntry: true, workerID: 0},
				{name: groupName + "-" + roleName + "-1", isReady: true, isEntry: false, workerID: 1},
			},
			newPodIsEntry:      false,
			newPodWorkerID:     2,
			initialRoleStatus:  datastore.RoleCreating,
			expectedRoleStatus: datastore.RoleRunning,
		},
		{
			description:    "last worker becomes ready - role should transition to Running",
			workerReplicas: 1,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{
				{name: groupName + "-" + roleName + "-0", isReady: true, isEntry: true, workerID: 0},
			},
			newPodIsEntry:      false,
			newPodWorkerID:     1,
			initialRoleStatus:  datastore.RoleCreating,
			expectedRoleStatus: datastore.RoleRunning,
		},
		{
			description:    "role already Running - should stay Running",
			workerReplicas: 0,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{},
			newPodIsEntry:      true,
			newPodWorkerID:     0,
			initialRoleStatus:  datastore.RoleRunning,
			expectedRoleStatus: datastore.RoleRunning,
		},
		{
			description:    "role in Deleting state - should not change to Running",
			workerReplicas: 0,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{},
			newPodIsEntry:      true,
			newPodWorkerID:     0,
			initialRoleStatus:  datastore.RoleDeleting,
			expectedRoleStatus: datastore.RoleDeleting,
		},
		{
			description:    "one of multiple workers still not ready - role should stay Creating",
			workerReplicas: 3,
			existingPods: []struct {
				name     string
				isReady  bool
				isEntry  bool
				workerID int
			}{
				{name: groupName + "-" + roleName + "-0", isReady: true, isEntry: true, workerID: 0},
				{name: groupName + "-" + roleName + "-1", isReady: true, isEntry: false, workerID: 1},
				{name: groupName + "-" + roleName + "-3", isReady: false, isEntry: false, workerID: 3}, // not ready
			},
			newPodIsEntry:      false,
			newPodWorkerID:     2,
			initialRoleStatus:  datastore.RoleCreating,
			expectedRoleStatus: datastore.RoleCreating,
		},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			// Setup fake clients
			kubeClient := kubefake.NewSimpleClientset()

			// Create informer factory
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

			// Create store and add initial role status
			store := datastore.New()

			// Create ModelServing
			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      msName,
					UID:       "test-ms-uid",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: ptr.To[int32](1),
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{
								Name:           roleName,
								Replicas:       ptr.To[int32](1),
								WorkerReplicas: tt.workerReplicas,
								EntryTemplate: workloadv1alpha1.PodTemplateSpec{
									Spec: corev1.PodSpec{
										Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
									},
								},
							},
						},
					},
				},
			}

			// Create controller with workqueue
			controller := &ModelServingController{
				kubeClientSet:    kubeClient,
				podsInformer:     podInformer.Informer(),
				podsLister:       podInformer.Lister(),
				servicesInformer: serviceInformer.Informer(),
				servicesLister:   serviceInformer.Lister(),
				store:            store,
				workqueue:        workqueue.NewTypedRateLimitingQueue[string](workqueue.DefaultTypedControllerRateLimiter[string]()),
			}

			// Start informers
			stop := make(chan struct{})
			defer close(stop)
			kubeInformerFactory.Start(stop)
			kubeInformerFactory.WaitForCacheSync(stop)

			podIndexer := podInformer.Informer().GetIndexer()

			// Add existing pods to indexer
			for _, existingPod := range tt.existingPods {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: ns,
						Name:      existingPod.name,
						Labels: map[string]string{
							workloadv1alpha1.ModelServingNameLabelKey: msName,
							workloadv1alpha1.GroupNameLabelKey:        groupName,
							workloadv1alpha1.RoleLabelKey:             roleName,
							workloadv1alpha1.RoleIDKey:                roleID,
							workloadv1alpha1.RevisionLabelKey:         revision,
						},
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: workloadv1alpha1.GroupVersion.String(),
								Kind:       "ModelServing",
								Name:       msName,
								UID:        ms.UID,
							},
						},
					},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
					},
				}
				if existingPod.isEntry {
					pod.Labels[workloadv1alpha1.EntryLabelKey] = utils.Entry
				}
				if existingPod.isReady {
					pod.Status.Conditions = []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					}
				}
				err := podIndexer.Add(pod)
				assert.NoError(t, err)

				// Add to store's running pods
				store.AddRunningPodToServingGroup(
					types.NamespacedName{Namespace: ns, Name: msName},
					groupName, pod.Name, revision, roleName, roleID,
				)
			}

			// Set initial role status if role exists in store
			if tt.initialRoleStatus != datastore.RoleNotFound {
				// Ensure the role exists in the store first
				store.AddServingGroupAndRole(
					types.NamespacedName{Namespace: ns, Name: msName},
					groupName, revision, roleName, roleID,
				)
				err = store.UpdateRoleStatus(
					types.NamespacedName{Namespace: ns, Name: msName},
					groupName, roleName, roleID, tt.initialRoleStatus,
				)
				assert.NoError(t, err)
			}

			// Create the new pod that triggers handleReadyPod
			var newPodName string
			if tt.newPodIsEntry {
				newPodName = groupName + "-" + roleName + "-0"
			} else {
				newPodName = fmt.Sprintf("%s-%s-%d", groupName, roleName, tt.newPodWorkerID)
			}

			newPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: ns,
					Name:      newPodName,
					Labels: map[string]string{
						workloadv1alpha1.ModelServingNameLabelKey: msName,
						workloadv1alpha1.GroupNameLabelKey:        groupName,
						workloadv1alpha1.RoleLabelKey:             roleName,
						workloadv1alpha1.RoleIDKey:                roleID,
						workloadv1alpha1.RevisionLabelKey:         revision,
					},
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: workloadv1alpha1.GroupVersion.String(),
							Kind:       "ModelServing",
							Name:       msName,
							UID:        ms.UID,
						},
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			}
			if tt.newPodIsEntry {
				newPod.Labels[workloadv1alpha1.EntryLabelKey] = utils.Entry
			}

			// Add new pod to indexer
			err = podIndexer.Add(newPod)
			assert.NoError(t, err)

			// Call handleReadyPod
			err = controller.handleReadyPod(ms, groupName, newPod)
			assert.NoError(t, err)

			// Verify role status
			actualRoleStatus := store.GetRoleStatus(
				types.NamespacedName{Namespace: ns, Name: msName},
				groupName, roleName, roleID,
			)
			assert.Equal(t, tt.expectedRoleStatus, actualRoleStatus,
				"Role status mismatch: expected %s, got %s", tt.expectedRoleStatus, actualRoleStatus)
		})
	}
}

func TestDeleteServingGroupRollbackOnFailure(t *testing.T) {
	tests := []struct {
		name                  string
		initialSgStatus       datastore.ServingGroupStatus
		podGroupDeletionError error
		podDeletionError      error
		serviceDeletionError  error
		expectedFinalStatus   datastore.ServingGroupStatus
		expectEnqueueCalled   bool
		description           string
	}{
		{
			name:                  "pod_group_deletion_fails_with_rollback",
			initialSgStatus:       datastore.ServingGroupRunning,
			podGroupDeletionError: fmt.Errorf("failed to delete pod group"),
			podDeletionError:      nil,
			serviceDeletionError:  nil,
			expectedFinalStatus:   datastore.ServingGroupRunning,
			expectEnqueueCalled:   true,
			description:           "failed to delete pod group, should rollback to original status and re-enqueue",
		},
		{
			name:                  "pod_deletion_fails_with_rollback",
			initialSgStatus:       datastore.ServingGroupCreating,
			podGroupDeletionError: nil,
			podDeletionError:      fmt.Errorf("failed to delete pods"),
			serviceDeletionError:  nil,
			expectedFinalStatus:   datastore.ServingGroupCreating,
			expectEnqueueCalled:   true,
			description:           "failed to delete pods, should rollback to original status and re-enqueue",
		},
		{
			name:                  "service_deletion_fails_with_rollback",
			initialSgStatus:       datastore.ServingGroupRunning,
			podGroupDeletionError: nil,
			podDeletionError:      nil,
			serviceDeletionError:  fmt.Errorf("failed to delete services"),
			expectedFinalStatus:   datastore.ServingGroupRunning,
			expectEnqueueCalled:   true,
			description:           "failed to delete services, should rollback to original status and re-enqueue",
		},
		{
			name:                  "all_operations_success_no_rollback",
			initialSgStatus:       datastore.ServingGroupRunning,
			podGroupDeletionError: nil,
			podDeletionError:      nil,
			serviceDeletionError:  nil,
			expectedFinalStatus:   datastore.ServingGroupDeleting,
			expectEnqueueCalled:   false,
			description:           "all deletions succeed, no rollback needed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := kubefake.NewSimpleClientset()
			kthenaClient := kthenafake.NewSimpleClientset()
			volcanoClient := volcanofake.NewSimpleClientset()
			apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

			// Create informer factories
			kubeInformerFactory := informers.NewSharedInformerFactory(client, 0)
			kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

			// Create controller
			controller, err := NewModelServingController(client, kthenaClient, volcanoClient, apiextfake)
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

			// Mock PodGroup deletion behavior using gomonkey
			var patch *gomonkey.Patches
			if tt.podGroupDeletionError != nil {
				patch = gomonkey.ApplyMethod(reflect.TypeOf(controller.podGroupManager), "DeletePodGroup", func(_ *podgroupmanager.Manager, ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error {
					return tt.podGroupDeletionError
				})
			} else {
				patch = gomonkey.ApplyMethod(reflect.TypeOf(controller.podGroupManager), "DeletePodGroup", func(_ *podgroupmanager.Manager, ctx context.Context, ms *workloadv1alpha1.ModelServing, servingGroupName string) error {
					return nil
				})
			}
			defer patch.Reset()

			if tt.podDeletionError != nil {
				client.PrependReactor("delete-collection", "pods", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, tt.podDeletionError
				})
			}

			if tt.serviceDeletionError != nil {
				client.PrependReactor("delete", "services", func(action kubetesting.Action) (handled bool, ret runtime.Object, err error) {
					return true, nil, tt.serviceDeletionError
				})
			}

			ms := &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model-serving",
					Namespace: "default",
				},
			}

			sgName := "test-model-serving-0"

			nsn := utils.GetNamespaceName(ms)
			controller.store.AddServingGroup(nsn, 0, "test-revision")
			controller.store.UpdateServingGroupStatus(nsn, sgName, tt.initialSgStatus)

			initialStatus := controller.store.GetServingGroupStatus(nsn, sgName)
			assert.Equal(t, tt.initialSgStatus, initialStatus)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						workloadv1alpha1.GroupNameLabelKey: sgName,
					},
				},
			}

			service := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service",
					Namespace: "default",
					Labels: map[string]string{
						workloadv1alpha1.GroupNameLabelKey: sgName,
					},
				},
			}

			_, err = client.CoreV1().Pods("default").Create(context.TODO(), pod, metav1.CreateOptions{})
			assert.NoError(t, err)

			_, err = client.CoreV1().Services("default").Create(context.TODO(), service, metav1.CreateOptions{})
			assert.NoError(t, err)
			err = controller.servicesInformer.GetIndexer().Add(service)
			assert.NoError(t, err)

			queue := []string{}
			patchEnqueue := gomonkey.NewPatches()
			patchEnqueue.ApplyPrivateMethod(reflect.TypeOf(&ModelServingController{}), "enqueueModelServing", func(ms *workloadv1alpha1.ModelServing, duration time.Duration) {
				queue = append(queue, ms.Name)
			})
			defer patchEnqueue.Reset()

			_ = controller.deleteServingGroup(context.Background(), ms, sgName)

			finalStatus := controller.store.GetServingGroupStatus(nsn, sgName)
			assert.Equal(t, tt.expectedFinalStatus, finalStatus, "final ServingGroup status should match expected")

			queueLen := len(queue)
			if tt.expectEnqueueCalled {
				assert.True(t, queueLen > 0, "should have enqueued for reconcile")
			} else {
				assert.Equal(t, 0, queueLen, "should not have enqueued for reconcile")
			}
		})
	}
}
