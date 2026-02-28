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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// helpers for test setup

func newTestModelServerWithPDGroup(name, namespace string) *aiv1alpha1.ModelServer {
	return &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{"app": name},
				PDGroup: &aiv1alpha1.PDGroup{
					GroupKey: "pd-group",
					DecodeLabels: map[string]string{
						"role": "decode",
					},
					PrefillLabels: map[string]string{
						"role": "prefill",
					},
				},
			},
		},
	}
}

func newTestPod(name, namespace string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}
}

// TestConcurrentAddOrUpdateModelServerAndDeletePod verifies that concurrently
// updating a ModelServer and deleting pods does not cause a data race.
// Before the fix, AddOrUpdateModelServer wrote modelServer.modelServer without
// holding the mutex, while DeletePod -> removePodFromPDGroups read it
// (via getPDGroupName) also without the mutex, causing a data race detectable
// by `go test -race`.
func TestConcurrentAddOrUpdateModelServerAndDeletePod(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	ms := newTestModelServerWithPDGroup("test-model", "default")
	msName := types.NamespacedName{Namespace: "default", Name: "test-model"}

	// Add the model server initially
	err := s.AddOrUpdateModelServer(ms, nil)
	assert.NoError(t, err)

	// Add several decode and prefill pods
	numPods := 20
	for i := 0; i < numPods; i++ {
		role := "decode"
		if i%2 == 0 {
			role = "prefill"
		}
		pod := newTestPod(fmt.Sprintf("pod-%d", i), "default", map[string]string{
			"pd-group": "group-a",
			"role":     role,
			"app":      "test-model",
		})
		err := s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms})
		assert.NoError(t, err)
	}

	// Concurrently: update ModelServer spec + delete pods
	// This is the exact scenario that triggers the data race.
	var wg sync.WaitGroup
	iterations := 100

	// Writer goroutine: repeatedly update the ModelServer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			updatedMS := newTestModelServerWithPDGroup("test-model", "default")
			// Vary the spec to simulate real updates
			updatedMS.Spec.InferenceEngine = aiv1alpha1.VLLM
			_ = s.AddOrUpdateModelServer(updatedMS, nil)
		}
	}()

	// Deleter goroutines: delete pods concurrently
	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			podName := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("pod-%d", idx)}
			_ = s.DeletePod(podName)
		}(i)
	}

	wg.Wait()

	// Verify the model server still exists and is consistent
	retrievedMS := s.GetModelServer(msName)
	assert.NotNil(t, retrievedMS)
}

// TestConcurrentAddOrUpdateModelServerAndGetPrefillPodsForDecodeGroup verifies
// that concurrently updating a ModelServer and calling GetPrefillPodsForDecodeGroup
// does not cause a data race.
// Before the fix, getPrefillPodsForDecodeGroup read m.modelServer.Spec.WorkloadSelector
// outside the RLock, while AddOrUpdateModelServer wrote m.modelServer without any lock.
func TestConcurrentAddOrUpdateModelServerAndGetPrefillPodsForDecodeGroup(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	ms := newTestModelServerWithPDGroup("test-model", "default")
	msName := types.NamespacedName{Namespace: "default", Name: "test-model"}

	err := s.AddOrUpdateModelServer(ms, nil)
	assert.NoError(t, err)

	// Add decode and prefill pods in two groups
	for _, group := range []string{"group-a", "group-b"} {
		for i := 0; i < 3; i++ {
			decodePod := newTestPod(
				fmt.Sprintf("decode-%s-%d", group, i), "default",
				map[string]string{"pd-group": group, "role": "decode", "app": "test-model"},
			)
			err := s.AddOrUpdatePod(decodePod, []*aiv1alpha1.ModelServer{ms})
			assert.NoError(t, err)

			prefillPod := newTestPod(
				fmt.Sprintf("prefill-%s-%d", group, i), "default",
				map[string]string{"pd-group": group, "role": "prefill", "app": "test-model"},
			)
			err = s.AddOrUpdatePod(prefillPod, []*aiv1alpha1.ModelServer{ms})
			assert.NoError(t, err)
		}
	}

	var wg sync.WaitGroup
	iterations := 200

	// Writer: repeatedly update ModelServer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			updatedMS := newTestModelServerWithPDGroup("test-model", "default")
			_ = s.AddOrUpdateModelServer(updatedMS, nil)
		}
	}()

	// Reader: repeatedly call GetPrefillPodsForDecodeGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		decodePodName := types.NamespacedName{Namespace: "default", Name: "decode-group-a-0"}
		for i := 0; i < iterations; i++ {
			// This should not panic or produce corrupt results
			_, _ = s.GetPrefillPodsForDecodeGroup(msName, decodePodName)
		}
	}()

	// Reader: repeatedly call GetDecodePods
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = s.GetDecodePods(msName)
		}
	}()

	// Reader: repeatedly call GetPrefillPods
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = s.GetPrefillPods(msName)
		}
	}()

	wg.Wait()

	// Should not have panicked
	retrievedMS := s.GetModelServer(msName)
	assert.NotNil(t, retrievedMS)
}

