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
	"sync"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// Helper function to create a test Pod
func createTestPod(namespace, name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// Helper function to create a test ModelServer
func createTestModelServer(namespace, name string, engine aiv1alpha1.InferenceEngine) *aiv1alpha1.ModelServer {
	return &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
		Spec: aiv1alpha1.ModelServerSpec{
			InferenceEngine: engine,
		},
	}
}

func newStoreWithMockBackend() *store {
	return New(WithPodRuntimeInspector(&fakePodRuntimeInspector{
		metricsFn: func(_ string, _ *corev1.Pod, _ map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
			return map[string]float64{
				utils.GPUCacheUsage:     0.5,
				utils.RequestWaitingNum: 10,
				utils.RequestRunningNum: 5,
			}, nil
		},
		modelsFn: func(_ string, _ *corev1.Pod) ([]string, error) {
			return []string{"test-model"}, nil
		},
	})).(*store)
}

// Test Case 1: ModelServer added first, then Pod
func TestStore_AddModelServerFirst_ThenPod(t *testing.T) {
	s := newStoreWithMockBackend()

	// Step 1: Add ModelServer first
	ms := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	pod := createTestPod("default", "pod1")

	msName := utils.GetNamespaceName(ms)
	podName := utils.GetNamespaceName(pod)

	// Add ModelServer with empty pod set
	err := s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// Verify ModelServer exists but has no pods
	assert.NotNil(t, s.GetModelServer(msName))
	pods, err := s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, pods, 0)

	// Step 2: Add Pod with ModelServer references
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	// Verify relationships are established
	podInfo := s.GetPodInfo(podName)
	assert.NotNil(t, podInfo)
	assert.True(t, podInfo.HasModelServer(msName))
	assert.Equal(t, string(aiv1alpha1.VLLM), podInfo.engine)

	// Verify ModelServer now references the pod
	msValue, _ := s.modelServer.Load(msName)
	assert.True(t, msValue.(*modelServer).pods.Contains(podName))
}

// Test Case 2: Pod added first, then ModelServer
// Note: Current implementation expects ModelServer to exist before Pod
func TestStore_AddPodFirst_ThenModelServer(t *testing.T) {
	s := newStoreWithMockBackend()

	ms := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	pod := createTestPod("default", "pod1")

	msName := utils.GetNamespaceName(ms)
	podName := utils.GetNamespaceName(pod)

	// Step 1: Add ModelServer first (current implementation requirement)
	err := s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// Step 2: Add Pod with ModelServer references
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	// Verify Pod exists and references ModelServer
	podInfo := s.GetPodInfo(podName)
	assert.NotNil(t, podInfo)
	assert.True(t, podInfo.HasModelServer(msName))

	// Verify ModelServer references the Pod
	msValue, _ := s.modelServer.Load(msName)
	assert.True(t, msValue.(*modelServer).pods.Contains(podName))

	// Step 3: Update ModelServer with explicit pod references
	err = s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName](podName))
	assert.NoError(t, err)

	// Verify ModelServer is properly set
	assert.Equal(t, ms, s.GetModelServer(msName))
}

// Test Case 3: Multiple Pods added with ModelServer
func TestStore_MultiplePods_ThenModelServer(t *testing.T) {
	s := newStoreWithMockBackend()

	ms := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	pod1 := createTestPod("default", "pod1")
	pod2 := createTestPod("default", "pod2")

	msName := utils.GetNamespaceName(ms)
	pod1Name := utils.GetNamespaceName(pod1)
	pod2Name := utils.GetNamespaceName(pod2)

	// Add ModelServer first (current implementation requirement)
	err := s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// Add pods
	err = s.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	err = s.AddOrUpdatePod(pod2, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	// Both pods should reference the ModelServer
	assert.True(t, s.GetPodInfo(pod1Name).HasModelServer(msName))
	assert.True(t, s.GetPodInfo(pod2Name).HasModelServer(msName))
	msValue, _ := s.modelServer.Load(msName)
	assert.True(t, msValue.(*modelServer).pods.Contains(pod1Name))
	assert.True(t, msValue.(*modelServer).pods.Contains(pod2Name))

	// Update ModelServer with explicit pod references
	err = s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName](pod1Name, pod2Name))
	assert.NoError(t, err)

	// Verify all relationships are maintained
	pods, err := s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, pods, 2)
}

