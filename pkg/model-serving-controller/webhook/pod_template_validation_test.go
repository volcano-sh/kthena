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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/convert"
)

func TestModelBoosterGeneratedPodTemplates(t *testing.T) {
	for _, fixture := range []string{"ModelBooster-vllm.yaml", "ModelBooster-vllm-disaggregated.yaml"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "e2e", "controller-manager", "testdata", fixture))
			require.NoError(t, err)
			var model workloadv1alpha1.ModelBooster
			require.NoError(t, yaml.Unmarshal(data, &model))
			ms, err := convert.BuildModelServing(&model)
			require.NoError(t, err)
			for _, role := range ms.Spec.Template.Roles {
				if role.WorkerReplicas == 0 {
					assert.Nil(t, role.WorkerTemplate, "role %s must omit its unused worker template", role.Name)
				}
			}
			allowed, reason := NewModelServingValidator().validateModelServing(ms)
			assert.True(t, allowed, reason)
		})
	}
}

func TestValidatePodTemplates(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*workloadv1alpha1.PodTemplateSpec)
		wantField string
	}{
		{
			name:   "omitted defaults",
			mutate: func(*workloadv1alpha1.PodTemplateSpec) {},
		},
		{
			name: "recursive probe and volume defaults",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Metadata = &workloadv1alpha1.Metadata{
					Labels: map[string]string{"app": "inference"}, Annotations: map[string]string{"example.com/config": "value"},
				}
				template.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox:1.36"}}
				template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}},
				}
				template.Spec.Volumes = []corev1.Volume{{
					Name: "config",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "config"},
					}},
				}}
				template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "config", MountPath: "/config"}}
			},
		},
		{
			name: "valid host aliases",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.HostAliases = []corev1.HostAlias{
					{IP: "10.0.0.1", Hostnames: []string{"inference.example.com"}},
					{IP: "::1", Hostnames: []string{"localhost"}},
				}
			},
		},
		{
			name: "invalid host alias IP",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.HostAliases = []corev1.HostAlias{{IP: "not-an-ip", Hostnames: []string{"example.com"}}}
			},
			wantField: "spec.hostAliases[0].ip",
		},
		{
			name: "invalid host alias hostname",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.HostAliases = []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"invalid_host"}}}
			},
			wantField: "spec.hostAliases[0].hostnames[0]",
		},
		{
			name: "negative resource request",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")}
			},
			wantField: "spec.containers[0].resources.requests[cpu]",
		},
		{
			name: "request exceeds limit",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
				}
			},
			wantField: "spec.containers[0].resources.requests",
		},
		{
			name: "fractional GPU limit",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("500m")}
			},
			wantField: "spec.containers[0].resources.limits[nvidia.com/gpu]",
		},
		{
			name: "GPU request differs from limit",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
					Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
				}
			},
			wantField: "spec.containers[0].resources.requests",
		},
		{
			name: "resources requests exceed limits",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"cpu": resource.MustParse("100m")},
					Limits:   corev1.ResourceList{"cpu": resource.MustParse("10m")},
				}
			},
			wantField: "spec.containers[0].resources.requests",
		},
		{
			name: "valid GPU limit only",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
			},
		},
		{
			name: "invalid init container resources",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.InitContainers = []corev1.Container{{
					Name: "init", Image: "busybox:1.36",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")}},
				}}
			},
			wantField: "spec.initContainers[0].resources.limits[cpu]",
		},
		{
			name: "unknown volume mount",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "missing", MountPath: "/data"}}
			},
			wantField: "spec.containers[0].volumeMounts[0].name",
		},
		{
			name: "invalid explicit DNS policy is not defaulted away",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.DNSPolicy = "invalid"
			},
			wantField: "spec.dnsPolicy",
		},
		{
			name: "invalid labels",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Metadata = &workloadv1alpha1.Metadata{Labels: map[string]string{"app": "invalid value"}}
			},
			wantField: "metadata.labels",
		},
		{
			name: "invalid annotation key",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Metadata = &workloadv1alpha1.Metadata{Annotations: map[string]string{"invalid key": "value"}}
			},
			wantField: "metadata.annotations",
		},
		{
			name: "missing containers",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Containers = nil
			},
			wantField: "spec.containers",
		},
		{
			name: "ephemeral containers forbidden in templates",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "busybox:1.36"},
				}}
			},
			wantField: "spec.ephemeralContainers",
		},
		{
			name: "pod level resources use upstream default feature gates",
			mutate: func(template *workloadv1alpha1.PodTemplateSpec) {
				template.Spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")},
				}
			},
			wantField: "spec.resources.requests[cpu]",
		},
	}

	for _, target := range []string{"entryTemplate", "workerTemplate"} {
		for _, tt := range tests {
			t.Run(target+"/"+tt.name, func(t *testing.T) {
				ms := modelServingWithValidPodTemplate()
				role := &ms.Spec.Template.Roles[0]
				template := &role.EntryTemplate
				if target == "workerTemplate" {
					// A supplied worker template is validated even with zero workers.
					role.WorkerTemplate = role.EntryTemplate.DeepCopy()
					template = role.WorkerTemplate
				}
				tt.mutate(template)
				before := ms.DeepCopy()
				errs := validatePodTemplates(ms)
				assert.Equal(t, before, ms, "validation must not mutate the submitted object")
				allowed, reason := NewModelServingValidator().validateModelServing(ms)
				if tt.wantField == "" {
					assert.Empty(t, errs)
					assert.True(t, allowed, reason)
					return
				}
				require.NotEmpty(t, errs)
				wantPath := "spec.template.roles[0]." + target + "." + tt.wantField
				fields := make([]string, 0, len(errs))
				for _, err := range errs {
					fields = append(fields, err.Field)
				}
				assert.Contains(t, fields, wantPath)
				assert.False(t, allowed)
				assert.Contains(t, reason, wantPath)
			})
		}
	}
}

func TestValidatePodTemplatesAggregatesRoles(t *testing.T) {
	ms := modelServingWithValidPodTemplate()
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, *ms.Spec.Template.Roles[0].DeepCopy())
	ms.Spec.Template.Roles[1].Name = "worker"
	ms.Spec.Template.Roles[1].WorkerTemplate = ms.Spec.Template.Roles[1].EntryTemplate.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.HostAliases = []corev1.HostAlias{{IP: "invalid"}}
	ms.Spec.Template.Roles[1].WorkerTemplate.Spec.HostAliases = []corev1.HostAlias{{IP: "invalid"}}

	errs := validatePodTemplates(ms)
	require.Len(t, errs, 2)
	assert.Equal(t, "spec.template.roles[0].entryTemplate.spec.hostAliases[0].ip", errs[0].Field)
	assert.Equal(t, "spec.template.roles[1].workerTemplate.spec.hostAliases[0].ip", errs[1].Field)
}

func modelServingWithValidPodTemplate() *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To(int32(1)),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{
					Name: "inference", Replicas: ptr.To(int32(1)),
					EntryTemplate: workloadv1alpha1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "nginx:1.27"}}},
					},
				}},
			},
		},
	}
}