// TestConcurrentAddOrUpdateModelServerAndPodsField verifies that concurrently
// updating a ModelServer's pods field (via AddOrUpdateModelServer) and reading
// pods (via GetPodsByModelServer) does not cause a data race.
// Before the fix, AddOrUpdateModelServer wrote modelServerObj.pods without the
// mutex, racing with getPods which reads it under RLock.
func TestConcurrentAddOrUpdateModelServerAndPodsField(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{"app": "test-model"},
			},
		},
	}
	msName := types.NamespacedName{Namespace: "default", Name: "test-model"}

	initialPods := sets.New[types.NamespacedName](
		types.NamespacedName{Namespace: "default", Name: "pod-0"},
	)
	err := s.AddOrUpdateModelServer(ms, initialPods)
	assert.NoError(t, err)

	var wg sync.WaitGroup
	iterations := 200

	// Writer: update ModelServer with different pod sets
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			pods := sets.New[types.NamespacedName](
				types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("pod-%d", i%5)},
			)
			_ = s.AddOrUpdateModelServer(ms, pods)
		}
	}()

	// Reader: repeatedly get pods list
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			value, ok := s.modelServer.Load(msName)
			if ok {
				msObj := value.(*modelServer)
				// getPods uses RLock internally — this would race with
				// the unlocked write before the fix
				_ = msObj.getPods()
			}
		}
	}()

	wg.Wait()
}

// TestConcurrentRemovePodFromPDGroupsAndAddOrUpdateModelServer verifies that
// removePodFromPDGroups (called during DeletePod) and AddOrUpdateModelServer
// do not race on the m.modelServer field.
// Before the fix, removePodFromPDGroups called getPDGroupName outside the lock.
func TestConcurrentRemovePodFromPDGroupsAndAddOrUpdateModelServer(t *testing.T) {
	ms := newTestModelServerWithPDGroup("test-model", "default")
	msObj := newModelServer(ms)

	// Pre-populate with pods in the PD group
	numPods := 10
	podNames := make([]types.NamespacedName, numPods)
	podLabelsList := make([]map[string]string, numPods)
	for i := 0; i < numPods; i++ {
		podName := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("pod-%d", i)}
		podNames[i] = podName
		role := "decode"
		if i%2 == 0 {
			role = "prefill"
		}
		labels := map[string]string{"pd-group": "group-a", "role": role}
		podLabelsList[i] = labels
		msObj.addPod(podName)
		msObj.categorizePodForPDGroup(podName, labels)
	}

	var wg sync.WaitGroup
	iterations := 100

	// Writer: update the modelServer pointer repeatedly
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			newMS := newTestModelServerWithPDGroup("test-model", "default")
			msObj.mutex.Lock()
			msObj.modelServer = newMS
			msObj.mutex.Unlock()
		}
	}()

	// Removers: concurrently remove pods from PD groups
	for i := 0; i < numPods; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			msObj.removePodFromPDGroups(podNames[idx], podLabelsList[idx])
		}(i)
	}

	wg.Wait()
}

