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

package inplace

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func newEligibilityModelServing() *workloadv1alpha1.ModelServing {
	podTemplate := func() workloadv1alpha1.PodTemplateSpec {
		return workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "download", Image: "example.com/downloader:v1"}},
				Containers: []corev1.Container{{
					Name:  "engine",
					Image: "example.com/engine:v1",
					Env:   []corev1.EnvVar{{Name: "MODEL", Value: "m1"}},
				}},
			},
		}
	}
	workerTemplate := podTemplate()
	return &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](2),
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{
					Name:           "decode",
					Replicas:       ptr.To[int32](1),
					EntryTemplate:  podTemplate(),
					WorkerReplicas: 1,
					WorkerTemplate: &workerTemplate,
				}},
			},
		},
	}
}

func TestCheckEligibility(t *testing.T) {
	rolePath := field.NewPath("spec", "template", "roles").Index(0)
	entryPath := rolePath.Child("entryTemplate")
	entryContainersPath := entryPath.Child("spec", "containers")

	latestEntry := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:latest"
	}

	tests := []struct {
		name string
		// base changes the old and new object alike before mutate changes the new one.
		base   func(ms *workloadv1alpha1.ModelServing)
		mutate func(ms *workloadv1alpha1.ModelServing)
		want   field.ErrorList
	}{
		{
			name: "image change on entry and worker",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				role := &ms.Spec.Template.Roles[0]
				role.EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
				role.WorkerTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
			},
		},
		{
			name: "scaling and rollout fields change with the image",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](4)
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				ms.Spec.RevisionHistoryLimit = ptr.To[int32](3)
				ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
					Type:         workloadv1alpha1.ServingGroupRollingUpdate,
					UpdateMethod: workloadv1alpha1.InPlaceUpdateMethod,
				}
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](30)
				role := &ms.Spec.Template.Roles[0]
				role.Replicas = ptr.To[int32](3)
				role.MaxUnavailable = ptr.To(intstr.FromInt32(2))
				role.EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
			},
		},
		{
			name: "explicit values that resolve to the defaults",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				template := &ms.Spec.Template.Roles[0].EntryTemplate
				template.Spec.RestartPolicy = corev1.RestartPolicyAlways
				template.Spec.Containers[0].Image = "example.com/engine:v2"
				template.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
			},
		},
		{
			name: "derived pull policy changes from a pinned tag to latest",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:latest"
			},
			want: field.ErrorList{field.Invalid(entryContainersPath.Index(0).Child("imagePullPolicy"), corev1.PullAlways, "")},
		},
		{
			name: "derived pull policy changes from latest to a pinned tag",
			base: latestEntry,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
			},
			want: field.ErrorList{field.Invalid(entryContainersPath.Index(0).Child("imagePullPolicy"), corev1.PullIfNotPresent, "")},
		},
		{
			name: "pinned tag keeps Always explicitly",
			base: latestEntry,
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "example.com/engine:v2"
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
			},
		},
		{
			name: "environment change",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env[0].Value = "m2"
			},
			want: field.ErrorList{field.Forbidden(entryPath, "")},
		},
		{
			name: "init container image change",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.InitContainers[0].Image = "example.com/downloader:v2"
			},
			want: field.ErrorList{field.Forbidden(entryPath, "")},
		},
		{
			name: "Pod template label change",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Metadata = &workloadv1alpha1.Metadata{Labels: map[string]string{"tier": "gold"}}
			},
			want: field.ErrorList{field.Forbidden(entryPath, "")},
		},
		{
			name: "container renamed",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Name = "server"
			},
			want: field.ErrorList{field.Forbidden(entryContainersPath, "")},
		},
		{
			name: "Role added",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, *ms.Spec.Template.Roles[0].DeepCopy())
				ms.Spec.Template.Roles[1].Name = "prefill"
			},
			want: field.ErrorList{field.Forbidden(field.NewPath("spec", "template", "roles"), "")},
		},
		{
			name: "workerReplicas and workerTemplate change",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Template.Roles[0].WorkerReplicas = 0
				ms.Spec.Template.Roles[0].WorkerTemplate = nil
			},
			want: field.ErrorList{
				field.Forbidden(rolePath.Child("workerReplicas"), ""),
				field.Forbidden(rolePath.Child("workerTemplate"), ""),
			},
		},
		{
			name: "scheduler, plugins, gang policy and network topology change",
			mutate: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.SchedulerName = "default-scheduler"
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
					Name:   "demo",
					Type:   workloadv1alpha1.PluginTypeBuiltIn,
					Config: &apiextensionsv1.JSON{Raw: []byte(`{}`)},
				}}
				ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"decode": 1}}
				ms.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{}
			},
			want: field.ErrorList{
				field.Forbidden(field.NewPath("spec", "schedulerName"), ""),
				field.Forbidden(field.NewPath("spec", "plugins"), ""),
				field.Forbidden(field.NewPath("spec", "template", "gangPolicy"), ""),
				field.Forbidden(field.NewPath("spec", "template", "networkTopology"), ""),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldMS := newEligibilityModelServing()
			if tt.base != nil {
				tt.base(oldMS)
			}
			newMS := oldMS.DeepCopy()
			tt.mutate(newMS)
			assert.Equal(t, errorKeys(tt.want), errorKeys(CheckEligibility(oldMS, newMS)))
		})
	}
}

func TestResolveTemplate(t *testing.T) {
	tests := []struct {
		image      string
		pullPolicy corev1.PullPolicy
		want       corev1.PullPolicy
	}{
		{image: "example.com/engine", want: corev1.PullAlways},
		{image: "example.com/engine:latest", want: corev1.PullAlways},
		{image: "example.com/engine:v1", want: corev1.PullIfNotPresent},
		{image: "example.com/engine@sha256:" + testImageDigest, want: corev1.PullIfNotPresent},
		{image: "example.com/engine:latest@sha256:" + testImageDigest, want: corev1.PullAlways},
		{image: "Invalid:Image", want: corev1.PullIfNotPresent},
		{image: "example.com/engine:latest", pullPolicy: corev1.PullNever, want: corev1.PullNever},
	}
	for _, tt := range tests {
		template := &workloadv1alpha1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Name: "init", Image: tt.image, ImagePullPolicy: tt.pullPolicy}},
				Containers:     []corev1.Container{{Name: "engine", Image: tt.image, ImagePullPolicy: tt.pullPolicy}},
			},
		}
		resolved := ResolveTemplate(template)
		assert.Equal(t, corev1.RestartPolicyAlways, resolved.Spec.RestartPolicy)
		assert.Equalf(t, tt.want, resolved.Spec.InitContainers[0].ImagePullPolicy, "init container image %q", tt.image)
		assert.Equalf(t, tt.want, resolved.Spec.Containers[0].ImagePullPolicy, "container image %q", tt.image)
		assert.Equal(t, tt.pullPolicy, template.Spec.Containers[0].ImagePullPolicy, "the input template is not mutated")
	}
}

// errorKeys drops error details, so tests assert only the error type and field.
func errorKeys(errs field.ErrorList) []string {
	var keys []string
	for _, err := range errs {
		keys = append(keys, string(err.Type)+" "+err.Field)
	}
	return keys
}

const testImageDigest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
