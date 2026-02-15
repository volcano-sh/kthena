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

package datastore

import (
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ptr is a helper function to get pointer to a value
func ptr[T any](v T) *T {
	return &v
}

func Test_updateHistogramMetrics(t *testing.T) {
	sum1 := float64(2)
	count1 := uint64(2)
	sum2 := float64(1)
	count2 := uint64(1)
	type args struct {
		podinfo          *PodInfo
		histogramMetrics map[string]*dto.Histogram
	}
	tests := []struct {
		name string
		args args
	}{
		{
			name: "update histogram metrics",
			args: args{
				podinfo: &PodInfo{
					TimePerOutputToken: &dto.Histogram{
						SampleSum:   &sum1,
						SampleCount: &count1,
					},
					TimeToFirstToken: &dto.Histogram{
						SampleSum:   &sum1,
						SampleCount: &count1,
					},
				},
				histogramMetrics: map[string]*dto.Histogram{
					utils.TPOT: {
						SampleSum:   &sum2,
						SampleCount: &count2,
					},
					utils.TTFT: {
						SampleSum:   &sum2,
						SampleCount: &count2,
					},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updateHistogramMetrics(tt.args.podinfo, tt.args.histogramMetrics)
			assert.Equal(t, tt.args.podinfo.TimePerOutputToken.SampleSum, &sum2)
			assert.Equal(t, tt.args.podinfo.TimePerOutputToken.SampleCount, &count2)
			assert.Equal(t, tt.args.podinfo.TimeToFirstToken.SampleSum, &sum2)
			assert.Equal(t, tt.args.podinfo.TimeToFirstToken.SampleCount, &count2)
		})
	}
}

func TestGetPreviousHistogram(t *testing.T) {
	sum1 := float64(2)
	count1 := uint64(2)

	type args struct {
		podinfo *PodInfo
	}
	tests := []struct {
		name string
		args args
		want map[string]*dto.Histogram
	}{
		{
			name: "get previous histogram",
			args: args{
				podinfo: &PodInfo{
					TimePerOutputToken: &dto.Histogram{
						SampleSum:   &sum1,
						SampleCount: &count1,
					},
					TimeToFirstToken: &dto.Histogram{
						SampleSum:   &sum1,
						SampleCount: &count1,
					},
				},
			},
			want: map[string]*dto.Histogram{
				utils.TPOT: {
					SampleSum:   &sum1,
					SampleCount: &count1,
				},
				utils.TTFT: {
					SampleSum:   &sum1,
					SampleCount: &count1,
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getPreviousHistogram(tt.args.podinfo)
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestStoreUpdatePodMetrics(t *testing.T) {
	sum1 := float64(1)
	count1 := uint64(1)
	sum2 := float64(2)
	count2 := uint64(2)
	podinfo := PodInfo{
		engine: "vLLM",
		TimePerOutputToken: &dto.Histogram{
			SampleSum:   &sum1,
			SampleCount: &count1,
		},
		TimeToFirstToken: &dto.Histogram{
			SampleSum:   &sum1,
			SampleCount: &count1,
		},
		GPUCacheUsage:     0.5,
		RequestWaitingNum: 10,
		RequestRunningNum: 5,
		TPOT:              100,
		TTFT:              200,
		modelServer: sets.New[types.NamespacedName](types.NamespacedName{
			Namespace: "default",
			Name:      "model1",
		}),
	}
	s := &store{
		pods:        sync.Map{},
		modelServer: sync.Map{},
	}

	podName := types.NamespacedName{
		Namespace: "default",
		Name:      "pod1",
	}
	modelServerName := types.NamespacedName{
		Namespace: "default",
		Name:      "model1",
	}

	s.pods.Store(podName, &podinfo)
	s.modelServer.Store(modelServerName, &modelServer{
		pods: sets.New[types.NamespacedName](podName),
	})

	patch := gomonkey.NewPatches()
	patch.ApplyFunc(backend.GetPodMetrics, func(backend string, pod *corev1.Pod, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
		return map[string]float64{
				utils.GPUCacheUsage:     0.8,
				utils.RequestWaitingNum: 15,
				utils.RequestRunningNum: 10,
				utils.TPOT:              120,
				utils.TTFT:              210,
			}, map[string]*dto.Histogram{
				utils.TPOT: {
					SampleSum:   &sum2,
					SampleCount: &count2,
				},
				utils.TTFT: {
					SampleSum:   &sum2,
					SampleCount: &count2,
				},
			}
	})
	defer patch.Reset()

	s.updatePodMetrics(&podinfo)

	name := types.NamespacedName{
		Namespace: "default",
		Name:      "pod1",
	}

	// Get pod info from sync.Map
	if value, ok := s.pods.Load(name); ok {
		podInfo := value.(*PodInfo)
		assert.Equal(t, podInfo.GPUCacheUsage, 0.8)
		assert.Equal(t, podInfo.RequestWaitingNum, float64(15))
		assert.Equal(t, podInfo.RequestRunningNum, float64(10))
		assert.Equal(t, podInfo.TPOT, float64(120))
		assert.Equal(t, podInfo.TTFT, float64(210))
		assert.Equal(t, podInfo.TimePerOutputToken.SampleSum, &sum2)
		assert.Equal(t, podInfo.TimePerOutputToken.SampleCount, &count2)
		assert.Equal(t, podInfo.TimeToFirstToken.SampleSum, &sum2)
		assert.Equal(t, podInfo.TimeToFirstToken.SampleCount, &count2)
	} else {
		t.Errorf("Pod not found in store")
	}
}

func TestStoreAddOrUpdatePod(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "pod1",
		},
	}
	ms1 := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
	}
	ms2 := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model2",
		},
	}

	// Add model server first
	s.AddOrUpdateModelServer(ms1, nil)
	s.AddOrUpdateModelServer(ms2, nil)

	modelServers := []*aiv1alpha1.ModelServer{ms1, ms2}
	err := s.AddOrUpdatePod(pod, modelServers)
	assert.NoError(t, err)

	podName := utils.GetNamespaceName(pod)
	// Check pod is stored and references model servers
	if value, ok := s.pods.Load(podName); ok {
		podInfo := value.(*PodInfo)
		for _, ms := range modelServers {
			msName := utils.GetNamespaceName(ms)
			assert.True(t, podInfo.modelServer.Contains(msName))
		}
		assert.Equal(t, podInfo.Pod.Name, pod.Name, "pod should be stored correctly")
		assert.Equal(t, podInfo.modelServer.Len(), 2, "pod should reference both model servers")
	} else {
		t.Errorf("Pod not found in store")
	}

	// Update pod with only one model server
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms1})
	assert.NoError(t, err)

	if value, ok := s.pods.Load(podName); ok {
		podInfo := value.(*PodInfo)
		assert.True(t, podInfo.modelServer.Contains(utils.GetNamespaceName(ms1)))
		assert.False(t, podInfo.modelServer.Contains(utils.GetNamespaceName(ms2)))
	}

	// Check model server references
	if value, ok := s.modelServer.Load(utils.GetNamespaceName(ms1)); ok {
		ms1Info := value.(*modelServer)
		assert.Equal(t, ms1Info.pods.Len(), 1, "model server 1 should still reference the pod")
	}
	if value, ok := s.modelServer.Load(utils.GetNamespaceName(ms2)); ok {
		ms2Info := value.(*modelServer)
		assert.Equal(t, ms2Info.pods.Len(), 0, "model server 2 should not reference the pod")
	}
}

