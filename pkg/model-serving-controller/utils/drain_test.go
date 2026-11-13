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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// TestDrainHookMetricPattern verifies the hook's metric pattern accepts the
// labeled format engines actually emit (vllm:num_requests_running{...} 0.0).
// Keep the pattern below in sync with DrainHookScript.
func TestDrainHookMetricPattern(t *testing.T) {
	if !strings.Contains(DrainHookScript, `(?:\{[^}]*\})?`) || !strings.Contains(DrainHookScript, "if counts and all(") {
		t.Fatal("DrainHookScript must accept optional labels and never treat an empty match as drained")
	}
	pattern := regexp.MustCompile(`(?m)^(?:vllm|sglang):(?:num_requests_running|num_running_reqs|num_requests_waiting|num_queue_reqs)(?:\{[^}]*\})?\s+(\S+)`)

	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "vllm labeled metrics",
			body: "vllm:num_requests_running{engine=\"0\",model_name=\"m\"} 3.0\n" +
				"vllm:num_requests_waiting{engine=\"0\",model_name=\"m\"} 1.0",
			want: []string{"3.0", "1.0"},
		},
		{
			name: "sglang labeled metrics",
			body: "sglang:num_running_reqs{model_name=\"m\"} 0.0\n" +
				"sglang:num_queue_reqs{model_name=\"m\"} 0.0",
			want: []string{"0.0", "0.0"},
		},
		{
			name: "unlabeled metrics",
			body: "vllm:num_requests_running 5",
			want: []string{"5"},
		},
		{
			name: "suffixed metric is not matched",
			body: "vllm:num_requests_waiting_by_reason{reason=\"capacity\"} 2.0",
			want: nil,
		},
		{
			name: "unrelated body is not matched",
			body: "python_gc_objects_collected_total{generation=\"0\"} 3.0",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matches := pattern.FindAllStringSubmatch(tc.body, -1)
			var got []string
			for _, m := range matches {
				got = append(got, m[1])
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// TestDrainHookScriptBehavior runs the hook against a mocked metrics endpoint.
// It is skipped when python3 is unavailable.
func TestDrainHookScriptBehavior(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	if err := exec.Command(python, "--version").Run(); err != nil {
		t.Skipf("python3 is not runnable: %v", err)
	}

	serve := func(body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		require.NoError(t, err)
		return port
	}
	run := func(t *testing.T, port, drainTimeout string) string {
		cmd := exec.Command(python, "-c", DrainHookScript)
		cmd.Env = append(os.Environ(),
			"DRAIN_TIMEOUT="+drainTimeout,
			"GRACE_SECONDS=60",
			"DRAIN_BUFFER_SECONDS=0",
			"DRAIN_METRICS_PORT="+port,
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err)
		return string(out)
	}

	t.Run("zero metrics exits early", func(t *testing.T) {
		port := serve("vllm:num_requests_running{engine=\"0\"} 0.0\nvllm:num_requests_waiting{engine=\"0\"} 0.0")
		out := run(t, port, "60")
		require.Contains(t, out, "in-flight drained")
	})

	t.Run("in-flight metrics keep polling until timeout", func(t *testing.T) {
		port := serve("vllm:num_requests_running{engine=\"0\"} 5.0\nvllm:num_requests_waiting{engine=\"0\"} 0.0")
		out := run(t, port, "2")
		require.Contains(t, out, "timeout before in-flight reached zero")
		require.NotContains(t, out, "in-flight drained")
	})

	t.Run("unmatched metrics do not end the drain", func(t *testing.T) {
		port := serve("python_gc_objects_collected_total{generation=\"0\"} 3.0")
		out := run(t, port, "2")
		require.Contains(t, out, "no drain metrics matched")
		require.NotContains(t, out, "in-flight drained")
	})
}
