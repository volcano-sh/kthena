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

package cmd

import (
	"context"
	"testing"

	"github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestApplyKthenaResource(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		resource   string
	}{
		{
			name:       "ModelServing",
			apiVersion: workloadv1alpha1.SchemeGroupVersion.String(),
			kind:       "ModelServing",
			resource:   "modelservings",
		},
		{
			name:       "ModelBooster",
			apiVersion: workloadv1alpha1.SchemeGroupVersion.String(),
			kind:       "ModelBooster",
			resource:   "modelboosters",
		},
		{
			name:       "AutoscalingPolicy",
			apiVersion: workloadv1alpha1.SchemeGroupVersion.String(),
			kind:       "AutoscalingPolicy",
			resource:   "autoscalingpolicies",
		},
		{
			name:       "ModelRoute",
			apiVersion: networkingv1alpha1.SchemeGroupVersion.String(),
			kind:       "ModelRoute",
			resource:   "modelroutes",
		},
		{
			name:       "ModelServer",
			apiVersion: networkingv1alpha1.SchemeGroupVersion.String(),
			kind:       "ModelServer",
			resource:   "modelservers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			obj := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": tt.apiVersion,
				"kind":       tt.kind,
				"metadata": map[string]interface{}{
					"name":      "test-resource",
					"namespace": "test-namespace",
				},
				"spec": map[string]interface{}{},
			}}

			if err := applyKthenaResource(context.Background(), client, obj); err != nil {
				t.Fatalf("applyKthenaResource() error = %v", err)
			}

			actions := client.Actions()
			if len(actions) != 1 {
				t.Fatalf("expected 1 client action, got %d", len(actions))
			}
			action := actions[0]
			if action.GetVerb() != "create" {
				t.Errorf("expected create action, got %q", action.GetVerb())
			}
			if action.GetResource().Resource != tt.resource {
				t.Errorf("expected resource %q, got %q", tt.resource, action.GetResource().Resource)
			}
			if action.GetNamespace() != "test-namespace" {
				t.Errorf("expected namespace %q, got %q", "test-namespace", action.GetNamespace())
			}
		})
	}
}

func TestApplyKthenaResourceRejectsUnsupportedKind(t *testing.T) {
	client := fake.NewSimpleClientset()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(networkingv1alpha1.SchemeGroupVersion.WithKind("Unsupported"))
	obj.SetName("test-resource")
	obj.SetNamespace(metav1.NamespaceDefault)

	err := applyKthenaResource(context.Background(), client, obj)
	if err == nil || err.Error() != "unsupported resource type: Unsupported" {
		t.Fatalf("expected unsupported resource error, got %v", err)
	}
}
