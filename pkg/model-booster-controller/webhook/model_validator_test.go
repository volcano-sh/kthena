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
			// This will trigger validation errors for autoscaling-related fields
			Backend: registryv1alpha1.ModelBackend{
				Name:        "backend1",
				Type:        registryv1alpha1.ModelBackendTypeVLLM,
				MinReplicas: 1,
				MaxReplicas: 3,
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
				Name:        "backend1",
				Type:        registryv1alpha1.ModelBackendTypeVLLM,
				MinReplicas: 1,
				MaxReplicas: 1,
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

// TestValidateBackendWorkerTypes_SGLang covers SGLang and SGLangDisaggregated
// worker-type rules.
func TestValidateBackendWorkerTypes_SGLang(t *testing.T) {
	validator := &ModelValidator{}
	mkModel := func(backendType registryv1alpha1.ModelBackendType, workers ...registryv1alpha1.ModelWorker) *registryv1alpha1.ModelBooster {
		return &registryv1alpha1.ModelBooster{
			ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"},
			Spec: registryv1alpha1.ModelBoosterSpec{
				Backend: registryv1alpha1.ModelBackend{
					Name:        "b1",
					Type:        backendType,
					MinReplicas: 1,
					MaxReplicas: 1,
					Workers:     workers,
				},
			},
		}
	}

	tests := []struct {
		name      string
		model     *registryv1alpha1.ModelBooster
		wantValid bool
		errSubstr string
	}{
		{
			name: "SGLang with exactly one server worker is valid",
			model: mkModel(registryv1alpha1.ModelBackendTypeSGLang,
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypeServer, Pods: 1, Image: "sglang:latest"},
			),
			wantValid: true,
		},
		{
			name: "SGLang with a non-server worker is rejected",
			model: mkModel(registryv1alpha1.ModelBackendTypeSGLang,
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypePrefill, Pods: 1, Image: "sglang:latest"},
			),
			wantValid: false,
			errSubstr: "worker type must be 'server'",
		},
		{
			name: "SGLangDisaggregated with prefill+decode is valid",
			model: mkModel(registryv1alpha1.ModelBackendTypeSGLangDisaggregated,
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypePrefill, Pods: 1, Image: "sglang:latest"},
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypeDecode, Pods: 1, Image: "sglang:latest"},
			),
			wantValid: true,
		},
		{
			name: "SGLangDisaggregated containing a server worker is rejected",
			model: mkModel(registryv1alpha1.ModelBackendTypeSGLangDisaggregated,
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypePrefill, Pods: 1, Image: "sglang:latest"},
				registryv1alpha1.ModelWorker{Type: registryv1alpha1.ModelWorkerTypeServer, Pods: 1, Image: "sglang:latest"},
			),
			wantValid: false,
			errSubstr: "SGLangDisaggregated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			valid, errorMsg := validator.validateModel(tt.model)
			assert.Equal(t, tt.wantValid, valid, "errorMsg=%s", errorMsg)
			if tt.errSubstr != "" {
				assert.Contains(t, errorMsg, tt.errSubstr)
			}
		})
	}
}
