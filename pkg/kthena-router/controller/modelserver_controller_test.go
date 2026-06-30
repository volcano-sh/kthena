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
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	informersv1alpha1 "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

type fakePodRuntimeInspector struct{}

func (fakePodRuntimeInspector) GetPodMetrics(_ string, _ *corev1.Pod, _ uint32, _ map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	return map[string]float64{
		utils.KVCacheUsage:      0.5,
		utils.RequestWaitingNum: 10,
		utils.RequestRunningNum: 5,
	}, nil
}

func (fakePodRuntimeInspector) GetPodModels(_ string, _ *corev1.Pod, _ uint32) ([]string, error) {
	return []string{"test-model"}, nil
}

func newStoreWithMockBackend() datastore.Store {
	return datastore.New(datastore.WithPodRuntimeInspector(fakePodRuntimeInspector{}))
}

func TestModelServerController_ModelServerLifecycle(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)
	modelServerIndexer := kthenaInformerFactory.Networking().V1alpha1().ModelServers().Informer().GetIndexer()

	stop := make(chan struct{})
	defer close(stop)

	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	t.Run("ModelServerCreate", func(t *testing.T) {
		ms := &aiv1alpha1.ModelServer{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-modelserver",
			},
			Spec: aiv1alpha1.ModelServerSpec{
				InferenceEngine: aiv1alpha1.VLLM,
				WorkloadSelector: &aiv1alpha1.WorkloadSelector{
					MatchLabels: map[string]string{
						"app": "test-model",
					},
				},
			},
		}

		if !waitForCacheSync(t, 5*time.Second, controller.modelServerSynced, controller.podSynced) {
			t.Fatal("Failed to sync caches within timeout")
		}

		require.NoError(t, modelServerIndexer.Add(ms.DeepCopy()))
		_, err := controller.modelServerLister.ModelServers("default").Get("test-modelserver")
		require.NoError(t, err)

		controller.enqueueModelServer(ms)
		assert.Equal(t, 1, controller.workqueue.Len())

		err = controller.syncModelServerHandler("default/test-modelserver")
		assert.NoError(t, err)

		storedMS := store.GetModelServer(types.NamespacedName{
			Namespace: "default",
			Name:      "test-modelserver",
		})
		require.NotNil(t, storedMS, "ModelServer should be found in store after creation")
		assert.Equal(t, "test-modelserver", storedMS.Name)
	})

	t.Run("ModelServerUpdate", func(t *testing.T) {
		ms := &aiv1alpha1.ModelServer{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-modelserver-update",
				Labels: map[string]string{
					"version": "v1",
				},
			},
			Spec: aiv1alpha1.ModelServerSpec{
				InferenceEngine: aiv1alpha1.VLLM,
				WorkloadSelector: &aiv1alpha1.WorkloadSelector{
					MatchLabels: map[string]string{
						"app": "test-model-update",
					},
				},
			},
		}

		require.NoError(t, modelServerIndexer.Add(ms.DeepCopy()))
		_, err := controller.modelServerLister.ModelServers("default").Get("test-modelserver-update")
		require.NoError(t, err)

		controller.enqueueModelServer(ms)
		err = controller.syncModelServerHandler("default/test-modelserver-update")
		assert.NoError(t, err)

		updatedMS := ms.DeepCopy()
		updatedMS.Labels["version"] = "v2"
		updatedMS.Spec.WorkloadSelector.MatchLabels["environment"] = "production"

		require.NoError(t, modelServerIndexer.Update(updatedMS.DeepCopy()))
		cachedMS, err := controller.modelServerLister.ModelServers("default").Get("test-modelserver-update")
		require.NoError(t, err)
		assert.Equal(t, "v2", cachedMS.Labels["version"])

		controller.enqueueModelServer(updatedMS)
		for controller.workqueue.Len() > 0 {
			item, _ := controller.workqueue.Get()
			controller.workqueue.Done(item)
			controller.workqueue.Forget(item)
		}
		controller.enqueueModelServer(updatedMS)
		assert.Equal(t, 1, controller.workqueue.Len())

		err = controller.syncModelServerHandler("default/test-modelserver-update")
		assert.NoError(t, err)

		storedMS := store.GetModelServer(types.NamespacedName{
			Namespace: "default",
			Name:      "test-modelserver-update",
		})
		require.NotNil(t, storedMS, "ModelServer should be found in store after update")
		assert.Equal(t, "v2", storedMS.Labels["version"])
		assert.Equal(t, "production", storedMS.Spec.WorkloadSelector.MatchLabels["environment"])
	})

	t.Run("ModelServerDelete", func(t *testing.T) {
		ms := &aiv1alpha1.ModelServer{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-modelserver-delete",
			},
			Spec: aiv1alpha1.ModelServerSpec{
				InferenceEngine: aiv1alpha1.VLLM,
				WorkloadSelector: &aiv1alpha1.WorkloadSelector{
					MatchLabels: map[string]string{
						"app": "test-model-delete",
					},
				},
			},
		}

		require.NoError(t, modelServerIndexer.Add(ms.DeepCopy()))
		_, err := controller.modelServerLister.ModelServers("default").Get("test-modelserver-delete")
		require.NoError(t, err)

		err = controller.syncModelServerHandler("default/test-modelserver-delete")
		assert.NoError(t, err)

		storedMS := store.GetModelServer(types.NamespacedName{
			Namespace: "default",
			Name:      "test-modelserver-delete",
		})
		require.NotNil(t, storedMS, "ModelServer should be found in store before deletion")

		require.NoError(t, modelServerIndexer.Delete(ms.DeepCopy()))
		_, err = controller.modelServerLister.ModelServers("default").Get("test-modelserver-delete")
		assert.Error(t, err)

		err = controller.syncModelServerHandler("default/test-modelserver-delete")
		assert.NoError(t, err)

		storedMS = store.GetModelServer(types.NamespacedName{
			Namespace: "default",
			Name:      "test-modelserver-delete",
		})
		assert.Nil(t, storedMS)
	})
}