func TestStoreDeletePod(t *testing.T) {
	podName := types.NamespacedName{Namespace: "default", Name: "pod1"}
	modelServerName := types.NamespacedName{Namespace: "default", Name: "model1"}

	pod := &corev1.Pod{}
	podInfo := &PodInfo{
		Pod:         pod,
		modelServer: sets.New[types.NamespacedName](modelServerName),
		models:      sets.New[string](),
	}

	ms := newModelServer(&aiv1alpha1.ModelServer{})
	ms.addPod(podName)

	s := &store{
		pods:        sync.Map{},
		modelServer: sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	s.pods.Store(podName, podInfo)
	s.modelServer.Store(modelServerName, ms)

	// Normal delete
	err := s.DeletePod(podName)
	assert.NoError(t, err)
	_, exists := s.pods.Load(podName)
	assert.False(t, exists, "pod should be deleted from store")
	assert.False(t, ms.pods.Contains(podName), "pod should be removed from modelServer set")

	// Delete non-existent pod
	err = s.DeletePod(types.NamespacedName{Namespace: "default", Name: "notfound"})
	assert.NoError(t, err)
}

func TestStoreDeletePod_MultiModelServers(t *testing.T) {
	podName := types.NamespacedName{Namespace: "default", Name: "pod1"}
	ms1Name := types.NamespacedName{Namespace: "default", Name: "model1"}
	ms2Name := types.NamespacedName{Namespace: "default", Name: "model2"}

	pod := &corev1.Pod{}
	podInfo := &PodInfo{
		Pod:         pod,
		modelServer: sets.New[types.NamespacedName](ms1Name, ms2Name),
		models:      sets.New[string](),
	}

	ms1 := newModelServer(&aiv1alpha1.ModelServer{})
	ms2 := newModelServer(&aiv1alpha1.ModelServer{})
	ms1.addPod(podName)
	ms2.addPod(podName)

	s := &store{
		pods:        sync.Map{},
		modelServer: sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	s.pods.Store(podName, podInfo)
	s.modelServer.Store(ms1Name, ms1)
	s.modelServer.Store(ms2Name, ms2)

	err := s.DeletePod(podName)
	assert.NoError(t, err)
	assert.False(t, ms1.pods.Contains(podName))
	assert.False(t, ms2.pods.Contains(podName))
}

func TestStoreAddOrUpdateModelServer(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
	}
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
	}
	pods := sets.New[types.NamespacedName](types.NamespacedName{Namespace: "default", Name: "pod1"})
	err := s.AddOrUpdateModelServer(ms, pods)
	assert.NoError(t, err)

	msName := utils.GetNamespaceName(ms)
	if value, ok := s.modelServer.Load(msName); ok {
		msInfo := value.(*modelServer)
		assert.NotNil(t, msInfo)
		assert.True(t, msInfo.pods.Contains(types.NamespacedName{Namespace: "default", Name: "pod1"}))
	} else {
		t.Errorf("ModelServer not found in store")
	}

	// Update with new pods
	pods2 := sets.New[types.NamespacedName](types.NamespacedName{Namespace: "default", Name: "pod2"})
	err = s.AddOrUpdateModelServer(ms, pods2)
	assert.NoError(t, err)

	if value, ok := s.modelServer.Load(msName); ok {
		msInfo := value.(*modelServer)
		assert.True(t, msInfo.pods.Contains(types.NamespacedName{Namespace: "default", Name: "pod2"}))
		assert.False(t, msInfo.pods.Contains(types.NamespacedName{Namespace: "default", Name: "pod1"}))
	}
}

