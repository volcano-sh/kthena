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
	"testing"

	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestPDGroup(t *testing.T) {
	store := New()

	// Create a ModelServer with PDGroup configuration
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
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

	modelServerName := types.NamespacedName{
		Namespace: "default",
		Name:      "test-model",
	}

	// Add the ModelServer to store
	err := store.AddOrUpdateModelServer(modelServer, nil)
	if err != nil {
		t.Fatalf("Failed to add model server: %v", err)
	}

	// Create test pods
	decodePod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decode-pod-1",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-a",
				"role":     "decode",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}

	decodePod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decode-pod-2",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-b",
				"role":     "decode",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.2",
		},
	}

	prefillPod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prefill-pod-1",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-a",
				"role":     "prefill",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.3",
		},
	}

	prefillPod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prefill-pod-2",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-b",
				"role":     "prefill",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.4",
		},
	}

	// Add pods to store
	err = store.AddOrUpdatePod(decodePod1, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add decode pod 1: %v", err)
	}

	err = store.AddOrUpdatePod(decodePod2, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add decode pod 2: %v", err)
	}

	err = store.AddOrUpdatePod(prefillPod1, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add prefill pod 1: %v", err)
	}

	err = store.AddOrUpdatePod(prefillPod2, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add prefill pod 2: %v", err)
	}

	// Test GetDecodePods
	decodePods, err := store.GetDecodePods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get decode pods: %v", err)
	}

	if len(decodePods) != 2 {
		t.Errorf("Expected 2 decode pods, got %d", len(decodePods))
	}

	// Test GetPrefillPods
	prefillPods, err := store.GetPrefillPods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get prefill pods: %v", err)
	}

	if len(prefillPods) != 2 {
		t.Errorf("Expected 2 prefill pods, got %d", len(prefillPods))
	}

	// Test GetPrefillPodsForDecodeGroup
	decodePod1Name := types.NamespacedName{
		Namespace: "default",
		Name:      "decode-pod-1",
	}

	matchingPrefillPods, err := store.GetPrefillPodsForDecodeGroup(modelServerName, decodePod1Name)
	if err != nil {
		t.Fatalf("Failed to get prefill pods for decode group: %v", err)
	}

	if len(matchingPrefillPods) != 1 {
		t.Errorf("Expected 1 prefill pod for decode group, got %d", len(matchingPrefillPods))
	}

	if len(matchingPrefillPods) > 0 && matchingPrefillPods[0].Pod.Name != prefillPod1.Name {
		t.Errorf("Expected prefill-pod-1, got %s", matchingPrefillPods[0].Pod.Name)
	}

	// Test with decode-pod-2 (group-b)
	decodePod2Name := types.NamespacedName{
		Namespace: "default",
		Name:      "decode-pod-2",
	}

	matchingPrefillPods2, err := store.GetPrefillPodsForDecodeGroup(modelServerName, decodePod2Name)
	if err != nil {
		t.Fatalf("Failed to get prefill pods for decode group: %v", err)
	}

	if len(matchingPrefillPods2) != 1 {
		t.Errorf("Expected 1 prefill pod for decode group, got %d", len(matchingPrefillPods2))
	}

	if len(matchingPrefillPods2) > 0 && matchingPrefillPods2[0].Pod.Name != "prefill-pod-2" {
		t.Errorf("Expected prefill-pod-2, got %s", matchingPrefillPods2[0].Pod.Name)
	}
}

func TestPDGroupPodRemoval(t *testing.T) {
	store := New()

	// Create a ModelServer with PDGroup configuration
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
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

	modelServerName := types.NamespacedName{
		Namespace: "default",
		Name:      "test-model",
	}

	// Add the ModelServer to store
	err := store.AddOrUpdateModelServer(modelServer, nil)
	if err != nil {
		t.Fatalf("Failed to add model server: %v", err)
	}

	// Create and add a decode pod
	decodePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "decode-pod",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-a",
				"role":     "decode",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}

	err = store.AddOrUpdatePod(decodePod, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add decode pod: %v", err)
	}

	// Verify pod is categorized
	decodePods, err := store.GetDecodePods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get decode pods: %v", err)
	}

	if len(decodePods) != 1 {
		t.Errorf("Expected 1 decode pod, got %d", len(decodePods))
	}

	// Remove the pod
	podName := types.NamespacedName{
		Namespace: "default",
		Name:      "decode-pod",
	}

	err = store.DeletePod(podName)
	if err != nil {
		t.Fatalf("Failed to delete pod: %v", err)
	}

	// Verify pod is removed from categorization
	decodePods, err = store.GetDecodePods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get decode pods after deletion: %v", err)
	}

	if len(decodePods) != 0 {
		t.Errorf("Expected 0 decode pods after deletion, got %d", len(decodePods))
	}
}

func TestPDGroupSpecChangeRecategorization(t *testing.T) {
	store := New()

	modelServerName := types.NamespacedName{
		Namespace: "default",
		Name:      "test-model",
	}

	// 1. Create ModelServer with initial PDGroup config
	modelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				PDGroup: &aiv1alpha1.PDGroup{
					GroupKey: "pd-group",
					PrefillLabels: map[string]string{
						"role": "prefill",
					},
					DecodeLabels: map[string]string{
						"role": "decode",
					},
				},
			},
		},
	}

	err := store.AddOrUpdateModelServer(modelServer, nil)
	if err != nil {
		t.Fatalf("Failed to add model server: %v", err)
	}

	// 2. Create and add a pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				"pd-group": "group-a",
				"role":     "prefill",
			},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
		},
	}

	err = store.AddOrUpdatePod(pod, []*aiv1alpha1.ModelServer{modelServer})
	if err != nil {
		t.Fatalf("Failed to add pod: %v", err)
	}

	// Verify pod is categorized as a prefill pod
	prefillPods, err := store.GetPrefillPods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get prefill pods: %v", err)
	}
	if len(prefillPods) != 1 {
		t.Errorf("Expected 1 prefill pod, got %d", len(prefillPods))
	}

	// 3. Update ModelServer PDGroup config, changing prefill label requirements while keeping the same GroupKey
	updatedModelServer := &aiv1alpha1.ModelServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: aiv1alpha1.ModelServerSpec{
			WorkloadSelector: &aiv1alpha1.WorkloadSelector{
				PDGroup: &aiv1alpha1.PDGroup{
					GroupKey: "pd-group",
					PrefillLabels: map[string]string{
						"role": "prefill-new", // changed
					},
					DecodeLabels: map[string]string{
						"role": "decode",
					},
				},
			},
		},
	}

	// Associated pods inside modelServer object
	podsSet := sets.New(types.NamespacedName{Namespace: "default", Name: "test-pod"})

	err = store.AddOrUpdateModelServer(updatedModelServer, podsSet)
	if err != nil {
		t.Fatalf("Failed to update model server: %v", err)
	}

	// Verify that the old prefill categorization has been cleared, and the pod is no longer categorized as prefill
	prefillPods, err = store.GetPrefillPods(modelServerName)
	if err != nil {
		t.Fatalf("Failed to get prefill pods after update: %v", err)
	}
	if len(prefillPods) != 0 {
		t.Errorf("Expected 0 prefill pods after PDGroup prefill label update, got %d", len(prefillPods))
	}
}
