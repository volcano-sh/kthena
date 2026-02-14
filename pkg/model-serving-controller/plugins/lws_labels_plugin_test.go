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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestLWSLabelsPluginOnPodCreate(t *testing.T) {
	tests := []struct {
		name           string
		req            *HookRequest
		expectLabels   map[string]string
		expectError    bool
		expectNoChange bool // true if req/pod is nil and no mutation expected
	}{
		{
			name: "entry pod gets all four LWS labels",
			req: &HookRequest{
				ModelServing: &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "my-lws"},
				},
				ServingGroup: "my-lws-0",
				RoleName:     "default",
				RoleID:       "default-0",
				IsEntry:      true,
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "my-lws-0-default-0-0",
						Labels: map[string]string{},
					},
				},
			},
			expectLabels: map[string]string{
				LWSLabelName:        "my-lws",
				LWSLabelGroupIndex:  "0",
				LWSLabelWorkerIndex: "0",
				LWSLabelGroupKey:    "my-lws-0",
			},
		},
		{
			name: "worker pod gets correct worker index",
			req: &HookRequest{
				ModelServing: &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "my-lws"},
				},
				ServingGroup: "my-lws-2",
				RoleName:     "default",
				RoleID:       "default-0",
				IsEntry:      false,
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "my-lws-2-default-0-3",
						Labels: map[string]string{},
					},
				},
			},
			expectLabels: map[string]string{
				LWSLabelName:        "my-lws",
				LWSLabelGroupIndex:  "2",
				LWSLabelWorkerIndex: "3",
				LWSLabelGroupKey:    "my-lws-2",
			},
		},
		{
			name: "existing user labels are not overwritten",
			req: &HookRequest{
				ModelServing: &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "my-lws"},
				},
				ServingGroup: "my-lws-0",
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "my-lws-0-default-0-0",
						Labels: map[string]string{
							LWSLabelName:       "user-override",
							LWSLabelGroupIndex: "99",
							"custom-label":     "keep-me",
						},
					},
				},
			},
			expectLabels: map[string]string{
				LWSLabelName:        "user-override", // preserved
				LWSLabelGroupIndex:  "99",            // preserved
				LWSLabelWorkerIndex: "0",             // injected
				LWSLabelGroupKey:    "my-lws-0",      // injected
				"custom-label":      "keep-me",       // untouched
			},
		},
		{
			name: "nil labels map is initialized",
			req: &HookRequest{
				ModelServing: &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "test"},
				},
				ServingGroup: "test-1",
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-1-role-0-0",
					},
				},
			},
			expectLabels: map[string]string{
				LWSLabelName:        "test",
				LWSLabelGroupIndex:  "1",
				LWSLabelWorkerIndex: "0",
				LWSLabelGroupKey:    "test-1",
			},
		},
		{
			name:           "nil request is safe",
			req:            nil,
			expectNoChange: true,
		},
		{
			name: "nil pod is safe",
			req: &HookRequest{
				ModelServing: &workloadv1alpha1.ModelServing{
					ObjectMeta: metav1.ObjectMeta{Name: "x"},
				},
				Pod: nil,
			},
			expectNoChange: true,
		},
		{
			name: "nil ModelServing is safe",
			req: &HookRequest{
				ModelServing: nil,
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "x-0-r-0-0"},
				},
			},
			expectNoChange: true,
		},
	}

	plugin := &LWSLabelsPlugin{name: LWSLabelsPluginName}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := plugin.OnPodCreate(context.Background(), tt.req)

			if tt.expectError && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tt.expectError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.expectNoChange {
				return
			}

			pod := tt.req.Pod
			for key, want := range tt.expectLabels {
				got, ok := pod.Labels[key]
				if !ok {
					t.Errorf("label %s missing", key)
				} else if got != want {
					t.Errorf("label %s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestLWSLabelsPluginReadyNoop(t *testing.T) {
	plugin := &LWSLabelsPlugin{name: LWSLabelsPluginName}
	if err := plugin.OnPodReady(context.Background(), &HookRequest{}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestLWSLabelsPluginRegistration(t *testing.T) {
	factory, ok := DefaultRegistry.factories[LWSLabelsPluginName]
	if !ok {
		t.Fatalf("plugin %s not registered in DefaultRegistry", LWSLabelsPluginName)
	}

	spec := workloadv1alpha1.PluginSpec{
		Name: LWSLabelsPluginName,
		Type: workloadv1alpha1.PluginTypeBuiltIn,
	}
	p, err := factory(spec)
	if err != nil {
		t.Fatalf("factory returned error: %v", err)
	}
	if p.Name() != LWSLabelsPluginName {
		t.Fatalf("plugin name = %q, want %q", p.Name(), LWSLabelsPluginName)
	}
}