// Test Case 4: ModelServer with multiple Pods added together
func TestStore_ModelServerWithMultiplePods_AddedTogether(t *testing.T) {
	s := newStoreWithMockBackend()

	ms := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	pod1 := createTestPod("default", "pod1")
	pod2 := createTestPod("default", "pod2")

	msName := utils.GetNamespaceName(ms)
	pod1Name := utils.GetNamespaceName(pod1)
	pod2Name := utils.GetNamespaceName(pod2)

	// Add ModelServer with pod references
	err := s.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName](pod1Name, pod2Name))
	assert.NoError(t, err)

	// Add pods
	err = s.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	err = s.AddOrUpdatePod(pod2, []*aiv1alpha1.ModelServer{ms})
	assert.NoError(t, err)

	// Verify all relationships
	assert.True(t, s.GetPodInfo(pod1Name).HasModelServer(msName))
	assert.True(t, s.GetPodInfo(pod2Name).HasModelServer(msName))

	pods, err := s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, pods, 2)
}

// Test Case 5: Pod belongs to multiple ModelServers
func TestStore_PodBelongsToMultipleModelServers(t *testing.T) {
	s := newStoreWithMockBackend()

	ms1 := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	ms2 := createTestModelServer("default", "model2", aiv1alpha1.VLLM)
	pod := createTestPod("default", "pod1")

	ms1Name := utils.GetNamespaceName(ms1)
	ms2Name := utils.GetNamespaceName(ms2)
	podName := utils.GetNamespaceName(pod)

	// Add ModelServers first
	err := s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	err = s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// Add Pod referencing both ModelServers
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms1, ms2})
	assert.NoError(t, err)

	// Verify Pod references both ModelServers
	podInfo := s.GetPodInfo(podName)
	assert.True(t, podInfo.HasModelServer(ms1Name))
	assert.True(t, podInfo.HasModelServer(ms2Name))
	assert.Equal(t, 2, podInfo.GetModelServerCount())

	// Verify both ModelServers reference the Pod
	ms1Value, _ := s.modelServer.Load(ms1Name)
	ms2Value, _ := s.modelServer.Load(ms2Name)
	assert.True(t, ms1Value.(*modelServer).pods.Contains(podName))
	assert.True(t, ms2Value.(*modelServer).pods.Contains(podName))
}

// Test Case 6: Pod with multiple ModelServers
func TestStore_PodWithMultipleModelServers_ThenAddModelServers(t *testing.T) {
	s := newStoreWithMockBackend()

	ms1 := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	ms2 := createTestModelServer("default", "model2", aiv1alpha1.VLLM)
	pod := createTestPod("default", "pod1")

	ms1Name := utils.GetNamespaceName(ms1)
	ms2Name := utils.GetNamespaceName(ms2)
	podName := utils.GetNamespaceName(pod)

	// Add ModelServers first (current implementation requirement)
	err := s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName]())
	assert.NoError(t, err)
	err = s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// Add Pod with references to both ModelServers
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms1, ms2})
	assert.NoError(t, err)

	// Verify ModelServer references
	ms1Value, _ := s.modelServer.Load(ms1Name)
	ms2Value, _ := s.modelServer.Load(ms2Name)
	assert.NotNil(t, ms1Value)
	assert.NotNil(t, ms2Value)
	assert.True(t, ms1Value.(*modelServer).pods.Contains(podName))
	assert.True(t, ms2Value.(*modelServer).pods.Contains(podName))

	// Update ModelServers explicitly
	err = s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName](podName))
	assert.NoError(t, err)

	err = s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName](podName))
	assert.NoError(t, err)

	// Verify relationships are maintained
	podInfo := s.GetPodInfo(podName)
	assert.True(t, podInfo.HasModelServer(ms1Name))
	assert.True(t, podInfo.HasModelServer(ms2Name))
}

// Test Case 7: Update operations - changing Pod's ModelServer associations
func TestStore_UpdatePodModelServerAssociations(t *testing.T) {
	s := newStoreWithMockBackend()

	ms1 := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	ms2 := createTestModelServer("default", "model2", aiv1alpha1.VLLM)
	pod := createTestPod("default", "pod1")

	ms1Name := utils.GetNamespaceName(ms1)
	ms2Name := utils.GetNamespaceName(ms2)
	podName := utils.GetNamespaceName(pod)

	// Initial setup: Pod belongs to ms1
	err := s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName]())
	assert.NoError(t, err)
	err = s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms1})
	assert.NoError(t, err)

	// Verify initial state
	assert.True(t, s.GetPodInfo(podName).HasModelServer(ms1Name))
	assert.False(t, s.GetPodInfo(podName).HasModelServer(ms2Name))
	ms1Value, _ := s.modelServer.Load(ms1Name)
	ms2Value, _ := s.modelServer.Load(ms2Name)
	assert.True(t, ms1Value.(*modelServer).pods.Contains(podName))
	assert.False(t, ms2Value.(*modelServer).pods.Contains(podName))

	// Update: Pod now belongs to ms2 instead
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms2})
	assert.NoError(t, err)

	// Verify updated state - old associations should be replaced
	podInfo := s.GetPodInfo(podName)
	assert.False(t, podInfo.HasModelServer(ms1Name))
	assert.True(t, podInfo.HasModelServer(ms2Name))
	assert.Equal(t, 1, podInfo.GetModelServerCount())

	// Note: The current implementation may not automatically remove pod from old modelServer
	// This might be a design consideration for the actual implementation
}

