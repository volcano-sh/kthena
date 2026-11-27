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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

func bootstrapValidationModelServing(strategy *workloadv1alpha1.BootstrapAccelerateStrategy) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:                    ptr.To[int32](4),
			BootstrapAccelerateStrategy: strategy,
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:           "prefill",
						Replicas:       ptr.To[int32](2),
						WorkerReplicas: 1,
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Image: "vllm:v1"}}},
						},
						WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vllm", Image: "vllm:v1"}}},
						},
					},
					{
						Name:     "router",
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "router", Image: "router:v1"}}},
						},
					},
				},
			},
		},
	}
}

func TestValidateBootstrapAccelerateStrategy(t *testing.T) {
	validator := NewModelServingValidator()
	tests := []struct {
		name        string
		mutate      func(*workloadv1alpha1.ModelServing)
		wantAllowed bool
		wantReason  string
	}{
		{
			name:        "unset strategy is valid",
			mutate:      func(ms *workloadv1alpha1.ModelServing) { ms.Spec.BootstrapAccelerateStrategy = nil },
			wantAllowed: true,
		},
		{
			name: "default all-roles strategy without modelExpress is valid",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{}
			},
			wantAllowed: true,
		},
		{
			name: "scoped roles with modelExpress ignores out-of-scope router container check",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Provider:            workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress,
					InitialSeedReplicas: ptr.To(intstr.FromString("25%")),
					Roles:               []string{"prefill"},
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm"},
						ReadyURL:         "https://127.0.0.1:8000/health",
					},
				}
			},
			wantAllowed: true,
		},
		{
			name: "all-roles scope with modelExpress requires engine container in every role",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm"},
					},
				}
			},
			wantAllowed: false,
			wantReason:  "must contain one of the ModelExpress engine containers",
		},
		{
			name: "all-roles scope passes when engineContainers covers all roles",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromInt32(2)),
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm", "router"},
						ReadyURL:         "http://127.0.0.1:8000/health",
					},
				}
			},
			wantAllowed: true,
		},
		{
			name: "unsupported provider is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Provider: "Unknown",
				}
			},
			wantAllowed: false,
			wantReason:  "provider",
		},
		{
			name: "zero integer initialSeedReplicas is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromInt32(0)),
				}
			},
			wantAllowed: false,
			wantReason:  "must be a positive integer",
		},
		{
			name: "negative integer initialSeedReplicas is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromInt32(-1)),
				}
			},
			wantAllowed: false,
			wantReason:  "must be a positive integer",
		},
		{
			name: "zero percent initialSeedReplicas is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromString("0%")),
				}
			},
			wantAllowed: false,
			wantReason:  "percentage must be between 1% and 100%",
		},
		{
			name: "over 100 percent initialSeedReplicas is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromString("101%")),
				}
			},
			wantAllowed: false,
			wantReason:  "must be a valid percent value",
		},
		{
			name: "non-percent string initialSeedReplicas is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					InitialSeedReplicas: ptr.To(intstr.FromString("two")),
				}
			},
			wantAllowed: false,
			wantReason:  "initialSeedReplicas",
		},
		{
			name: "empty role entry is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{""},
				}
			},
			wantAllowed: false,
			wantReason:  "role name must not be empty",
		},
		{
			name: "duplicate role entry is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{"prefill", "prefill"},
				}
			},
			wantAllowed: false,
			wantReason:  "Duplicate value",
		},
		{
			name: "unknown role entry is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{"nonexistent"},
				}
			},
			wantAllowed: false,
			wantReason:  "does not exist in spec.template.roles",
		},
		{
			name: "empty engineContainers is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles:        []string{"prefill"},
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{},
				}
			},
			wantAllowed: false,
			wantReason:  "at least one engine container must be specified",
		},
		{
			name: "duplicate or empty engineContainers entry is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{"prefill"},
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm", "vllm", ""},
					},
				}
			},
			wantAllowed: false,
			wantReason:  "Duplicate value",
		},
		{
			name: "missing engine container in workerTemplate is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Name = "other"
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{"prefill"},
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm"},
					},
				}
			},
			wantAllowed: false,
			wantReason:  "workerTemplate",
		},
		{
			name: "malformed readyURL is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{
					Roles: []string{"prefill"},
					ModelExpress: &workloadv1alpha1.ModelExpressConfig{
						EngineContainers: []string{"vllm"},
						ReadyURL:         "grpc://127.0.0.1:8001",
					},
				}
			},
			wantAllowed: false,
			wantReason:  "must be an absolute http or https URL",
		},
		{
			name: "explicit managed modelexpress plugin is rejected",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{
					{Name: plugins.ModelExpressPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn},
				}
			},
			wantAllowed: false,
			wantReason:  "plugin modelexpress is managed by spec.bootstrapAccelerateStrategy",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ms := bootstrapValidationModelServing(nil)
			tc.mutate(ms)
			allowed, reason := validator.validateModelServing(ms)
			assert.Equal(t, tc.wantAllowed, allowed, "reason=%s", reason)
			if tc.wantReason != "" {
				assert.Contains(t, reason, tc.wantReason)
			}
		})
	}
}
