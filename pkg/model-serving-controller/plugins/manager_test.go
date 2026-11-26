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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

type testPlugin struct {
	name       string
	calls      *[]string
	errOn      string
	syncResult HookResult
}

func (t *testPlugin) Name() string { return t.name }

func (t *testPlugin) OnModelServingSync(_ context.Context, _ *HookRequest) HookResult {
	*t.calls = append(*t.calls, "ms-sync-"+t.name)
	if t.errOn == "ms-sync" {
		res := t.syncResult
		if res.Err == nil {
			res.Err = assertError
		}
		return res
	}
	return t.syncResult
}

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

func (t *testPlugin) OnRoleSync(_ context.Context, _ *HookRequest) error {
	*t.calls = append(*t.calls, "role-sync-"+t.name)
	if t.errOn == "role-sync" {
		return assertError
	}
	return nil
}

func (t *testPlugin) OnRoleDelete(_ context.Context, _ *HookRequest) error {
	*t.calls = append(*t.calls, "role-delete-"+t.name)
	if t.errOn == "role-delete" {
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
		{plugin: p2, spec: workloadv1alpha1.PluginSpec{Name: "p2", Scope: &workloadv1alpha1.PluginScope{Target: workloadv1alpha1.PluginTargetWorker}}},
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

func TestNewChainRejectsDuplicatePlugins(t *testing.T) {
	registry := NewRegistry()
	registry.Register("demo", func(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
		return &testPlugin{name: spec.Name, calls: &[]string{}}, nil
	})

	_, err := NewChain(registry, []workloadv1alpha1.PluginSpec{
		{Name: "demo", Type: workloadv1alpha1.PluginTypeBuiltIn},
		{Name: "demo", Type: workloadv1alpha1.PluginTypeBuiltIn},
	})
	if err == nil || !strings.Contains(err.Error(), "configured more than once") {
		t.Fatalf("expected duplicate plugin error, got %v", err)
	}
}

func TestChainRoleLifecycleHooks(t *testing.T) {
	calls := []string{}
	chain := &Chain{entries: []entry{
		{plugin: &testPlugin{name: "all", calls: &calls}, spec: workloadv1alpha1.PluginSpec{Name: "all"}},
		{plugin: &testPlugin{name: "scoped", calls: &calls}, spec: workloadv1alpha1.PluginSpec{
			Name:  "scoped",
			Scope: &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}},
		}},
	}}
	req := &HookRequest{RoleName: "decode", RoleID: "decode-0"}

	if err := chain.OnRoleSync(context.Background(), req); err != nil {
		t.Fatalf("unexpected Role sync error: %v", err)
	}
	if err := chain.OnRoleDelete(context.Background(), req); err != nil {
		t.Fatalf("unexpected Role delete error: %v", err)
	}
	if got := strings.Join(calls, ","); got != "role-sync-all,role-delete-all" {
		t.Fatalf("Role lifecycle scope mismatch, got %s", got)
	}
}

func TestChainRoleDeleteRespectsConfigAndScope(t *testing.T) {
	calls := []string{}
	registry := NewRegistry()
	registry.Register("cleanup", func(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
		return &testPlugin{name: spec.Name, calls: &calls}, nil
	})
	registry.Register("unconfigured", func(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
		return &testPlugin{name: spec.Name, calls: &calls}, nil
	})

	chain, err := NewChain(registry, []workloadv1alpha1.PluginSpec{{
		Name:  "cleanup",
		Type:  workloadv1alpha1.PluginTypeBuiltIn,
		Scope: &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}},
	}})
	if err != nil {
		t.Fatalf("unexpected chain build error: %v", err)
	}

	if err := chain.OnRoleDelete(context.Background(), &HookRequest{RoleName: "decode"}); err != nil {
		t.Fatalf("unexpected Role delete error: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected out-of-scope and unconfigured plugins not to run, got %v", calls)
	}

	if err := chain.OnRoleDelete(context.Background(), &HookRequest{RoleName: "prefill"}); err != nil {
		t.Fatalf("unexpected Role delete error: %v", err)
	}
	if got := strings.Join(calls, ","); got != "role-delete-cleanup" {
		t.Fatalf("role delete hook mismatch, got %s", got)
	}
}

type inPlacePlugin struct {
	testPlugin
	supported bool
}

func (p *inPlacePlugin) SupportsInPlaceUpdate() bool { return p.supported }