func TestStoreDeleteModelServer(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
	}
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
	}
	msName := utils.GetNamespaceName(ms)
	podName := types.NamespacedName{Namespace: "default", Name: "pod1"}
	modelSrv := newModelServer(ms)
	modelSrv.addPod(podName)
	s.modelServer.Store(msName, modelSrv)
	podInfo := &PodInfo{
		Pod:         &corev1.Pod{},
		modelServer: sets.New[types.NamespacedName](msName),
		models:      sets.New[string](),
	}
	s.pods.Store(podName, podInfo)

	err := s.DeleteModelServer(msName)
	assert.NoError(t, err)
	_, exists := s.modelServer.Load(msName)
	assert.False(t, exists, "modelServer should be deleted")
	assert.False(t, podInfo.modelServer.Contains(msName), "modelServer ref should be removed from podInfo")
	_, podExists := s.pods.Load(podName)
	assert.False(t, podExists, "pod should be deleted if no modelServer left")
}

func TestStoreGetPodsByModelServer(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
	}
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "model1",
		},
	}
	msName := utils.GetNamespaceName(ms)
	podName := types.NamespacedName{Namespace: "default", Name: "pod1"}
	modelSrv := newModelServer(ms)
	modelSrv.addPod(podName)
	s.modelServer.Store(msName, modelSrv)
	podInfo := &PodInfo{
		Pod:         &corev1.Pod{},
		modelServer: sets.New[types.NamespacedName](msName),
		models:      sets.New[string](),
	}
	s.pods.Store(podName, podInfo)

	pods, err := s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, pods, 1)
	assert.Equal(t, podInfo, pods[0])

	_, err = s.GetPodsByModelServer(types.NamespacedName{Namespace: "default", Name: "notfound"})
	assert.Error(t, err)
}

