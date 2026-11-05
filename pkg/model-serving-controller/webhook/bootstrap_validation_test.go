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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func bootstrapTestModelServing() *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](4),
			BootstrapAccelerateStrategy: &workloadv1alpha1.BootstrapAccelerateStrategy{
				Roles: []workloadv1alpha1.BootstrapRole{
					{Name: "prefill", InitialSeedReplicas: ptr.To(intstr.FromInt32(2))},
					{Name: "decode", InitialSeedReplicas: ptr.To(intstr.FromString("25%"))},
				},
				ModelExpress: &workloadv1alpha1.ModelExpressConfig{
					EngineContainers: []string{"engine"},
					ReadyURL:         "http://127.0.0.1:8000/health",
				},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "prefill",
						Replicas: ptr.To[int32](2),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
							{Name: "engine", Image: "vllm:v1"}, {Name: "sidecar", Image: "proxy:v1"},
						}}},
						WorkerReplicas: 1,
						WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
							{Name: "engine", Image: "vllm:v1"},
						}}},
					},
					{
						Name:     "decode",
						Replicas: ptr.To[int32](4),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
							{Name: "engine", Image: "vllm:v1"},
						}}},
					},
				},
			},
		},
	}
}

func TestValidateBootstrapAccelerateStrategy(t *testing.T) {
	tests := []struct {
		name              string
		mutate            func(*workloadv1alpha1.ModelServing)
		expectedSubstring string
	}{
		{name: "valid"},
		{name: "unset strategy", mutate: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.BootstrapAccelerateStrategy = nil }},
		{name: "default seed and no modelExpress", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = nil
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress = nil
		}},
		{name: "unsupported provider", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Provider = "Other"
		}, expectedSubstring: `spec.bootstrapAccelerateStrategy.provider: Unsupported value: "Other"`},
		{name: "roles are required", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles = nil
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.roles: Required value"},
		{name: "unknown role", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].Name = "unknown"
		}, expectedSubstring: "role unknown does not exist in spec.template.roles"},
		{name: "empty role name", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].Name = ""
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.roles[0].name: Required value"},
		{name: "duplicate role", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[1].Name = "prefill"
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.roles[1].name: Duplicate value"},
		{name: "zero integer seed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(0))
		}, expectedSubstring: "must be a positive integer"},
		{name: "negative integer seed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(-1))
		}, expectedSubstring: "must be a positive integer"},
		{name: "zero percent seed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[1].InitialSeedReplicas = ptr.To(intstr.FromString("0%"))
		}, expectedSubstring: "percentage must be between 1% and 100%"},
		{name: "over one hundred percent seed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[1].InitialSeedReplicas = ptr.To(intstr.FromString("150%"))
		}, expectedSubstring: "must be a valid percent value (0-100)"},
		{name: "malformed percent seed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[1].InitialSeedReplicas = ptr.To(intstr.FromString("abc"))
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.roles[1].initialSeedReplicas: Invalid value"},
		{name: "engine containers are required", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = nil
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.modelExpress.engineContainers: Required value"},
		{name: "empty engine container name", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"engine", ""}
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.modelExpress.engineContainers[1]: Required value"},
		{name: "duplicate engine container", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"engine", "engine"}
		}, expectedSubstring: "spec.bootstrapAccelerateStrategy.modelExpress.engineContainers[1]: Duplicate value"},
		{name: "entry template lacks engine container", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Name = "vllm"
		}, expectedSubstring: "spec.template.roles[decode].entryTemplate.spec.containers: Invalid value"},
		{name: "worker template lacks engine container", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Name = "vllm"
		}, expectedSubstring: "spec.template.roles[prefill].workerTemplate.spec.containers: Invalid value"},
		{name: "out-of-scope role may lack engine container", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles = ms.Spec.BootstrapAccelerateStrategy.Roles[:1]
			ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Name = "vllm"
		}},
		{name: "relative readyURL", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = "/health"
		}, expectedSubstring: "must be an absolute http or https URL"},
		{name: "non-http readyURL", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = "grpc://127.0.0.1:8001"
		}, expectedSubstring: "must be an absolute http or https URL"},
		{name: "https readyURL", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = "https://localhost:8443/health"
		}},
		{name: "managed plugin cannot be listed", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "modelexpress", Type: workloadv1alpha1.PluginTypeBuiltIn}}
		}, expectedSubstring: "spec.plugins[0].name: Forbidden: plugin modelexpress is managed by spec.bootstrapAccelerateStrategy"},
		{name: "managed plugin cannot be listed without the strategy", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy = nil
			ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "modelexpress", Type: workloadv1alpha1.PluginTypeBuiltIn}}
		}, expectedSubstring: "spec.plugins[0].name: Forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := bootstrapTestModelServing()
			if tt.mutate != nil {
				tt.mutate(ms)
			}
			errs := validateBootstrapAccelerateStrategy(ms)
			if tt.expectedSubstring == "" {
				assert.Empty(t, errs, "unexpected errors: %v", errs)
				return
			}
			require.NotEmpty(t, errs)
			assert.Contains(t, errs.ToAggregate().Error(), tt.expectedSubstring)
		})
	}
}

func TestValidateModelServingIncludesBootstrapAccelerateStrategy(t *testing.T) {
	ms := bootstrapTestModelServing()
	ms.Spec.BootstrapAccelerateStrategy.Roles[0].Name = "unknown"
	allowed, reason := NewModelServingValidator().validateModelServing(ms)
	assert.False(t, allowed)
	assert.Contains(t, reason, "role unknown does not exist in spec.template.roles")

	// The strategy is mutable: changing it alone passes update validation.
	old := bootstrapTestModelServing()
	updated := bootstrapTestModelServing()
	updated.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromInt32(3))
	allowed, reason = NewModelServingValidator().validateModelServingUpdate(old, updated)
	assert.True(t, allowed, reason)
}