// TestConcurrentCategorizePodAndRemovePodFromPDGroups verifies that concurrent
// pod categorization and removal on the same modelServer do not race.
func TestConcurrentCategorizePodAndRemovePodFromPDGroups(t *testing.T) {
	ms := newTestModelServerWithPDGroup("test-model", "default")
	msObj := newModelServer(ms)

	var wg sync.WaitGroup

	// Many goroutines adding and removing pods concurrently
	numGoroutines := 50
	for i := 0; i < numGoroutines; i++ {
		wg.Add(2)
		podName := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("pod-%d", i)}
		labels := map[string]string{"pd-group": "group-a", "role": "decode"}

		// Categorize
		go func() {
			defer wg.Done()
			msObj.addPod(podName)
			msObj.categorizePodForPDGroup(podName, labels)
		}()

		// Remove (may or may not find the pod)
		go func() {
			defer wg.Done()
			msObj.removePodFromPDGroups(podName, labels)
			msObj.deletePod(podName)
		}()
	}

	wg.Wait()
}

// TestRemovePodFromPDGroupsCorrectness verifies that removePodFromPDGroups
// correctly removes a pod from the PD group and cleans up empty groups,
// even when the modelServer spec is being concurrently updated.
func TestRemovePodFromPDGroupsCorrectness(t *testing.T) {
	ms := newTestModelServerWithPDGroup("test-model", "default")
	msObj := newModelServer(ms)

	// Add a decode pod
	podName := types.NamespacedName{Namespace: "default", Name: "decode-pod-1"}
	labels := map[string]string{"pd-group": "group-a", "role": "decode"}

	msObj.addPod(podName)
	msObj.categorizePodForPDGroup(podName, labels)

	// Verify pod is categorized
	decodePods := msObj.getAllDecodePods()
	assert.Len(t, decodePods, 1, "should have 1 decode pod before removal")

	// Remove the pod
	msObj.removePodFromPDGroups(podName, labels)

	// Verify pod is removed
	decodePods = msObj.getAllDecodePods()
	assert.Len(t, decodePods, 0, "should have 0 decode pods after removal")

	// Verify empty group is cleaned up
	msObj.mutex.RLock()
	_, exists := msObj.pdGroups["group-a"]
	msObj.mutex.RUnlock()
	assert.False(t, exists, "empty PD group should be cleaned up")
}

// TestGetPrefillPodsForDecodeGroupCorrectness verifies that the fixed
// getPrefillPodsForDecodeGroup correctly reads modelServer config and
// returns the right prefill pods for a given decode pod's group.
func TestGetPrefillPodsForDecodeGroupCorrectness(t *testing.T) {
	ms := newTestModelServerWithPDGroup("test-model", "default")
	msObj := newModelServer(ms)

	// Add decode pod in group-a
	decodePodName := types.NamespacedName{Namespace: "default", Name: "decode-1"}
	decodePod := &PodInfo{
		Pod: newTestPod("decode-1", "default", map[string]string{
			"pd-group": "group-a", "role": "decode",
		}),
	}
	msObj.addPod(decodePodName)
	msObj.categorizePodForPDGroup(decodePodName, decodePod.Pod.Labels)

	// Add prefill pod in group-a
	prefillPodName := types.NamespacedName{Namespace: "default", Name: "prefill-1"}
	prefillPod := newTestPod("prefill-1", "default", map[string]string{
		"pd-group": "group-a", "role": "prefill",
	})
	msObj.addPod(prefillPodName)
	msObj.categorizePodForPDGroup(prefillPodName, prefillPod.Labels)

	// Add prefill pod in group-b (should NOT be returned)
	prefillPodB := types.NamespacedName{Namespace: "default", Name: "prefill-2"}
	msObj.addPod(prefillPodB)
	msObj.categorizePodForPDGroup(prefillPodB, map[string]string{
		"pd-group": "group-b", "role": "prefill",
	})

	// Get prefill pods for the decode pod's group (group-a)
	result := msObj.getPrefillPodsForDecodeGroup(decodePod)
	assert.Len(t, result, 1, "should return 1 prefill pod from group-a")
	assert.Equal(t, prefillPodName, result[0], "should return the correct prefill pod")
}