// TestStoreDeleteModelRoute tests various scenarios for DeleteModelRoute method
func TestStoreDeleteModelRoute(t *testing.T) {
	t.Run("delete route with model name", func(t *testing.T) {
		s := &store{
			routeInfo:           make(map[string]*modelRouteInfo),
			routes:              make(map[string][]*aiv1alpha1.ModelRoute),
			loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
			callbacks:           make(map[string][]CallbackFunc),
			requestWaitingQueue: sync.Map{},
		}

		// Create and add a model route
		mr := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "test-route",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName:    "test-model",
				LoraAdapters: []string{"lora1", "lora2"},
			},
		}

		err := s.AddOrUpdateModelRoute(mr)
		assert.NoError(t, err)

		// Add a request queue
		s.requestWaitingQueue.Store("test-model", NewRequestPriorityQueue(nil))

		// Track delete callbacks
		var deleteCallbackCalled atomic.Bool
		s.RegisterCallback("ModelRoute", func(data EventData) {
			if data.EventType == EventDelete {
				deleteCallbackCalled.Store(true)
			}
		})

		// Delete the route
		err = s.DeleteModelRoute("default/test-route")
		assert.NoError(t, err)

		// Verify state
		s.routeMutex.RLock()
		assert.Nil(t, s.routeInfo["default/test-route"])
		assert.Empty(t, s.routes["test-model"])
		assert.Empty(t, s.loraRoutes["lora1"])
		assert.Empty(t, s.loraRoutes["lora2"])
		s.routeMutex.RUnlock()

		// Verify queue is deleted
		_, exists := s.requestWaitingQueue.Load("test-model")
		assert.False(t, exists)

		// Verify callback was called
		assert.Eventually(t, func() bool {
			return deleteCallbackCalled.Load()
		}, time.Second, 10*time.Millisecond)
	})

	t.Run("delete route with only lora adapters", func(t *testing.T) {
		s := &store{
			routeInfo:           make(map[string]*modelRouteInfo),
			routes:              make(map[string][]*aiv1alpha1.ModelRoute),
			loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
			callbacks:           make(map[string][]CallbackFunc),
			requestWaitingQueue: sync.Map{},
		}

		// Create and add a route with only lora adapters
		mr := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "test-ns",
				Name:      "lora-route",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName:    "", // No base model
				LoraAdapters: []string{"lora3", "lora4"},
			},
		}

		err := s.AddOrUpdateModelRoute(mr)
		assert.NoError(t, err)

		// Delete the route
		err = s.DeleteModelRoute("test-ns/lora-route")
		assert.NoError(t, err)

		// Verify state
		s.routeMutex.RLock()
		assert.Nil(t, s.routeInfo["test-ns/lora-route"])
		assert.Empty(t, s.loraRoutes["lora3"])
		assert.Empty(t, s.loraRoutes["lora4"])
		s.routeMutex.RUnlock()
	})

	t.Run("delete non-existent route", func(t *testing.T) {
		s := &store{
			routeInfo:           make(map[string]*modelRouteInfo),
			routes:              make(map[string][]*aiv1alpha1.ModelRoute),
			loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
			callbacks:           make(map[string][]CallbackFunc),
			requestWaitingQueue: sync.Map{},
		}

		// Track callbacks
		var deleteCallbackCalled atomic.Bool
		s.RegisterCallback("ModelRoute", func(data EventData) {
			if data.EventType == EventDelete {
				deleteCallbackCalled.Store(true)
			}
		})

		// Delete non-existent route should not error
		err := s.DeleteModelRoute("default/non-existent")
		assert.NoError(t, err)

		// Callback should still be called
		assert.Eventually(t, func() bool {
			return deleteCallbackCalled.Load()
		}, time.Second, 10*time.Millisecond)
	})

	t.Run("delete route while preserving others", func(t *testing.T) {
		s := &store{
			routeInfo:           make(map[string]*modelRouteInfo),
			routes:              make(map[string][]*aiv1alpha1.ModelRoute),
			loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
			callbacks:           make(map[string][]CallbackFunc),
			requestWaitingQueue: sync.Map{},
		}

		// Add multiple routes
		mr1 := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "route1",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName:    "model1",
				LoraAdapters: []string{"lora1"},
			},
		}
		mr2 := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "route2",
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName:    "model2",
				LoraAdapters: []string{"lora2"},
			},
		}

		err := s.AddOrUpdateModelRoute(mr1)
		assert.NoError(t, err)
		err = s.AddOrUpdateModelRoute(mr2)
		assert.NoError(t, err)

		s.requestWaitingQueue.Store("model1", NewRequestPriorityQueue(nil))
		s.requestWaitingQueue.Store("model2", NewRequestPriorityQueue(nil))

		// Delete route1
		err = s.DeleteModelRoute("default/route1")
		assert.NoError(t, err)

		// Verify route1 is deleted but route2 remains
		s.routeMutex.RLock()
		assert.Nil(t, s.routeInfo["default/route1"])
		assert.NotNil(t, s.routeInfo["default/route2"])
		assert.Empty(t, s.routes["model1"])
		assert.NotEmpty(t, s.routes["model2"])
		assert.Empty(t, s.loraRoutes["lora1"])
		assert.NotEmpty(t, s.loraRoutes["lora2"])
		s.routeMutex.RUnlock()

		// Check queues
		_, exists1 := s.requestWaitingQueue.Load("model1")
		assert.False(t, exists1)
		_, exists2 := s.requestWaitingQueue.Load("model2")
		assert.True(t, exists2)
	})
}

