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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestValidateModelRoute(t *testing.T) {
	weight0 := uint32(0)
	weight20 := uint32(20)
	weight80 := uint32(80)
	invalidHeaderRegex := "["
	invalidURIRegex := "["

	tests := []struct {
		name           string
		modelRoute     *networkingv1alpha1.ModelRoute
		expectValid    bool
		expectedReason string
	}{
		{
			name: "valid model route with model name",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model route with lora adapters",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					LoraAdapters: []string{"adapter1", "adapter2"},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model route with both model name and lora adapters",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName:    "test-model",
					LoraAdapters: []string{"adapter1"},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model route with default and zero weights",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server-1"},
								{ModelServerName: "test-server-2", Weight: &weight0},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model route with external provider target",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ExternalModelProviderName: "openai-provider"},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model route with weighted internal and external targets",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server", Weight: &weight80},
								{ExternalModelProviderName: "openai-provider", Weight: &weight20},
							},
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "invalid model route - missing both model name and lora adapters",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec: Required value: either modelName or loraAdapters must be specified",
		},
		{
			name: "invalid model route - empty string in lora adapters",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					LoraAdapters: []string{"adapter1", "", "adapter3"},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.loraAdapters[1]: Invalid value: \"\": lora adapter name cannot be an empty string",
		},
		{
			name: "invalid model route - multiple empty strings in lora adapters",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					LoraAdapters: []string{"", "adapter2", ""},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.loraAdapters[0]: Invalid value: \"\": lora adapter name cannot be an empty string\n  - spec.loraAdapters[2]: Invalid value: \"\": lora adapter name cannot be an empty string",
		},
		{
			name: "invalid model route - all lora adapters are empty",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					LoraAdapters: []string{"", ""},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.loraAdapters[0]: Invalid value: \"\": lora adapter name cannot be an empty string\n  - spec.loraAdapters[1]: Invalid value: \"\": lora adapter name cannot be an empty string",
		},
		{
			name: "invalid model route - empty model name and empty lora adapters list",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName:    "",
					LoraAdapters: []string{},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName: "test-server",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec: Required value: either modelName or loraAdapters must be specified",
		},
		{
			name: "invalid model route - rule with empty targetModels",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name:         "empty-targets-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels: Required value: each rule must have at least one target model",
		},
		{
			name: "invalid model route - multiple rules with empty targetModels",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "valid-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server"},
							},
						},
						{
							Name:         "empty-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[1].targetModels: Required value: each rule must have at least one target model",
		},
		{
			name: "invalid model route - empty target reference",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: ""},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels[0]: Invalid value: \"/\": exactly one of modelServerName or externalModelProviderName must be set",
		},
		{
			name: "invalid model route - target has both model server and external provider",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{
									ModelServerName:           "test-server",
									ExternalModelProviderName: "openai-provider",
								},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels[0]: Invalid value: \"test-server/openai-provider\": exactly one of modelServerName or externalModelProviderName must be set",
		},
		{
			name: "invalid model route - target has neither model server nor external provider",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels[0]: Invalid value: \"/\": exactly one of modelServerName or externalModelProviderName must be set",
		},
		{
			name: "invalid model route - lora route uses external provider",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					LoraAdapters: []string{"adapter1"},
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ExternalModelProviderName: "openai-provider"},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels[0].externalModelProviderName: Invalid value: \"openai-provider\": lora routes must target modelServerName",
		},
		{
			name: "invalid model route - all target weights are zero",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server-1", Weight: &weight0},
								{ModelServerName: "test-server-2", Weight: &weight0},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].targetModels: Invalid value: 0: total weight must be greater than zero",
		},
		{
			name: "invalid model route - invalid header regex",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							ModelMatch: &networkingv1alpha1.ModelMatch{
								Headers: map[string]*networkingv1alpha1.StringMatch{
									"x-user": {Regex: &invalidHeaderRegex},
								},
							},
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server"},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].modelMatch.headers[x-user].regex: Invalid value: \"[\": error parsing regexp: missing closing ]: `[`",
		},
		{
			name: "invalid model route - invalid uri regex",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						{
							Name: "test-rule",
							ModelMatch: &networkingv1alpha1.ModelMatch{
								Uri: &networkingv1alpha1.StringMatch{
									Regex: &invalidURIRegex,
								},
							},
							TargetModels: []*networkingv1alpha1.TargetModel{
								{ModelServerName: "test-server"},
							},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0].modelMatch.uri.regex: Invalid value: \"[\": error parsing regexp: missing closing ]: `[`",
		},
		{
			name: "invalid model route - nil rule",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					ModelName: "test-model",
					Rules: []*networkingv1alpha1.Rule{
						nil,
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.rules[0]: Invalid value: null: rule must not be nil",
		},
		{
			name: "invalid model route - combined errors: missing model name and empty targetModels",
			modelRoute: &networkingv1alpha1.ModelRoute{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelRoute",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-route",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelRouteSpec{
					Rules: []*networkingv1alpha1.Rule{
						{
							Name:         "empty-rule",
							TargetModels: []*networkingv1alpha1.TargetModel{},
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec: Required value: either modelName or loraAdapters must be specified\n  - spec.rules[0].targetModels: Required value: each rule must have at least one target model",
		},
	}

	// Create a validator instance
	kubeClient := fake.NewSimpleClientset()
	validator := NewKthenaRouterValidator(kubeClient, 8080)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := validator.validateModelRoute(tt.modelRoute)

			assert.Equal(t, tt.expectValid, allowed, "Expected validation result should match")

			if !tt.expectValid {
				assert.Equal(t, tt.expectedReason, reason, "Error message should match expected reason")
			} else {
				assert.Empty(t, reason, "Reason should be empty for valid model routes")
			}
		})
	}
}