func TestModelServerController_PodLifecycle(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-modelserver-pods",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{
					"app": "test-model-pods",
				},
			},
		},
	}
	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms, metav1.CreateOptions{})
	assert.NoError(t, err)

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	stop := make(chan struct{})
	defer close(stop)

	go controller.Run(stop)

	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)
	waitForCacheSync(t, 5*time.Second, controller.modelServerSynced, controller.podSynced)

	t.Run("PodCreateReady", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-ready",
				Labels: map[string]string{
					"app": "test-model-pods",
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

		_, err := kubeClient.CoreV1().Pods("default").Create(
			context.Background(), pod, metav1.CreateOptions{})
		assert.NoError(t, err)

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) > 0 && pods[0].Pod.Name == "test-pod-ready"
		})
		assert.True(t, sync, "Pod should be found in store after creation")
	})

	t.Run("PodCreateNotReady", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-not-ready",
				Labels: map[string]string{
					"app": "test-model-pods",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				},
			},
		}

		_, err := kubeClient.CoreV1().Pods("default").Create(
			context.Background(), pod, metav1.CreateOptions{})
		assert.NoError(t, err)

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := controller.podLister.Pods("default").Get("test-pod-not-ready")
			return pods != nil && pods.Name == "test-pod-not-ready"
		})
		assert.True(t, sync, "Pod should be found in lister after creation")

		sync = waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) == 1 && pods[0].Pod.Name == "test-pod-ready"
		})
		assert.True(t, sync, "Pod should be found in store after creation")
	})

	t.Run("PodUpdateBecomesReady", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-update-ready",
				Labels: map[string]string{
					"app": "test-model-pods",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{
					{
						Type:   corev1.PodReady,
						Status: corev1.ConditionFalse,
					},
				},
			},
		}

		_, err := kubeClient.CoreV1().Pods("default").Create(
			context.Background(), pod, metav1.CreateOptions{})
		assert.NoError(t, err)

		updatedPod := pod.DeepCopy()
		updatedPod.Status.Phase = corev1.PodRunning
		updatedPod.Status.Conditions[0].Status = corev1.ConditionTrue

		_, err = kubeClient.CoreV1().Pods("default").Update(
			context.Background(), updatedPod, metav1.UpdateOptions{})
		assert.NoError(t, err)

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) == 2 &&
				(pods[0].Pod.Name == "test-pod-update-ready" || pods[1].Pod.Name == "test-pod-update-ready")
		})
		assert.True(t, sync, "Pod should be found in store after update")
	})

	t.Run("PodUpdateBecomesNotReady", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-update-not-ready",
				Labels: map[string]string{
					"app": "test-model-pods",
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

		_, err := kubeClient.CoreV1().Pods("default").Create(
			context.Background(), pod, metav1.CreateOptions{})
		assert.NoError(t, err)

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) == 3
		})
		assert.True(t, sync, "Pod should be found in store after creation")

		updatedPod := pod.DeepCopy()
		updatedPod.Status.Phase = corev1.PodFailed
		updatedPod.Status.Conditions[0].Status = corev1.ConditionFalse

		_, err = kubeClient.CoreV1().Pods("default").Update(
			context.Background(), updatedPod, metav1.UpdateOptions{})
		assert.NoError(t, err)

		sync = waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) == 2 && (pods[0].Pod.Name != "test-pod-update-not-ready" && pods[1].Pod.Name != "test-pod-update-not-ready")
		})
		assert.True(t, sync, "Pod should not be found in store after creation")
	})

	t.Run("PodDelete", func(t *testing.T) {
		for _, podName := range []string{"test-pod-ready", "test-pod-not-ready", "test-pod-update-ready", "test-pod-update-not-ready"} {
			err = kubeClient.CoreV1().Pods("default").Delete(
				context.Background(), podName, metav1.DeleteOptions{})
			assert.NoError(t, err)
		}

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) == 0
		})
		assert.True(t, sync, "Pod should not be found in store after deletion")
	})
}