// Test Case 8: Interleaved operations
func TestStore_InterleavedOperations(t *testing.T) {
	s := newStoreWithMockBackend()

	ms1 := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	ms2 := createTestModelServer("default", "model2", aiv1alpha1.VLLM)
	pod1 := createTestPod("default", "pod1")
	pod2 := createTestPod("default", "pod2")

	ms1Name := utils.GetNamespaceName(ms1)
	ms2Name := utils.GetNamespaceName(ms2)
	pod1Name := utils.GetNamespaceName(pod1)
	pod2Name := utils.GetNamespaceName(pod2)

	// Interleaved operations
	// 1. Add ms1
	err := s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// 2. Add pod1 to ms1
	err = s.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{ms1})
	assert.NoError(t, err)

	// 3. Add ms2
	err = s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName]())
	assert.NoError(t, err)

	// 4. Add pod2 to ms2
	err = s.AddOrUpdatePod(pod2, []*aiv1alpha1.ModelServer{ms2})
	assert.NoError(t, err)

	// 5. Update pod1 to belong to both ms1 and ms2
	err = s.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{ms1, ms2})
	assert.NoError(t, err)

	// Verify final state
	pod1Info := s.GetPodInfo(pod1Name)
	pod2Info := s.GetPodInfo(pod2Name)

	assert.True(t, pod1Info.HasModelServer(ms1Name))
	assert.True(t, pod1Info.HasModelServer(ms2Name))
	assert.Equal(t, 2, pod1Info.GetModelServerCount())

	assert.False(t, pod2Info.HasModelServer(ms1Name))
	assert.True(t, pod2Info.HasModelServer(ms2Name))
	assert.Equal(t, 1, pod2Info.GetModelServerCount())

	ms1Value, _ := s.modelServer.Load(ms1Name)
	ms2Value, _ := s.modelServer.Load(ms2Name)
	assert.True(t, ms1Value.(*modelServer).pods.Contains(pod1Name))
	assert.False(t, ms1Value.(*modelServer).pods.Contains(pod2Name))
	assert.True(t, ms2Value.(*modelServer).pods.Contains(pod1Name))
	assert.True(t, ms2Value.(*modelServer).pods.Contains(pod2Name))
}

// Test Case 9: Deletion scenarios
func TestStore_DeletionScenarios(t *testing.T) {
	s := newStoreWithMockBackend()

	ms1 := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	ms2 := createTestModelServer("default", "model2", aiv1alpha1.VLLM)
	pod1 := createTestPod("default", "pod1")
	pod2 := createTestPod("default", "pod2")

	ms1Name := utils.GetNamespaceName(ms1)
	ms2Name := utils.GetNamespaceName(ms2)
	pod1Name := utils.GetNamespaceName(pod1)
	pod2Name := utils.GetNamespaceName(pod2)

	// Setup: Both pods belong to both ModelServers
	err := s.AddOrUpdatePod(pod1, []*aiv1alpha1.ModelServer{ms1, ms2})
	assert.NoError(t, err)
	err = s.AddOrUpdatePod(pod2, []*aiv1alpha1.ModelServer{ms1, ms2})
	assert.NoError(t, err)

	// Add ModelServers with both pods
	s.AddOrUpdateModelServer(ms1, sets.New[types.NamespacedName](pod1Name, pod2Name))
	s.AddOrUpdateModelServer(ms2, sets.New[types.NamespacedName](pod1Name, pod2Name))

	// Verify initial setup
	assert.Equal(t, 2, s.GetPodInfo(pod1Name).GetModelServerCount())
	assert.Equal(t, 2, s.GetPodInfo(pod2Name).GetModelServerCount())

	// Delete pod1
	err = s.DeletePod(pod1Name)
	assert.NoError(t, err)

	// Verify pod1 is gone but pod2 remains
	podInfo := s.GetPodInfo(pod1Name)
	exists := podInfo != nil
	assert.False(t, exists)
	assert.NotNil(t, s.GetPodInfo(pod2Name))

	// Verify ModelServers no longer reference pod1 but still reference pod2
	ms1Value, _ := s.modelServer.Load(ms1Name)
	ms2Value, _ := s.modelServer.Load(ms2Name)
	assert.False(t, ms1Value.(*modelServer).pods.Contains(pod1Name))
	assert.True(t, ms1Value.(*modelServer).pods.Contains(pod2Name))
	assert.False(t, ms2Value.(*modelServer).pods.Contains(pod1Name))
	assert.True(t, ms2Value.(*modelServer).pods.Contains(pod2Name))

	// Delete ms1
	err = s.DeleteModelServer(types.NamespacedName{Namespace: ms1.Namespace, Name: ms1.Name})
	assert.NoError(t, err)

	// Verify ms1 is gone and pod2 no longer references it
	_, exists = s.modelServer.Load(ms1Name)
	assert.False(t, exists)
	assert.False(t, s.GetPodInfo(pod2Name).HasModelServer(ms1Name))
	assert.True(t, s.GetPodInfo(pod2Name).HasModelServer(ms2Name))
	assert.Equal(t, 1, s.GetPodInfo(pod2Name).GetModelServerCount())
}