func TestValidateModelServer(t *testing.T) {
	tests := []struct {
		name           string
		modelServer    *networkingv1alpha1.ModelServer
		expectValid    bool
		expectedReason string
	}{
		{
			name: "valid model server",
			modelServer: &networkingv1alpha1.ModelServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelServerSpec{
					InferenceEngine: networkingv1alpha1.VLLM,
					WorkloadSelector: &networkingv1alpha1.WorkloadSelector{
						MatchLabels: map[string]string{
							"app": "test-server",
						},
					},
					WorkloadPort: networkingv1alpha1.WorkloadPort{
						Port: 8000,
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid model server with pd group",
			modelServer: &networkingv1alpha1.ModelServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelServerSpec{
					InferenceEngine: networkingv1alpha1.VLLM,
					WorkloadSelector: &networkingv1alpha1.WorkloadSelector{
						MatchLabels: map[string]string{
							"app": "test-server",
						},
						PDGroup: &networkingv1alpha1.PDGroup{
							GroupKey: "pd-group",
							PrefillLabels: map[string]string{
								"role": "prefill",
							},
							DecodeLabels: map[string]string{
								"role": "decode",
							},
						},
					},
					WorkloadPort: networkingv1alpha1.WorkloadPort{
						Port: 8000,
					},
				},
			},
			expectValid: true,
		},
		{
			name: "invalid model server - empty workload selector",
			modelServer: &networkingv1alpha1.ModelServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelServerSpec{
					InferenceEngine:  networkingv1alpha1.VLLM,
					WorkloadSelector: &networkingv1alpha1.WorkloadSelector{},
					WorkloadPort: networkingv1alpha1.WorkloadPort{
						Port: 8000,
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:   - spec.workloadSelector.matchLabels: Required value: labels must contain at least one label",
		},
		{
			name: "invalid model server - invalid match labels",
			modelServer: &networkingv1alpha1.ModelServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelServerSpec{
					InferenceEngine: networkingv1alpha1.VLLM,
					WorkloadSelector: &networkingv1alpha1.WorkloadSelector{
						MatchLabels: map[string]string{
							"app@name": "test-server",
						},
					},
					WorkloadPort: networkingv1alpha1.WorkloadPort{
						Port: 8000,
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:   - spec.workloadSelector.matchLabels: Invalid value: {\"app@name\":\"test-server\"}: invalid selector: key: Invalid value: \"app@name\": name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')",
		},
		{
			name: "invalid model server - empty pd group",
			modelServer: &networkingv1alpha1.ModelServer{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "networking.serving.volcano.sh/v1alpha1",
					Kind:       "ModelServer",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-server",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ModelServerSpec{
					InferenceEngine: networkingv1alpha1.VLLM,
					WorkloadSelector: &networkingv1alpha1.WorkloadSelector{
						MatchLabels: map[string]string{
							"app": "test-server",
						},
						PDGroup: &networkingv1alpha1.PDGroup{
							GroupKey:      "",
							PrefillLabels: map[string]string{},
							DecodeLabels:  map[string]string{},
						},
					},
					WorkloadPort: networkingv1alpha1.WorkloadPort{
						Port: 8000,
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:   - spec.workloadSelector.pdGroup.groupKey: Required value: groupKey must be specified  - spec.workloadSelector.pdGroup.prefillLabels: Required value: labels must contain at least one label  - spec.workloadSelector.pdGroup.decodeLabels: Required value: labels must contain at least one label",
		},
	}

	kubeClient := fake.NewSimpleClientset()
	validator := NewKthenaRouterValidator(kubeClient, 8080)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := validator.validateModelServer(tt.modelServer)

			assert.Equal(t, tt.expectValid, allowed, "Expected validation result should match")

			if !tt.expectValid {
				assert.Equal(t, tt.expectedReason, reason, "Error message should match expected reason")
			} else {
				assert.Empty(t, reason, "Reason should be empty for valid model servers")
			}
		})
	}
}

func TestValidateExternalModelProvider(t *testing.T) {
	optionalTrue := true

	tests := []struct {
		name           string
		provider       *networkingv1alpha1.ExternalModelProvider
		expectValid    bool
		expectedReason string
	}{
		{
			name: "valid openai provider",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "openai-provider",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					Model:        ptrString("gpt-4o-mini"),
					BaseURL:      "https://api.openai.com/v1",
					Auth: &networkingv1alpha1.ProviderAuth{
						SecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "openai-api-key"},
							Key:                  "apiKey",
						},
					},
				},
			},
			expectValid: true,
		},
		{
			name: "valid anthropic provider with version header",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "anthropic-provider",
					Namespace: "default",
				},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.Anthropic,
					Model:        ptrString("claude-sonnet"),
					BaseURL:      "https://api.anthropic.com",
					Auth: &networkingv1alpha1.ProviderAuth{
						SecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "anthropic-api-key"},
							Key:                  "apiKey",
						},
					},
					Headers: map[string]string{
						"anthropic-version": "2023-06-01",
					},
				},
			},
			expectValid: true,
		},
		{
			name: "invalid provider - http baseURL",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "http://api.example.com",
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.baseURL: Invalid value: \"http://api.example.com\": baseURL must use https",
		},
		{
			name: "invalid provider - url contains userinfo query and fragment",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "https://user@example.com/v1?debug=true#frag",
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.baseURL: Invalid value: \"https://user@example.com/v1?debug=true#frag\": baseURL must not contain userinfo, query, or fragment",
		},
		{
			name: "invalid provider - reserved static authorization header",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "https://api.example.com/v1",
					Headers: map[string]string{
						"authorization": "Bearer unsafe",
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.headers[authorization]: Invalid value: \"authorization\": header is reserved and cannot be configured as a static header",
		},
		{
			name: "invalid provider - reserved static x-api-key header",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.Anthropic,
					BaseURL:      "https://api.anthropic.com",
					Headers: map[string]string{
						"X-API-Key": "unsafe",
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.headers[X-API-Key]: Invalid value: \"X-API-Key\": header is reserved and cannot be configured as a static header",
		},
		{
			name: "invalid provider - malformed static header name",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "https://api.example.com/v1",
					Headers: map[string]string{
						"Bad Header": "value",
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.headers[Bad Header]: Invalid value: \"Bad Header\": header name is invalid",
		},
		{
			name: "invalid provider - static header value contains newline",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "https://api.example.com/v1",
					Headers: map[string]string{
						"X-Tenant": "tenant-a\r\nX-Injected: true",
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.headers[X-Tenant]: Invalid value: \"tenant-a\\r\\nX-Injected: true\": header value must not contain CR or LF",
		},
		{
			name: "invalid provider - optional secret ref",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					ProviderType: networkingv1alpha1.OpenAI,
					BaseURL:      "https://api.example.com/v1",
					Auth: &networkingv1alpha1.ProviderAuth{
						SecretRef: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "openai-api-key"},
							Key:                  "apiKey",
							Optional:             &optionalTrue,
						},
					},
				},
			},
			expectValid:    false,
			expectedReason: "validation failed:\n  - spec.auth.secretRef.optional: Invalid value: true: optional must be false or unset",
		},
		{
			name: "invalid provider - multiple validation errors use separate lines",
			provider: &networkingv1alpha1.ExternalModelProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-provider", Namespace: "default"},
				Spec: networkingv1alpha1.ExternalModelProviderSpec{
					BaseURL: "http://api.example.com",
					Auth: &networkingv1alpha1.ProviderAuth{
						SecretRef: corev1.SecretKeySelector{},
					},
				},
			},
			expectValid: false,
			expectedReason: "validation failed:\n" +
				"  - spec.baseURL: Invalid value: \"http://api.example.com\": baseURL must use https\n" +
				"  - spec.auth.secretRef.name: Required value: secret name is required\n" +
				"  - spec.auth.secretRef.key: Required value: secret key is required",
		},
	}

	kubeClient := fake.NewSimpleClientset()
	validator := NewKthenaRouterValidator(kubeClient, 8080)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := validator.validateExternalModelProvider(tt.provider)

			assert.Equal(t, tt.expectValid, allowed, "Expected validation result should match")
			if !tt.expectValid {
				assert.Equal(t, tt.expectedReason, reason, "Error message should match expected reason")
			} else {
				assert.Empty(t, reason, "Reason should be empty for valid providers")
			}
		})
	}
}

func ptrString(value string) *string {
	return &value
}