func TestModelServerController_ErrorHandling(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	t.Run("InvalidModelServerKey", func(t *testing.T) {
		err := controller.syncModelServerHandler("invalid-key-format")
		assert.NoError(t, err)
	})

	t.Run("InvalidPodKey", func(t *testing.T) {
		err := controller.syncPodHandler("invalid-key-format")
		assert.NoError(t, err)
	})

	t.Run("NonExistentModelServer", func(t *testing.T) {
		err := controller.syncModelServerHandler("default/non-existent-modelserver")
		assert.NoError(t, err)
	})

	t.Run("NonExistentPod", func(t *testing.T) {
		err := controller.syncPodHandler("default/non-existent-pod")
		assert.NoError(t, err)
	})
}

func TestModelServerController_WorkQueueProcessing(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	t.Run("InitialSyncSignal", func(t *testing.T) {
		controller.workqueue.Add(QueueItem{})
		assert.Equal(t, 1, controller.workqueue.Len())

		processed := controller.processNextWorkItem()
		assert.True(t, processed)
		assert.True(t, controller.HasSynced())
		assert.Equal(t, 0, controller.workqueue.Len())
	})

	t.Run("UnknownResourceType", func(t *testing.T) {
		unknownItem := QueueItem{
			ResourceType: "UnknownType",
			Key:          "default/unknown-resource",
		}

		controller.workqueue.Add(unknownItem)
		assert.Equal(t, 1, controller.workqueue.Len())

		processed := controller.processNextWorkItem()
		assert.True(t, processed)
		assert.Equal(t, 0, controller.workqueue.Len())
	})

	t.Run("MultipleQueueItems", func(t *testing.T) {
		items := []QueueItem{
			{ResourceType: ResourceTypeModelServer, Key: "default/ms1"},
			{ResourceType: ResourceTypePod, Key: "default/pod1"},
			{ResourceType: ResourceTypeModelServer, Key: "default/ms2"},
			{ResourceType: ResourceTypePod, Key: "default/pod2"},
		}

		for _, item := range items {
			controller.workqueue.Add(item)
		}
		assert.Equal(t, 4, controller.workqueue.Len())

		processedCount := 0
		for controller.workqueue.Len() > 0 {
			processed := controller.processNextWorkItem()
			assert.True(t, processed)
			processedCount++
		}
		assert.Equal(t, 4, processedCount)
		assert.Equal(t, 0, controller.workqueue.Len())
	})
}