func TestChainPluginWithoutInPlaceUpdate(t *testing.T) {
	calls := []string{}
	supported := &inPlacePlugin{testPlugin: testPlugin{name: "supported", calls: &calls}, supported: true}
	unsupported := &inPlacePlugin{testPlugin: testPlugin{name: "unsupported", calls: &calls}}
	undeclared := &testPlugin{name: "undeclared", calls: &calls}
	tests := []struct {
		name    string
		chain   *Chain
		want    string
		wantAny bool
	}{
		{name: "no chain"},
		{name: "every plugin supports in-place updates", chain: &Chain{entries: []entry{{plugin: supported}}}},
		{name: "a plugin declares no support", chain: &Chain{entries: []entry{{plugin: supported}, {plugin: unsupported}}}, want: "unsupported", wantAny: true},
		{name: "a plugin does not declare", chain: &Chain{entries: []entry{{plugin: undeclared}, {plugin: unsupported}}}, want: "undeclared", wantAny: true},
		{name: "lws-standard-labels", chain: &Chain{entries: []entry{{plugin: &LWSLabelsPlugin{name: LWSLabelsPluginName}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.chain.PluginWithoutInPlaceUpdate()
			if got != tt.want || ok != tt.wantAny {
				t.Fatalf("PluginWithoutInPlaceUpdate() = %q, %v, want %q, %v", got, ok, tt.want, tt.wantAny)
			}
		})
	}
}

func TestChainOnModelServingSync(t *testing.T) {
	errFirst := pluginError("first failed")
	errSecond := pluginError("second failed")

	tests := []struct {
		name             string
		nilChain         bool
		req              *HookRequest
		plugins          []func(calls *[]string) entry
		wantCalls        string
		wantRequeueAfter time.Duration
		wantContinue     bool
		wantErrs         []error
	}{
		{
			name:     "nil chain",
			nilChain: true,
			req:      &HookRequest{},
		},
		{
			name: "nil request",
			plugins: []func(calls *[]string) entry{
				func(calls *[]string) entry {
					return entry{plugin: &testPlugin{name: "p1", calls: calls}, spec: workloadv1alpha1.PluginSpec{Name: "p1"}}
				},
			},
		},
		{
			name: "runs all plugins in order ignoring role and target scope and picks shortest positive requeue",
			req:  &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{}},
			plugins: []func(calls *[]string) entry{
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p1", calls: calls, syncResult: HookResult{RequeueAfter: 10 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p1", Scope: &workloadv1alpha1.PluginScope{Roles: []string{"prefill"}, Target: workloadv1alpha1.PluginTargetWorker}},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p2", calls: calls, syncResult: HookResult{RequeueAfter: 3 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p2"},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p3", calls: calls},
						spec:   workloadv1alpha1.PluginSpec{Name: "p3"},
					}
				},
			},
			wantCalls:        "ms-sync-p1,ms-sync-p2,ms-sync-p3",
			wantRequeueAfter: 3 * time.Second,
		},
		{
			name: "blocking error stops remaining plugins",
			req:  &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{}},
			plugins: []func(calls *[]string) entry{
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p1", calls: calls, errOn: "ms-sync", syncResult: HookResult{Err: errFirst, Continue: false, RequeueAfter: 5 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p1"},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p2", calls: calls},
						spec:   workloadv1alpha1.PluginSpec{Name: "p2"},
					}
				},
			},
			wantCalls:        "ms-sync-p1",
			wantRequeueAfter: 5 * time.Second,
			wantContinue:     false,
			wantErrs:         []error{errFirst},
		},
		{
			name: "continuable error runs remaining plugins and preserves requeue",
			req:  &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{}},
			plugins: []func(calls *[]string) entry{
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p1", calls: calls, errOn: "ms-sync", syncResult: HookResult{Err: errFirst, Continue: true, RequeueAfter: 8 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p1"},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p2", calls: calls, syncResult: HookResult{RequeueAfter: 2 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p2"},
					}
				},
			},
			wantCalls:        "ms-sync-p1,ms-sync-p2",
			wantRequeueAfter: 2 * time.Second,
			wantContinue:     true,
			wantErrs:         []error{errFirst},
		},
		{
			name: "continuable error followed by blocking error stops and joins both errors",
			req:  &HookRequest{ModelServing: &workloadv1alpha1.ModelServing{}},
			plugins: []func(calls *[]string) entry{
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p1", calls: calls, errOn: "ms-sync", syncResult: HookResult{Err: errFirst, Continue: true, RequeueAfter: 4 * time.Second}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p1"},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p2", calls: calls, errOn: "ms-sync", syncResult: HookResult{Err: errSecond, Continue: false}},
						spec:   workloadv1alpha1.PluginSpec{Name: "p2"},
					}
				},
				func(calls *[]string) entry {
					return entry{
						plugin: &testPlugin{name: "p3", calls: calls},
						spec:   workloadv1alpha1.PluginSpec{Name: "p3"},
					}
				},
			},
			wantCalls:        "ms-sync-p1,ms-sync-p2",
			wantRequeueAfter: 4 * time.Second,
			wantContinue:     false,
			wantErrs:         []error{errFirst, errSecond},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := []string{}
			var chain *Chain
			if !tt.nilChain {
				entries := make([]entry, 0, len(tt.plugins))
				for _, build := range tt.plugins {
					entries = append(entries, build(&calls))
				}
				chain = &Chain{entries: entries}
			}

			res := chain.OnModelServingSync(context.Background(), tt.req)
			if got := strings.Join(calls, ","); got != tt.wantCalls {
				t.Fatalf("calls = %q, want %q", got, tt.wantCalls)
			}
			if res.RequeueAfter != tt.wantRequeueAfter {
				t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeueAfter)
			}
			if res.Continue != tt.wantContinue {
				t.Fatalf("Continue = %v, want %v", res.Continue, tt.wantContinue)
			}
			if len(tt.wantErrs) == 0 {
				if res.Err != nil {
					t.Fatalf("unexpected error: %v", res.Err)
				}
			} else {
				if res.Err == nil {
					t.Fatalf("expected error containing %v, got nil", tt.wantErrs)
				}
				for _, wantErr := range tt.wantErrs {
					if !errors.Is(res.Err, wantErr) {
						t.Fatalf("expected error %v in %v", wantErr, res.Err)
					}
				}
			}
		})
	}
}
