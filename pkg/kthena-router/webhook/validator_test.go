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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestValidateModelRoute(t *testing.T) {
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
			expectedReason: "validation failed:   - spec: Required value: either modelName or loraAdapters must be specified",
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
			expectedReason: "validation failed:   - spec.loraAdapters[1]: Invalid value: \"\": lora adapter name cannot be an empty string",
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
			expectedReason: "validation failed:   - spec.loraAdapters[0]: Invalid value: \"\": lora adapter name cannot be an empty string  - spec.loraAdapters[2]: Invalid value: \"\": lora adapter name cannot be an empty string",
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
			expectedReason: "validation failed:   - spec.loraAdapters[0]: Invalid value: \"\": lora adapter name cannot be an empty string  - spec.loraAdapters[1]: Invalid value: \"\": lora adapter name cannot be an empty string",
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
			expectedReason: "validation failed:   - spec: Required value: either modelName or loraAdapters must be specified",
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
			expectedReason: "validation failed:   - spec.rules[0].targetModels: Required value: each rule must have at least one target model",
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
			expectedReason: "validation failed:   - spec.rules[1].targetModels: Required value: each rule must have at least one target model",
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
			expectedReason: "validation failed:   - spec: Required value: either modelName or loraAdapters must be specified  - spec.rules[0].targetModels: Required value: each rule must have at least one target model",
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