// TestStoreDeleteModelRoute_RequestQueueCleanup specifically tests the cleanup of request queues
func TestStoreDeleteModelRoute_RequestQueueCleanup(t *testing.T) {
	s := &store{
		routeInfo:           make(map[string]*modelRouteInfo),
		routes:              make(map[string][]*aiv1alpha1.ModelRoute),
		loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
		callbacks:           make(map[string][]CallbackFunc),
		requestWaitingQueue: sync.Map{},
	}

	// Create a model route
	mr := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "cleanup-test",
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName: "cleanup-model",
		},
	}

	// Add the route
	err := s.AddOrUpdateModelRoute(mr)
	assert.NoError(t, err)

	// Create and setup a request queue
	queue := NewRequestPriorityQueue(nil)
	s.requestWaitingQueue.Store("cleanup-model", queue)

	// Verify queue exists
	val, exists := s.requestWaitingQueue.Load("cleanup-model")
	assert.True(t, exists)
	assert.NotNil(t, val)

	// Delete the model route
	err = s.DeleteModelRoute("default/cleanup-test")
	assert.NoError(t, err)

	// Verify queue is deleted
	_, exists = s.requestWaitingQueue.Load("cleanup-model")
	assert.False(t, exists)
}

// TestStoreDeleteModelRoute_LoraQueueCleanup verifies that when a ModelRoute with lora adapters is deleted,
// waiting queues for both the base model and all lora names are cleaned up (ratelimit/fairness per-model resources).
func TestStoreDeleteModelRoute_LoraQueueCleanup(t *testing.T) {
	s := &store{
		routeInfo:           make(map[string]*modelRouteInfo),
		routes:              make(map[string][]*aiv1alpha1.ModelRoute),
		loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
		callbacks:           make(map[string][]CallbackFunc),
		requestWaitingQueue: sync.Map{},
	}

	// Create a model route with base model and lora adapters
	mr := &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "lora-cleanup-test",
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName:    "base-model",
			LoraAdapters: []string{"lora-a", "lora-b"},
		},
	}

	err := s.AddOrUpdateModelRoute(mr)
	assert.NoError(t, err)

	// Create waiting queues for base model and loras (as used by fairness/ratelimit per-model)
	s.requestWaitingQueue.Store("base-model", NewRequestPriorityQueue(nil))
	s.requestWaitingQueue.Store("lora-a", NewRequestPriorityQueue(nil))
	s.requestWaitingQueue.Store("lora-b", NewRequestPriorityQueue(nil))

	// Delete the model route
	err = s.DeleteModelRoute("default/lora-cleanup-test")
	assert.NoError(t, err)

	// Verify all related queues are deleted (base model + lora adapters)
	_, existsBase := s.requestWaitingQueue.Load("base-model")
	_, existsLoraA := s.requestWaitingQueue.Load("lora-a")
	_, existsLoraB := s.requestWaitingQueue.Load("lora-b")
	assert.False(t, existsBase, "waiting queue for base model should be cleaned up")
	assert.False(t, existsLoraA, "waiting queue for lora-a should be cleaned up")
	assert.False(t, existsLoraB, "waiting queue for lora-b should be cleaned up")
}

