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

package handlers

import (
	"encoding/json"
	"testing"

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCreatePatch(t *testing.T) {
	// Create an original model
	original := &v1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelBoosterSpec{
			AutoscalingPolicy: &v1alpha1.AutoscalingPolicySpec{},
			Backend: v1alpha1.ModelBackend{
				Name:        "backend1",
				Type:        "vLLM",
				ModelURI:    "hf://test/model",
				MinReplicas: 1,
				MaxReplicas: 10,
				Workers: []v1alpha1.ModelWorker{
					{
						Type:     "server",
						Image:    "test-image",
						Replicas: 1,
					},
				},
			},
		},
	}

	// Create a mutated model and apply the actual mutations
	mutated := original.DeepCopy()

	mutator := NewModelMutator() // No parameters needed since mutateModel doesn't use client
	mutator.mutateModel(mutated)

	// Test the createPatch function
	patch, err := createPatch(original, mutated)
	if err != nil {
		t.Fatalf("Error creating patch: %v", err)
	}

	// Verify that we got a valid patch
	if len(patch) == 0 {
		t.Fatal("Expected non-empty patch")
	}

	// Parse the patch to verify it's valid JSON
	var patchObj []interface{}
	t.Logf("patch: %v", patch)
	if err := json.Unmarshal(patch, &patchObj); err != nil {
		t.Fatalf("Error unmarshaling patch: %v", err)
	}

	// Verify that we have patch operations
	if len(patchObj) == 0 {
		t.Fatal("Expected patch operations")
	}

	t.Logf("Patch created successfully with %d operations", len(patchObj))

	// Log the patch for debugging
	for i, op := range patchObj {
		t.Logf("Operation %d: %+v", i+1, op)
	}
}

func TestCreatePatchNoChanges(t *testing.T) {
	// Create a model
	original := &v1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelBoosterSpec{
			Backend: v1alpha1.ModelBackend{
				Name:        "backend1",
				Type:        "vLLM",
				ModelURI:    "hf://test/model",
				MinReplicas: 1,
				MaxReplicas: 10,
				Workers: []v1alpha1.ModelWorker{
					{
						Type:     "server",
						Image:    "test-image",
						Replicas: 1,
					},
				},
			},
		},
	}

	// Create an identical copy
	mutated := original.DeepCopy()

	// Test the createPatch function
	patch, err := createPatch(original, mutated)
	if err != nil {
		t.Fatalf("Error creating patch: %v", err)
	}

	// Parse the patch
	var patchObj []interface{}
	if err := json.Unmarshal(patch, &patchObj); err != nil {
		t.Fatalf("Error unmarshaling patch: %v", err)
	}

	// Should have no operations for identical objects
	if len(patchObj) != 0 {
		t.Fatalf("Expected no patch operations for identical objects, got %d", len(patchObj))
	}

	t.Log("No patch operations created for identical objects - correct behavior")
}
