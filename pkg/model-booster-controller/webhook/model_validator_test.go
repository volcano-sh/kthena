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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

func TestValidateWorkerImagesRejectsWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		image string
	}{
		{name: "space", image: "example/image:latest "},
		{name: "tab", image: "example/image:\tlatest"},
		{name: "newline", image: "example/image:latest\n"},
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
						Replicas: 1,
						Workers: []registryv1alpha1.ModelWorker{{
							Type:  registryv1alpha1.ModelWorkerTypeServer,
							Pods:  1,
							Image: tt.image,
						}},
					},
				},
			}

			valid, errorMsg := (&ModelValidator{}).validateModel(model)
			assert.False(t, valid)
			assert.Contains(t, errorMsg, "image cannot contain whitespace")
		})
	}
}

func TestValidateEnginePorts(t *testing.T) {
	tests := []struct {
		name            string
		ports           []string
		expectErrMsg    string
		expectErrorPath string
	}{
		{
			name:  "matching numeric ports",
			ports: []string{"9000", "9000"},
		},
		{
			name:  "matching string ports",
			ports: []string{`"9000"`, `"9000"`},
		},
		{
			name:            "null port",
			ports:           []string{"null", "8000"},
			expectErrMsg:    "invalid port",
			expectErrorPath: "spec.backend.workers[0].config.port",
		},
		{
			name:            "different ports",
			ports:           []string{"9000", "8000"},
			expectErrMsg:    "workers use different engine ports",
			expectErrorPath: "spec.backend.workers[1].config.port",
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
						Type:     registryv1alpha1.ModelBackendTypeVLLMDisaggregated,
						Replicas: 1,
						Workers: []registryv1alpha1.ModelWorker{
							{
								Type:  registryv1alpha1.ModelWorkerTypePrefill,
								Pods:  1,
								Image: "test-image:latest",
								Config: apiextensionsv1.JSON{
									Raw: []byte(fmt.Sprintf(`{"port":%s}`, tt.ports[0])),
								},
							},
							{
								Type:  registryv1alpha1.ModelWorkerTypeDecode,
								Pods:  1,
								Image: "test-image:latest",
								Config: apiextensionsv1.JSON{
									Raw: []byte(fmt.Sprintf(`{"port":%s}`, tt.ports[1])),
								},
							},
						},
					},
				},
			}

			valid, errorMsg := (&ModelValidator{}).validateModel(model)
			if tt.expectErrMsg == "" {
				assert.True(t, valid, "expected valid but got error: %s", errorMsg)
				return
			}
			assert.False(t, valid)
			assert.Contains(t, errorMsg, tt.expectErrMsg)
			assert.Contains(t, errorMsg, tt.expectErrorPath)
			assert.NotContains(t, errorMsg, "test-image:latest")
		})
	}
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
			name:        "pvc modelURI with trailing slash is valid",
			modelURI:    "pvc://crater-storage/models/Qwen/",
			cacheURI:    "pvc://crater-storage",
			expectValid: true,
		},
		{
			name:        "pvc cacheURI with trailing slash is valid",
			modelURI:    "pvc:///crater-storage/models/Qwen",
			cacheURI:    "pvc://crater-storage/",
			expectValid: true,
		},
		{
			name:        "pvc modelURI with repeated slashes is valid",
			modelURI:    "pvc://crater-storage//models//Qwen",
			cacheURI:    "pvc://crater-storage",
			expectValid: true,
		},
		{
			name:        "s3 modelURI with non-pvc cacheURI is valid",
			modelURI:    "s3://bucket/models/Qwen",
			cacheURI:    "hostpath:///tmp/cache",
			expectValid: true,
		},
		{
			name:        "obs modelURI with non-pvc cacheURI is valid",
			modelURI:    "obs://bucket/models/Qwen",
			cacheURI:    "hostpath:///tmp/cache",
			expectValid: true,
		},
		{
			name:        "ms modelURI with non-pvc cacheURI is valid",
			modelURI:    "ms://namespace/repo",
			cacheURI:    "hostpath:///tmp/cache",
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
		{
			// The claim name "foo" must not be treated as a prefix match against a
			// modelURI whose first path segment is "foobar" - only a full path-segment
			// match (exact or followed by '/') makes the source reachable through the mount.
			name:        "pvc modelURI with claim-name-prefix collision is invalid",
			modelURI:    "pvc:///foobar/models/Qwen",
			cacheURI:    "pvc://foo",
			expectValid: false,
			expectMsg:   "is not reachable via cacheURI mount",
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

func nixlWorker(workerType registryv1alpha1.ModelWorkerType, role string) registryv1alpha1.ModelWorker {
	return registryv1alpha1.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"NixlConnector\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func mooncakeWorker(workerType registryv1alpha1.ModelWorkerType, role string) registryv1alpha1.ModelWorker {
	return registryv1alpha1.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"MooncakeConnector\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func mooncakeV1Worker(workerType registryv1alpha1.ModelWorkerType, role string) registryv1alpha1.ModelWorker {
	return registryv1alpha1.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"MooncakeConnectorV1\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func TestValidateKvConnectorConfig(t *testing.T) {
	tests := []struct {
		name        string
		workers     []registryv1alpha1.ModelWorker
		expectValid bool
		expectMsg   string
	}{
		{
			name: "prefill NIXL + decode Mooncake is rejected",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "kv_connector mismatch between prefill",
		},
		{
			name: "prefill Mooncake + decode NIXL is rejected",
			workers: []registryv1alpha1.ModelWorker{
				mooncakeWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "kv_connector mismatch between prefill",
		},
		{
			name: "prefill NIXL + decode NIXL succeeds",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: true,
		},
		{
			name: "worker order does not change the result",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
			},
			expectValid: true,
		},
		{
			name: "prefill missing kv-transfer-config is rejected",
			workers: []registryv1alpha1.ModelWorker{
				{Type: registryv1alpha1.ModelWorkerTypePrefill},
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "prefill worker has no kv-transfer-config",
		},
		{
			name: "decode missing kv-transfer-config is rejected",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				{Type: registryv1alpha1.ModelWorkerTypeDecode},
			},
			expectValid: false,
			expectMsg:   "decode worker has no kv-transfer-config",
		},
		{
			name: "missing kv_connector is rejected",
			workers: []registryv1alpha1.ModelWorker{
				{
					Type: registryv1alpha1.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{\"kv_role\":\"kv_producer\"}"}`),
					},
				},
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "kv_connector is missing",
		},
		{
			name: "unknown kv_connector is rejected",
			workers: []registryv1alpha1.ModelWorker{
				{
					Type: registryv1alpha1.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"UnknownConnector\",\"kv_role\":\"kv_producer\"}"}`),
					},
				},
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "unsupported kv_connector",
		},
		{
			name: "prefill with kv_consumer role is rejected",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_consumer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "invalid kv_role",
		},
		{
			name: "decode with kv_producer role is rejected",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_producer"),
			},
			expectValid: false,
			expectMsg:   "invalid kv_role",
		},
		{
			name: "PD backend with no kv-transfer-config on either worker remains valid",
			workers: []registryv1alpha1.ModelWorker{
				{Type: registryv1alpha1.ModelWorkerTypePrefill},
				{Type: registryv1alpha1.ModelWorkerTypeDecode},
			},
			expectValid: true,
		},
		{
			// Matches the vllm-ascend connector name used by the existing
			// examples/model-booster/prefill-decode-disaggregation.yaml ModelBooster example.
			name: "matching MooncakeConnectorV1 succeeds",
			workers: []registryv1alpha1.ModelWorker{
				mooncakeV1Worker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeV1Worker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: true,
		},
		{
			name: "duplicate prefill workers are rejected regardless of order",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "found 2 prefill workers, expected at most 1",
		},
		{
			name: "duplicate decode workers are rejected regardless of order",
			workers: []registryv1alpha1.ModelWorker{
				nixlWorker(registryv1alpha1.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
				nixlWorker(registryv1alpha1.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectValid: false,
			expectMsg:   "found 2 decode workers, expected at most 1",
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
						Type:     registryv1alpha1.ModelBackendTypeVLLMDisaggregated,
						Replicas: 1,
						Workers:  tt.workers,
					},
				},
			}

			valid, errorMsg := (&ModelValidator{}).validateModel(model)

			if tt.expectValid {
				assert.True(t, valid, "expected valid but got error: %s", errorMsg)
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

func TestValidateImageField(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		wantErr bool
	}{
		{
			name:    "valid image without tag",
			image:   "nginx",
			wantErr: false,
		},
		{
			name:    "valid image with tag",
			image:   "nginx:latest",
			wantErr: false,
		},
		{
			name:    "valid image with registry",
			image:   "docker.io/library/nginx:1.19",
			wantErr: false,
		},
		{
			name:    "empty image",
			image:   "",
			wantErr: false, // Optional fields return nil in validateImageField
		},
		{
			name:    "whitespace only",
			image:   "   ",
			wantErr: true,
		},
		{
			name:    "image with spaces",
			image:   "nginx:latest ",
			wantErr: true,
		},
		{
			name:    "image with internal spaces",
			image:   "my registry/nginx:latest",
			wantErr: true,
		},
		{
			name:    "image with tabs",
			image:   "nginx\t:latest",
			wantErr: true,
		},
		{
			name:    "image with newline",
			image:   "nginx\n:latest",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateImageField(tt.image)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateImageField() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