// TestStoreDeleteModelRoute_ConcurrentAccess tests thread safety of DeleteModelRoute
func TestStoreDeleteModelRoute_ConcurrentAccess(t *testing.T) {
	s := &store{
		routeInfo:           make(map[string]*modelRouteInfo),
		routes:              make(map[string][]*aiv1alpha1.ModelRoute),
		loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
		callbacks:           make(map[string][]CallbackFunc),
		requestWaitingQueue: sync.Map{},
	}

	// Add multiple routes
	for i := 0; i < 10; i++ {
		mr := &aiv1alpha1.ModelRoute{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      fmt.Sprintf("route%d", i),
			},
			Spec: aiv1alpha1.ModelRouteSpec{
				ModelName: fmt.Sprintf("model%d", i),
			},
		}
		err := s.AddOrUpdateModelRoute(mr)
		assert.NoError(t, err)

		s.requestWaitingQueue.Store(fmt.Sprintf("model%d", i), NewRequestPriorityQueue(nil))
	}

	// Concurrently delete routes
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			err := s.DeleteModelRoute(fmt.Sprintf("default/route%d", index))
			assert.NoError(t, err)
		}(i)
	}

	wg.Wait()

	// Verify all routes and queues are deleted
	s.routeMutex.RLock()
	assert.Empty(t, s.routeInfo)
	assert.Empty(t, s.routes)
	assert.Empty(t, s.loraRoutes)
	s.routeMutex.RUnlock()

	// Verify all queues are deleted
	s.requestWaitingQueue.Range(func(key, value interface{}) bool {
		t.Errorf("Queue should not exist for key: %v", key)
		return true
	})
}

// createComplexModelRoute creates a ModelRoute with multiple rules for different models
func createComplexModelRoute() *aiv1alpha1.ModelRoute {
	return &aiv1alpha1.ModelRoute{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "complex-route",
		},
		Spec: aiv1alpha1.ModelRouteSpec{
			ModelName:    "llama2-7b",
			LoraAdapters: []string{"math-lora", "code-lora", "science-lora"},
			Rules: []*aiv1alpha1.Rule{
				{
					Name: "base-model-rule",
					ModelMatch: &aiv1alpha1.ModelMatch{
						Body: &aiv1alpha1.BodyMatch{
							Model: ptr("llama2-7b"),
						},
					},
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "base-model-server",
						},
					},
				},
				{
					Name: "math-lora-rule",
					ModelMatch: &aiv1alpha1.ModelMatch{
						Body: &aiv1alpha1.BodyMatch{
							Model: ptr("math-lora"),
						},
					},
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "math-specialized-server",
						},
					},
				},
				{
					Name: "code-lora-rule",
					ModelMatch: &aiv1alpha1.ModelMatch{
						Body: &aiv1alpha1.BodyMatch{
							Model: ptr("code-lora"),
						},
					},
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "code-specialized-server",
						},
					},
				},
				{
					Name: "science-lora-rule",
					ModelMatch: &aiv1alpha1.ModelMatch{
						Body: &aiv1alpha1.BodyMatch{
							Model: ptr("science-lora"),
						},
					},
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "science-specialized-server",
						},
					},
				},
				{
					Name: "fallback-rule",
					TargetModels: []*aiv1alpha1.TargetModel{
						{
							ModelServerName: "fallback-server",
						},
					},
				},
			},
		},
	}
}

