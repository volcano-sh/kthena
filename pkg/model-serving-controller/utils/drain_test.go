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

package utils

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func msWithAnnotation(value string) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "demo",
			Namespace:   "default",
			Annotations: map[string]string{workloadv1alpha1.DrainTimeoutAnnotationKey: value},
		},
	}
}

func TestParseDrainConfig(t *testing.T) {
	cases := []struct {
		name        string
		value       string
		set         bool
		wantEnabled bool
		wantInvalid bool
		wantSeconds int
	}{
		{name: "missing", set: false},
		{name: "zero", value: "0s", set: true},
		{name: "invalid", value: "abc", set: true, wantInvalid: true},
		{name: "too small", value: "30s", set: true, wantInvalid: true},
		{name: "enabled", value: "300s", set: true, wantEnabled: true, wantSeconds: 300},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := &workloadv1alpha1.ModelServing{}
			if tc.set {
				ms.Annotations = map[string]string{workloadv1alpha1.DrainTimeoutAnnotationKey: tc.value}
			}
			got := ParseDrainConfig(ms)
			if got.Enabled != tc.wantEnabled || got.Invalid != tc.wantInvalid || got.TimeoutSeconds != tc.wantSeconds {
				t.Fatalf("ParseDrainConfig(%q) = %+v", tc.value, got)
			}
		})
	}
}

func TestParseRoleBuffer(t *testing.T) {
	roleWith := func(value string, set bool) workloadv1alpha1.Role {
		role := workloadv1alpha1.Role{}
		if set {
			role.EntryTemplate.Metadata = &workloadv1alpha1.Metadata{
				Annotations: map[string]string{workloadv1alpha1.DrainKVBufferAnnotationKey: value},
			}
		}
		return role
	}
	cases := []struct {
		name  string
		value string
		set   bool
		want  int
	}{
		{name: "missing", set: false, want: 0},
		{name: "zero", value: "0s", set: true, want: 0},
		{name: "invalid", value: "abc", set: true, want: 0},
		{name: "negative", value: "-5s", set: true, want: 0},
		{name: "normal", value: "20s", set: true, want: 20},
		{name: "clamped", value: "120s", set: true, want: 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRoleBuffer(roleWith(tc.value, tc.set)); got != tc.want {
				t.Fatalf("ParseRoleBuffer(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func drainTestPod(withPorts bool) *corev1.Pod {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "engine"}},
		},
	}
	if withPorts {
		pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8000}}
	}
	return pod
}

func TestApplyDrainDisabled(t *testing.T) {
	pod := drainTestPod(true)
	ApplyDrain(pod, workloadv1alpha1.Role{}, &workloadv1alpha1.ModelServing{})
	if pod.Spec.Containers[0].Lifecycle != nil {
		t.Fatal("preStop hook must not be injected when the drain is disabled")
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "DRAIN_TIMEOUT" {
			t.Fatal("drain env must not be injected when the drain is disabled")
		}
	}
}

func TestApplyDrainNoPorts(t *testing.T) {
	ms := msWithAnnotation("300s")
	pod := drainTestPod(false)
	ApplyDrain(pod, workloadv1alpha1.Role{}, ms)
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "DRAIN_METRICS_PORT" || env.Name == "DRAIN_TIMEOUT" {
			t.Fatalf("env %s must not be injected without a metrics port", env.Name)
		}
	}
	if pod.Spec.Containers[0].Lifecycle != nil {
		t.Fatal("preStop hook must not be injected without a metrics port")
	}
}

func TestApplyDrainInjectsHookAndEnv(t *testing.T) {
	ms := msWithAnnotation("300s")
	pod := drainTestPod(true)
	ApplyDrain(pod, workloadv1alpha1.Role{}, ms)

	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		"DRAIN_TIMEOUT":        "300",
		"GRACE_SECONDS":        "30",
		"DRAIN_BUFFER_SECONDS": "0",
		"DRAIN_METRICS_PORT":   "8000",
	} {
		if env[name] != want {
			t.Fatalf("env %s = %q, want %q", name, env[name], want)
		}
	}
	preStop := pod.Spec.Containers[0].Lifecycle.PreStop
	if preStop == nil || preStop.Exec == nil || len(preStop.Exec.Command) != 3 {
		t.Fatal("preStop hook was not injected")
	}
	if preStop.Exec.Command[0] != "python3" || preStop.Exec.Command[1] != "-c" {
		t.Fatalf("unexpected preStop command: %v", preStop.Exec.Command[:2])
	}
}

func TestApplyDrainKeepsExistingPreStop(t *testing.T) {
	ms := msWithAnnotation("300s")
	pod := drainTestPod(true)
	existing := &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"custom", "hook"}}}
	pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{PreStop: existing}
	ApplyDrain(pod, workloadv1alpha1.Role{}, ms)
	if pod.Spec.Containers[0].Lifecycle.PreStop != existing {
		t.Fatal("an existing preStop hook must not be overwritten")
	}
}

func TestApplyDrainWithKVBuffer(t *testing.T) {
	ms := msWithAnnotation("300s")
	role := workloadv1alpha1.Role{}
	role.EntryTemplate.Metadata = &workloadv1alpha1.Metadata{
		Annotations: map[string]string{workloadv1alpha1.DrainKVBufferAnnotationKey: "30s"},
	}
	pod := drainTestPod(true)
	ApplyDrain(pod, role, ms)
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "DRAIN_BUFFER_SECONDS" && e.Value != "30" {
			t.Fatalf("DRAIN_BUFFER_SECONDS = %q, want 30", e.Value)
		}
	}
}