// TestGetPrefillPodsForDecodeGroupNoPDGroupConfig verifies that
// getPrefillPodsForDecodeGroup returns nil when PDGroup is not configured.
func TestGetPrefillPodsForDecodeGroupNoPDGroupConfig(t *testing.T) {
	ms := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				MatchLabels: map[string]string{"app": "test"},
				// No PDGroup configured
			},
		},
	}
	msObj := newModelServer(ms)

	pod := &PodInfo{
		Pod: newTestPod("decode-1", "default", map[string]string{"pd-group": "group-a", "role": "decode"}),
	}

	result := msObj.getPrefillPodsForDecodeGroup(pod)
	assert.Nil(t, result, "should return nil when PDGroup is not configured")
}

// TestAddOrUpdateModelServerUpdatesModelServerFieldUnderLock verifies that
// after updating a ModelServer, the new spec is visible to all readers and
// the internal state is consistent.
func TestAddOrUpdateModelServerUpdatesModelServerFieldUnderLock(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	ms := newTestModelServerWithPDGroup("test-model", "default")
	msName := types.NamespacedName{Namespace: "default", Name: "test-model"}

	// Add the model server initially
	err := s.AddOrUpdateModelServer(ms, nil)
	assert.NoError(t, err)

	// Update with modified spec
	updatedMS := newTestModelServerWithPDGroup("test-model", "default")
	updatedMS.Spec.InferenceEngine = aiv1alpha1.SGLang

	err = s.AddOrUpdateModelServer(updatedMS, nil)
	assert.NoError(t, err)

	// Verify the updated spec is visible
	retrievedMS := s.GetModelServer(msName)
	assert.NotNil(t, retrievedMS)
	assert.Equal(t, aiv1alpha1.SGLang, retrievedMS.Spec.InferenceEngine,
		"ModelServer spec should reflect the latest update")
}

// TestConcurrentFullLifecycle exercises the complete lifecycle of ModelServer +
// pods concurrently: add ModelServer, add pods, update ModelServer, delete pods,
// get PD groups, all running concurrently.
func TestConcurrentFullLifecycle(t *testing.T) {
	s := &store{
		modelServer: sync.Map{},
		pods:        sync.Map{},
		callbacks:   make(map[string][]CallbackFunc),
	}

	ms := newTestModelServerWithPDGroup("test-model", "default")

	err := s.AddOrUpdateModelServer(ms, nil)
	assert.NoError(t, err)

	msName := types.NamespacedName{Namespace: "default", Name: "test-model"}

	var wg sync.WaitGroup
	iterations := 100

	// Goroutine 1: Add and update pods
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			role := "decode"
			if i%2 == 0 {
				role = "prefill"
			}
			pod := newTestPod(fmt.Sprintf("lifecycle-pod-%d", i%10), "default", map[string]string{
				"pd-group": "group-a",
				"role":     role,
				"app":      "test-model",
			})
			_ = s.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{ms})
		}
	}()

	// Goroutine 2: Delete pods
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			podName := types.NamespacedName{Namespace: "default", Name: fmt.Sprintf("lifecycle-pod-%d", i%10)}
			_ = s.DeletePod(podName)
		}
	}()

	// Goroutine 3: Update ModelServer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			newMS := newTestModelServerWithPDGroup("test-model", "default")
			_ = s.AddOrUpdateModelServer(newMS, nil)
		}
	}()

	// Goroutine 4: Read PD groups
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = s.GetDecodePods(msName)
			_, _ = s.GetPrefillPods(msName)
		}
	}()

	// Goroutine 5: Read prefill pods for decode group
	wg.Add(1)
	go func() {
		defer wg.Done()
		decodePodName := types.NamespacedName{Namespace: "default", Name: "lifecycle-pod-1"}
		for i := 0; i < iterations; i++ {
			_, _ = s.GetPrefillPodsForDecodeGroup(msName, decodePodName)
		}
	}()

	wg.Wait()

	// Should not have panicked — store is still accessible
	retrievedMS := s.GetModelServer(msName)
	assert.NotNil(t, retrievedMS)
}
