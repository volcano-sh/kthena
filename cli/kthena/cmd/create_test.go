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
	"os"
	"path/filepath"
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

func TestLoadTemplateValuesNamespace(t *testing.T) {
	tests := []struct {
		name         string
		valuesFile   string
		set          map[string]string
		namespace    string
		namespaceSet bool
		want         string
	}{
		{name: "flag default", namespace: "default", want: "default"},
		{name: "explicit flag", namespace: "prod", namespaceSet: true, want: "prod"},
		{name: "values file without flag", valuesFile: "namespace: from-file\n", namespace: "default", want: "from-file"},
		{name: "explicit flag overrides values file", valuesFile: "namespace: from-file\n", namespace: "prod", namespaceSet: true, want: "prod"},
		{name: "--set without flag", set: map[string]string{"namespace": "from-set"}, namespace: "default", want: "from-set"},
		{name: "explicit flag overrides --set", set: map[string]string{"namespace": "from-set"}, namespace: "prod", namespaceSet: true, want: "prod"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldValuesFile, oldNamespace, oldName, oldFlags := valuesFile, namespace, name, manifestFlags
			t.Cleanup(func() {
				valuesFile, namespace, name, manifestFlags = oldValuesFile, oldNamespace, oldName, oldFlags
			})

			valuesFile, name, namespace, manifestFlags = "", "", tt.namespace, tt.set
			if tt.valuesFile != "" {
				valuesFile = filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(valuesFile, []byte(tt.valuesFile), 0o600); err != nil {
					t.Fatalf("failed to write values file: %v", err)
				}
			}

			values, err := loadTemplateValues(tt.namespaceSet)
			if err != nil {
				t.Fatalf("loadTemplateValues() error = %v", err)
			}
			if got := values["namespace"]; got != tt.want {
				t.Errorf("namespace = %v, want %q", got, tt.want)
			}
		})
	}
}