// Test Case 10: Edge case - Empty operations
func TestStore_EdgeCases(t *testing.T) {
	s := New().(*store)

	// Delete non-existent pod
	err := s.DeletePod(types.NamespacedName{Namespace: "default", Name: "nonexistent"})
	assert.NoError(t, err)

	// Delete non-existent ModelServer
	ms := createTestModelServer("default", "nonexistent", aiv1alpha1.VLLM)
	err = s.DeleteModelServer(types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name})
	assert.NoError(t, err)

	// Get non-existent ModelServer
	result := s.GetModelServer(types.NamespacedName{Namespace: "default", Name: "nonexistent"})
	assert.Nil(t, result)

	// Get pods from non-existent ModelServer
	_, err = s.GetPodsByModelServer(types.NamespacedName{Namespace: "default", Name: "nonexistent"})
	assert.Error(t, err)

	// Add pod with empty ModelServer list
	pod := createTestPod("default", "pod1")
	err = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{})
	assert.NoError(t, err)

	podName := utils.GetNamespaceName(pod)
	podInfo := s.GetPodInfo(podName)
	assert.NotNil(t, podInfo)
	assert.Equal(t, 0, podInfo.GetModelServerCount())
}

// Test Case 11: random operations (simulated)
func TestStore_RandomOperations(t *testing.T) {
	s := newStoreWithMockBackend()

	// Simulate rapid add/update operations that might happen concurrently
	ms := createTestModelServer("default", "model1", aiv1alpha1.VLLM)
	pods := make([]*corev1.Pod, 5)
	for i := 0; i < 5; i++ {
		pods[i] = createTestPod("default", "pod"+string(rune('1'+i)))
	}

	msName := utils.GetNamespaceName(ms)

	wg := sync.WaitGroup{}
	wg.Add(len(pods))
	podSets := sets.NewWithLength[types.NamespacedName](5)
	for i := range pods {
		podSets.Insert(utils.GetNamespaceName(pods[i]))
		go func(p *corev1.Pod) {
			defer wg.Done()
			err := s.AddOrUpdatePod(p, []*aiv1alpha1.ModelServer{ms})
			assert.NoError(t, err)
		}(pods[i])
	}
	wg.Wait()

	err := s.AddOrUpdateModelServer(ms, podSets)
	assert.NoError(t, err)
	// Verify all relationships
	for _, pod := range pods {
		podName := utils.GetNamespaceName(pod)
		assert.True(t, s.GetPodInfo(podName).HasModelServer(msName))
		msValue, _ := s.modelServer.Load(msName)
		assert.True(t, msValue.(*modelServer).pods.Contains(podName))
	}

	retrievedPods, err := s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, retrievedPods, 5)

	// delete pod
	s.DeletePod(utils.GetNamespaceName(pods[0]))
	assert.Nil(t, s.GetPodInfo(utils.GetNamespaceName(pods[0])))
	msValue, _ := s.modelServer.Load(msName)
	assert.False(t, msValue.(*modelServer).pods.Contains(utils.GetNamespaceName(pods[0])))
	retrievedPods, err = s.GetPodsByModelServer(msName)
	assert.NoError(t, err)
	assert.Len(t, retrievedPods, 4)

	s.DeleteModelServer(types.NamespacedName{Namespace: ms.Namespace, Name: ms.Name})
	_, ok := s.modelServer.Load(msName)
	assert.False(t, ok)
	for i, pod := range pods {
		if i == 0 {
			continue
		}
		podName := utils.GetNamespaceName(pod)
		assert.Nil(t, s.GetPodInfo(podName))
	}
}