func TestModelServerController_PodSelectionLogic(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	stop := make(chan struct{})
	defer close(stop)
	go controller.Run(stop)
	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	t.Run("PodWithNonMatchingLabels", func(t *testing.T) {
		ms := &aiv1alpha1.ModelServer{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-modelserver-selector",
			},
			Spec: aiv1alpha1.ModelServerSpec{
				InferenceEngine: aiv1alpha1.VLLM,
				WorkloadSelector: &aiv1alpha1.WorkloadSelector{
					MatchLabels: map[string]string{
						"app":     "specific-model",
						"version": "v1",
					},
				},
			},
		}

		_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
			context.Background(), ms, metav1.CreateOptions{})
		assert.NoError(t, err)

		podNonMatching := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-non-matching",
				Labels: map[string]string{
					"app":     "different-model",
					"version": "v2",
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

		_, err = kubeClient.CoreV1().Pods("default").Create(
			context.Background(), podNonMatching, metav1.CreateOptions{})
		assert.NoError(t, err)

		waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := controller.podLister.Pods("default").Get("test-pod-non-matching")
			return pods != nil && pods.Name == "test-pod-non-matching"
		})

		controller.enqueuePod(podNonMatching)
		err = controller.syncPodHandler("default/test-pod-non-matching")
		assert.NoError(t, err)

		podMatching := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-pod-matching",
				Labels: map[string]string{
					"app":     "specific-model",
					"version": "v1",
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

		_, err = kubeClient.CoreV1().Pods("default").Create(
			context.Background(), podMatching, metav1.CreateOptions{})
		assert.NoError(t, err)

		sync := waitForObjectInCache(t, 2*time.Second, func() bool {
			pods, _ := store.GetPodsByModelServer(utils.GetNamespaceName(ms))
			return len(pods) > 0 && pods[0].Pod.Name == "test-pod-matching"
		})
		assert.True(t, sync, "Pod should be found in store after creation")
	})
}

