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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

type testPlugin struct {
	name  string
	calls *[]string
	errOn string
}

func (t *testPlugin) Name() string { return t.name }

func (t *testPlugin) OnPodCreate(_ context.Context, _ *HookRequest) error {
	*t.calls = append(*t.calls, "create-"+t.name)
	if t.errOn == "create" {
		return assertError
	}
	return nil
}

func (t *testPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	*t.calls = append(*t.calls, "ready-"+t.name)
	if t.errOn == "ready" {
		return assertError
	}
	return nil
}

type pluginError string

func (p pluginError) Error() string { return string(p) }

const assertError = pluginError("plugin error")

func TestChainOrderingAndScope(t *testing.T) {
	calls := []string{}
	p1 := &testPlugin{name: "p1", calls: &calls}
	p2 := &testPlugin{name: "p2", calls: &calls}

	chain := &Chain{entries: []entry{
		{plugin: p1, spec: workloadv1alpha1.PluginSpec{Name: "p1"}},
		{plugin: p2, spec: workloadv1alpha1.PluginSpec{Name: "p2", Scope: &workloadv1alpha1.PluginScope{Targets: []workloadv1alpha1.PluginTarget{workloadv1alpha1.PluginTargetWorker}}}},
	}}

	entryReq := &HookRequest{RoleName: "role", RoleID: "role-0", IsEntry: true, Pod: &corev1.Pod{}}
	if err := chain.OnPodCreate(context.Background(), entryReq); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(calls, ","); got != "create-p1" {
		t.Fatalf("entry run mismatch, got %s", got)
	}

	calls = calls[:0]
	workerReq := &HookRequest{RoleName: "role", RoleID: "role-0", IsEntry: false, Pod: &corev1.Pod{}}
	if err := chain.OnPodCreate(context.Background(), workerReq); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(calls, ","); got != "create-p1,create-p2" {
		t.Fatalf("worker run mismatch, got %s", got)
	}

	calls = calls[:0]
	if err := chain.OnPodReady(context.Background(), workerReq); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(calls, ","); got != "ready-p1,ready-p2" {
		t.Fatalf("ready run mismatch, got %s", got)
	}
}

func TestChainErrorPropagation(t *testing.T) {
	calls := []string{}
	errPlugin := &testPlugin{name: "bad", calls: &calls, errOn: "create"}
	chain := &Chain{entries: []entry{{plugin: errPlugin, spec: workloadv1alpha1.PluginSpec{Name: "bad"}}}}

	err := chain.OnPodCreate(context.Background(), &HookRequest{Pod: &corev1.Pod{}})
	if err == nil {
		t.Fatalf("expected error from plugin")
	}
}

func TestNewChainTypeValidation(t *testing.T) {
	registry := NewRegistry()
	registry.Register("demo", func(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
		return &testPlugin{name: spec.Name, calls: &[]string{}}, nil
	})

	if _, err := NewChain(registry, []workloadv1alpha1.PluginSpec{{Name: "demo", Type: workloadv1alpha1.PluginType("Unsupported")}}); err == nil {
		t.Fatalf("expected error for unsupported type")
	}
}
