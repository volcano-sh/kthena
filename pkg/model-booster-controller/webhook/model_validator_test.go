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

package webhook

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateModel_ErrorFormatting(t *testing.T) {
	validator := &ModelValidator{}

	// Create a model that will trigger multiple validation errors
	model := &registryv1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: registryv1alpha1.ModelBoosterSpec{
			// This will trigger validation errors for replica bounds.
			Backend: registryv1alpha1.ModelBackend{
				Name:     "backend1",
				Type:     registryv1alpha1.ModelBackendTypeVLLM,
				Replicas: 1000001,
				Workers: []registryv1alpha1.ModelWorker{
					{
						Type:  registryv1alpha1.ModelWorkerTypeServer,
						Pods:  1,
						Image: "test-image:latest",
					},
				},
			},
		},
	}

	valid, errorMsg := validator.validateModel(model)

	// Should not be valid due to multiple errors
	assert.False(t, valid)
	assert.NotEmpty(t, errorMsg)

	// Check that the error message is properly formatted
	assert.True(t, strings.HasPrefix(errorMsg, "validation failed:\n"))

	// Check that errors are formatted with bullet points and line breaks
	lines := strings.Split(errorMsg, "\n")
	assert.True(t, len(lines) > 1, "Error message should be multi-line")

	// Check that each error line (except the first) starts with "  - "
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" { // Skip empty lines
			assert.True(t, strings.HasPrefix(lines[i], "  - "),
				"Each error line should start with '  - ', but got: %q", lines[i])
		}
	}

	// Verify that the error message is more readable than the old format
	// (should not be in Go slice format like [error1 error2 error3])
	assert.False(t, strings.HasPrefix(strings.TrimSpace(strings.Split(errorMsg, "\n")[1]), "[") &&
		strings.HasSuffix(strings.TrimSpace(errorMsg), "]"),
		"Error message should not be in Go slice format")

	t.Logf("Formatted error message:\n%s", errorMsg)
}

func TestValidateModel_NoErrors(t *testing.T) {
	validator := &ModelValidator{}

	// Create a valid model
	model := &registryv1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: registryv1alpha1.ModelBoosterSpec{
			Backend: registryv1alpha1.ModelBackend{
				Name:     "backend1",
				Type:     registryv1alpha1.ModelBackendTypeVLLM,
				Replicas: 1,
				Workers: []registryv1alpha1.ModelWorker{
					{
						Type:  registryv1alpha1.ModelWorkerTypeServer,
						Pods:  1,
						Image: "test-image:latest",
					},
				},
			},
		},
	}

	valid, errorMsg := validator.validateModel(model)

	// Should be valid with no errors
	assert.True(t, valid)
	assert.Empty(t, errorMsg)
}

func TestValidatePVCURICompatibility(t *testing.T) {
	tests := []struct {
		name        string
		modelURI    string
		cacheURI    string
		expectValid bool
		expectMsg   string
	}{
		{
			name:        "hf modelURI with pvc cacheURI is valid",
			modelURI:    "hf://Qwen/Qwen2.5-7B-Instruct",
			cacheURI:    "pvc://model-cache",
			expectValid: true,
		},
		{
			name:        "hf modelURI with hostpath cacheURI is valid",
			modelURI:    "hf://Qwen/Qwen2.5-7B-Instruct",
			cacheURI:    "hostpath:///tmp/cache",
			expectValid: true,
		},
		{
			name:        "pvc modelURI with matching pvc cacheURI is valid",
			modelURI:    "pvc:///crater-storage/models/Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: true,
		},
		{
			name:        "pvc modelURI without leading slash in source is valid",
			modelURI:    "pvc://crater-storage/models/Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: true,
		},
		{
			name:        "pvc modelURI pointing to root of mounted pvc is valid",
			modelURI:    "pvc://my-pvc",
			cacheURI:    "pvc://my-pvc",
			expectValid: true,
		},
		{
			name:        "pvc modelURI with hostpath cacheURI is invalid",
			modelURI:    "pvc:///shared/models/Qwen",
			cacheURI:    "hostpath:///tmp/cache",
			expectValid: false,
			expectMsg:   "when modelURI uses pvc://, cacheURI must also use pvc://",
		},
		{
			name:        "pvc modelURI with empty cacheURI is invalid",
			modelURI:    "pvc:///shared/models/Qwen",
			cacheURI:    "",
			expectValid: false,
			expectMsg:   "when modelURI uses pvc://, cacheURI must also use pvc://",
		},
		{
			name:        "pvc modelURI path not under cacheURI mount is invalid",
			modelURI:    "pvc:///different-pvc/models/Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: false,
			expectMsg:   "is not reachable via cacheURI mount",
		},
		{
			name:        "pvc modelURI with mid-path traversal is invalid",
			modelURI:    "pvc:///crater-storage/../other-pvc/models/Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: false,
			expectMsg:   "must not contain '..' path segments",
		},
		{
			name:        "pvc modelURI with leading path traversal is invalid",
			modelURI:    "pvc://../other/models/Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: false,
			expectMsg:   "must not contain '..' path segments",
		},
		{
			name:        "pvc cacheURI with empty claim name is invalid",
			modelURI:    "pvc:///shared/models/Qwen",
			cacheURI:    "pvc://",
			expectValid: false,
			expectMsg:   "must contain a single PVC claim name with no path separator",
		},
		{
			name:        "pvc cacheURI with slashed claim name is invalid",
			modelURI:    "pvc:///foo/bar/models/Qwen",
			cacheURI:    "pvc://foo/bar",
			expectValid: false,
			expectMsg:   "claim names cannot contain '/'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &registryv1alpha1.ModelBooster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "default",
				},
				Spec: registryv1alpha1.ModelBoosterSpec{
					Backend: registryv1alpha1.ModelBackend{
						Name:     "backend1",
						Type:     registryv1alpha1.ModelBackendTypeVLLM,
						ModelURI: tt.modelURI,
						CacheURI: tt.cacheURI,
						Replicas: 1,
						Workers: []registryv1alpha1.ModelWorker{
							{
								Type:  registryv1alpha1.ModelWorkerTypeServer,
								Pods:  1,
								Image: "test-image:latest",
							},
						},
					},
				},
			}

			valid, errorMsg := (&ModelValidator{}).validateModel(model)

			if tt.expectValid {
				assert.True(t, valid, "expected valid but got error: %s", errorMsg)
				assert.Empty(t, errorMsg)
			} else {
				assert.False(t, valid)
				assert.Contains(t, errorMsg, tt.expectMsg)
			}
		})
	}
}

func TestPVCModelSourcePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"pvc:///crater-storage/models/Qwen", "/crater-storage/models/Qwen"},
		{"pvc://crater-storage/models/Qwen", "/crater-storage/models/Qwen"},
		{"pvc://my-pvc", "/my-pvc"},
		{"pvc:///my-pvc", "/my-pvc"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := pvcModelSourcePath(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCacheVolumeMountPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"pvc://crater-storage", "/crater-storage"},
		{"pvc:///crater-storage", "/crater-storage"},
		{"hostpath:///tmp/cache", "/tmp/cache"},
		{"hostpath://tmp/cache", "/tmp/cache"},
		{"", ""},
		{"invalid", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := cacheVolumeMountPath(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}