func TestModelServerController_ComprehensiveLifecycleTest(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "integration-modelserver",
			Labels: map[string]string{
				"version": "v1",
			},
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{
					"app": "integration-model",
				},
			},
		},
	}

	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "ready-pod",
			Labels: map[string]string{
				"app": "integration-model",
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

	notReadyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test-ns",
			Name:      "not-ready-pod",
			Labels: map[string]string{
				"app": "integration-model",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionFalse,
				},
			},
		},
	}

	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("test-ns").Create(
		context.Background(), ms, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = kubeClient.CoreV1().Pods("test-ns").Create(
		context.Background(), readyPod, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = kubeClient.CoreV1().Pods("test-ns").Create(
		context.Background(), notReadyPod, metav1.CreateOptions{})
	assert.NoError(t, err)

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	stopCh := make(chan struct{})
	defer close(stopCh)

	store := newStoreWithMockBackend()
	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	kthenaInformerFactory.Start(stopCh)
	kubeInformerFactory.Start(stopCh)

	waitForObjectInCache(t, 2*time.Second, func() bool {
		ms, err := controller.modelServerLister.ModelServers("test-ns").Get("integration-modelserver")
		return err == nil && ms.Name == "integration-modelserver"
	})

	err = controller.syncModelServerHandler("test-ns/integration-modelserver")
	assert.NoError(t, err)

	waitForObjectInCache(t, 2*time.Second, func() bool {
		ret, err := controller.podLister.Pods("test-ns").List(labels.Everything())
		return err == nil && len(ret) == 2
	})

	err = controller.syncPodHandler("test-ns/ready-pod")
	assert.NoError(t, err)

	err = controller.syncPodHandler("test-ns/not-ready-pod")
	assert.NoError(t, err)

	updatedMS := ms.DeepCopy()
	updatedMS.Labels["version"] = "v2"
	updatedMS.Spec.InferenceEngine = aiv1alpha1.SGLang

	_, err = kthenaClient.NetworkingV1alpha1().ModelServers("test-ns").Update(
		context.Background(), updatedMS, metav1.UpdateOptions{})
	assert.NoError(t, err)

	if !waitForCacheSync(t, 5*time.Second, controller.modelServerSynced) {
		t.Log("Cache sync timeout after update - proceeding anyway")
	}

	found := waitForObjectInCache(t, 2*time.Second, func() bool {
		ms, err := controller.modelServerLister.ModelServers("test-ns").Get("integration-modelserver")
		if err != nil {
			return false
		}
		return ms.Labels["version"] == "v2"
	})
	assert.True(t, found, "Updated ModelServer should be found in cache after update")

	err = controller.syncModelServerHandler("test-ns/integration-modelserver")
	assert.NoError(t, err)

	storedMS := store.GetModelServer(types.NamespacedName{
		Namespace: "test-ns",
		Name:      "integration-modelserver",
	})
	if storedMS != nil {
		assert.Equal(t, "v2", storedMS.Labels["version"])
		assert.Equal(t, aiv1alpha1.SGLang, storedMS.Spec.InferenceEngine)
	}

	err = controller.syncModelServerHandler("test-ns/non-existent-modelserver")
	assert.NoError(t, err)

	err = controller.syncPodHandler("test-ns/non-existent-pod")
	assert.NoError(t, err)
}

func TestModelServerController_SharedPods(t *testing.T) {
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()

	ms1 := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{
					"app": "shared-model",
				},
			},
		},
	}
	ms2 := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model2",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: aiv1alpha1.VLLM,
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{
					"app": "shared-model",
				},
			},
		},
	}

	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod1",
			Labels: map[string]string{
				"app": "shared-model",
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
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod2",
			Labels: map[string]string{
				"app": "shared-model",
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

	_, err := kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms1, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = kubeClient.CoreV1().Pods("default").Create(
		context.Background(), pod1, metav1.CreateOptions{})
	assert.NoError(t, err)

	_, err = kubeClient.CoreV1().Pods("default").Create(
		context.Background(), pod2, metav1.CreateOptions{})
	assert.NoError(t, err)

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := informersv1alpha1.NewSharedInformerFactory(kthenaClient, 0)

	store := newStoreWithMockBackend()

	controller := NewModelServerController(
		kthenaClient,
		kthenaInformerFactory,
		kubeInformerFactory,
		store,
	)

	stop := make(chan struct{})
	defer close(stop)

	kthenaInformerFactory.Start(stop)
	kubeInformerFactory.Start(stop)

	if !waitForCacheSync(t, 5*time.Second, controller.modelServerSynced, controller.podSynced) {
		t.Fatal("Failed to sync caches within timeout")
	}

	waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.modelServerLister.ModelServers("default").Get("model1")
		return err == nil
	})

	waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.podLister.Pods("default").Get("pod1")
		return err == nil
	})

	waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.podLister.Pods("default").Get("pod2")
		return err == nil
	})

	ms1Name := utils.GetNamespaceName(ms1)
	pod1Name := utils.GetNamespaceName(pod1)
	pod2Name := utils.GetNamespaceName(pod2)

	err = controller.syncModelServerHandler("default/model1")
	assert.NoError(t, err)

	err = controller.syncPodHandler("default/pod1")
	assert.NoError(t, err)
	err = controller.syncPodHandler("default/pod2")
	assert.NoError(t, err)

	_, err = kthenaClient.NetworkingV1alpha1().ModelServers("default").Create(
		context.Background(), ms2, metav1.CreateOptions{})
	assert.NoError(t, err)

	waitForObjectInCache(t, 2*time.Second, func() bool {
		_, err := controller.modelServerLister.ModelServers("default").Get("model2")
		return err == nil
	})

	ms2Name := utils.GetNamespaceName(ms2)
	err = controller.syncModelServerHandler("default/model2")
	assert.NoError(t, err)

	pods, err := store.GetPodsByModelServer(ms2Name)
	assert.NoError(t, err)
	assert.Len(t, pods, 2, "ms2 should have 2 pods")

	podNames := make(map[types.NamespacedName]bool)
	for _, pod := range pods {
		podNames[utils.GetNamespaceName(pod.Pod)] = true
	}
	assert.True(t, podNames[pod1Name], "pod1 should be returned for ms2")
	assert.True(t, podNames[pod2Name], "pod2 should be returned for ms2")

	podsMS1, err := store.GetPodsByModelServer(ms1Name)
	assert.NoError(t, err)
	assert.Len(t, podsMS1, 2, "ms1 should also have 2 pods")

	pod1Info := store.GetPodInfo(pod1Name)
	assert.NotNil(t, pod1Info)
	assert.True(t, pod1Info.HasModelServer(ms1Name), "pod1 should reference ms1")
	assert.True(t, pod1Info.HasModelServer(ms2Name), "pod1 should reference ms2")

	pod2Info := store.GetPodInfo(pod2Name)
	assert.NotNil(t, pod2Info)
	assert.True(t, pod2Info.HasModelServer(ms1Name), "pod2 should reference ms1")
	assert.True(t, pod2Info.HasModelServer(ms2Name), "pod2 should reference ms2")
}

func waitForCacheSync(t *testing.T, timeout time.Duration, cacheSyncWaiters ...cache.InformerSynced) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if !cache.WaitForCacheSync(ctx.Done(), cacheSyncWaiters...) {
		t.Logf("Cache sync timeout after %v - some caches may not be synced", timeout)
		return false
	}
	return true
}

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