func TestStoreMatchModelServer(t *testing.T) {
	tests := []struct {
		name           string
		setupStore     func() *store
		modelName      string
		request        *http.Request
		expectedServer types.NamespacedName
		expectedIsLora bool
		expectedError  bool
	}{
		{
			name: "match base model route",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				// Create a ModelRoute with base model and LoRA adapters
				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "test-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName:    "llama2-7b",
						LoraAdapters: []string{"math-lora", "code-lora"},
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "default-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "llama2-server",
										Weight:          ptr(uint32(100)),
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName:      "llama2-7b",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "llama2-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "match LoRA adapter route",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "test-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName:    "llama2-7b",
						LoraAdapters: []string{"math-lora", "code-lora"},
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "lora-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "lora-server",
										Weight:          ptr(uint32(100)),
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName:      "math-lora",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "lora-server"},
			expectedIsLora: true,
			expectedError:  false,
		},
		{
			name: "match with header conditions",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "test-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName:    "llama2-7b",
						LoraAdapters: []string{"math-lora"},
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "header-rule",
								ModelMatch: &aiv1alpha1.ModelMatch{
									Headers: map[string]*aiv1alpha1.StringMatch{
										"X-Model-Type": {
											Exact: ptr("production"),
										},
									},
								},
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "prod-server",
										Weight:          ptr(uint32(100)),
									},
								},
							},
							{
								Name: "fallback-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "dev-server",
										Weight:          ptr(uint32(100)),
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName: "llama2-7b",
			request: &http.Request{
				URL: &url.URL{Path: "/v1/chat/completions"},
				Header: map[string][]string{
					"X-Model-Type": {"production"},
				},
			},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "prod-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "route math-lora to specialized server",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}
				s.AddOrUpdateModelRoute(createComplexModelRoute())
				return s
			},
			modelName:      "math-lora",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "math-specialized-server"},
			expectedIsLora: true,
			expectedError:  false,
		},
		{
			name: "route base model to base server",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}
				s.AddOrUpdateModelRoute(createComplexModelRoute())
				return s
			},
			modelName:      "llama2-7b",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "base-model-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "route code-lora to specialized server",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}
				s.AddOrUpdateModelRoute(createComplexModelRoute())
				return s
			},
			modelName:      "code-lora",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "code-specialized-server"},
			expectedIsLora: true,
			expectedError:  false,
		},
		{
			name: "route science-lora to specialized server",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}
				s.AddOrUpdateModelRoute(createComplexModelRoute())
				return s
			},
			modelName:      "science-lora",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "science-specialized-server"},
			expectedIsLora: true,
			expectedError:  false,
		},
		{
			name: "route with URI prefix match",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "uri-prefix-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName: "uri-test-model",
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "v1-prefix-rule",
								ModelMatch: &aiv1alpha1.ModelMatch{
									Uri: &aiv1alpha1.StringMatch{
										Prefix: ptr("/v1/"),
									},
								},
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "v1-server",
									},
								},
							},
							{
								Name: "fallback-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "default-server",
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName:      "uri-test-model",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "v1-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "route with URI exact match",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "uri-exact-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName: "exact-uri-model",
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "exact-chat-rule",
								ModelMatch: &aiv1alpha1.ModelMatch{
									Uri: &aiv1alpha1.StringMatch{
										Exact: ptr("/v1/chat/completions"),
									},
								},
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "chat-server",
									},
								},
							},
							{
								Name: "fallback-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "fallback-server",
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName:      "exact-uri-model",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "chat-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "route falls back when URI doesn't match",
			setupStore: func() *store {
				s := &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}

				mr := &aiv1alpha1.ModelRoute{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "default",
						Name:      "uri-fallback-route",
					},
					Spec: aiv1alpha1.ModelRouteSpec{
						ModelName: "fallback-uri-model",
						Rules: []*aiv1alpha1.Rule{
							{
								Name: "specific-v1-rule",
								ModelMatch: &aiv1alpha1.ModelMatch{
									Uri: &aiv1alpha1.StringMatch{
										Prefix: ptr("/v1/"),
									},
								},
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "v1-server",
									},
								},
							},
							{
								Name: "fallback-rule",
								TargetModels: []*aiv1alpha1.TargetModel{
									{
										ModelServerName: "fallback-server",
									},
								},
							},
						},
					},
				}
				s.AddOrUpdateModelRoute(mr)
				return s
			},
			modelName:      "fallback-uri-model",
			request:        &http.Request{URL: &url.URL{Path: "/v2/completions"}},
			expectedServer: types.NamespacedName{Namespace: "default", Name: "fallback-server"},
			expectedIsLora: false,
			expectedError:  false,
		},
		{
			name: "no matching route",
			setupStore: func() *store {
				return &store{
					routeInfo:  make(map[string]*modelRouteInfo),
					routes:     make(map[string][]*aiv1alpha1.ModelRoute),
					loraRoutes: make(map[string][]*aiv1alpha1.ModelRoute),
				}
			},
			modelName:      "non-existent-model",
			request:        &http.Request{URL: &url.URL{Path: "/v1/chat/completions"}},
			expectedServer: types.NamespacedName{},
			expectedIsLora: false,
			expectedError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.setupStore()
			server, isLora, _, err := s.MatchModelServer(tt.modelName, tt.request, "")

			if tt.expectedError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedIsLora, isLora)
			assert.Equal(t, tt.expectedServer, server)
		})
	}
}
