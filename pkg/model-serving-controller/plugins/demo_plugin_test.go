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

package plugins

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestDemoPluginOnPodCreate(t *testing.T) {
	cfg := DemoConfig{
		RuntimeClassName: "nvidia",
		Annotations:      map[string]string{"a": "b"},
		Env:              []corev1.EnvVar{{Name: "EXTRA", Value: "1"}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	spec := workloadv1alpha1.PluginSpec{
		Name:   DemoPluginName,
		Type:   workloadv1alpha1.PluginTypeBuiltIn,
		Config: &apiextensionsv1.JSON{Raw: raw},
	}

	plugin, err := NewDemoPlugin(spec)
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}

	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c1"}}}}
	req := &HookRequest{Pod: pod}
	if err := plugin.OnPodCreate(context.Background(), req); err != nil {
		t.Fatalf("on create: %v", err)
	}

	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "nvidia" {
		t.Fatalf("runtimeClass not set: %v", pod.Spec.RuntimeClassName)
	}
	if pod.Annotations["a"] != "b" {
		t.Fatalf("annotation not applied")
	}
	if len(pod.Spec.Containers[0].Env) != 1 || pod.Spec.Containers[0].Env[0].Name != "EXTRA" {
		t.Fatalf("env not injected: %+v", pod.Spec.Containers[0].Env)
	}
}

func TestDemoPluginReadyNoop(t *testing.T) {
	plugin := &DemoPlugin{name: "demo"}
	if err := plugin.OnPodReady(context.Background(), &HookRequest{}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}
