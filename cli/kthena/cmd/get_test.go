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
	"strings"
	"testing"
	"time"

	"github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGetTemplatesCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		checkOutput func(string) bool
	}{
		{
			name:        "list all templates",
			args:        []string{"get", "templates"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME") &&
					strings.Contains(output, "DESCRIPTION")
			},
		},
		{
			name:        "list templates with yaml output",
			args:        []string{"get", "templates", "-o", "yaml"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "Name:") ||
					strings.Contains(output, "Description:")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v, output: %s", err, output)
				}

				if tt.checkOutput != nil && !tt.checkOutput(output) {
					t.Errorf("Output validation failed. Output: %s", output)
				}
			}

			// Reset flags
			outputFormat = ""
		})
	}
}

func TestGetTemplateCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		errorMsg    string
		checkOutput func(string) bool
	}{
		{
			name:        "get existing template",
			args:        []string{"get", "template", "Qwen/Qwen3-8B"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "ModelBooster") ||
					strings.Contains(output, "apiVersion")
			},
		},
		{
			name:        "get template with yaml output",
			args:        []string{"get", "template", "Qwen/Qwen3-8B", "-o", "yaml"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "apiVersion") &&
					strings.Contains(output, "ModelBooster")
			},
		},
		{
			name:        "get nonexistent template",
			args:        []string{"get", "template", "nonexistent-template"},
			expectError: true,
			errorMsg:    "not found",
		},
		{
			name:        "get template without name",
			args:        []string{"get", "template"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none. Output: %s", output)
				} else if tt.errorMsg != "" && !strings.Contains(err.Error(), tt.errorMsg) && !strings.Contains(output, tt.errorMsg) {
					t.Errorf("Expected error containing '%s', got: %v", tt.errorMsg, err)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v, output: %s", err, output)
				}

				if tt.checkOutput != nil && !tt.checkOutput(output) {
					t.Errorf("Output validation failed. Output: %s", output)
				}
			}

			// Reset flags
			outputFormat = ""
		})
	}
}

func TestGetModelBoostersCommand(t *testing.T) {
	// Create fake client with test data
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.ModelBooster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-model-1",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			},
		},
		&workloadv1alpha1.ModelBooster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-model-2",
				Namespace:         "production",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-2 * time.Hour)},
			},
		},
	)

	// Mock the client function
	originalGetClientFunc := getKthenaClientFunc
	getKthenaClientFunc = func() (versioned.Interface, error) {
		return fakeClient, nil
	}
	defer func() {
		getKthenaClientFunc = originalGetClientFunc
	}()

	tests := []struct {
		name        string
		args        []string
		expectError bool
		checkOutput func(string) bool
	}{
		{
			name:        "list model-boosters in default namespace",
			args:        []string{"get", "model-boosters"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME") &&
					strings.Contains(output, "test-model-1")
			},
		},
		{
			name:        "list model-boosters in specific namespace",
			args:        []string{"get", "model-boosters", "-n", "production"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "test-model-2")
			},
		},
		{
			name:        "list model-boosters across all namespaces",
			args:        []string{"get", "model-boosters", "--all-namespaces"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAMESPACE") &&
					strings.Contains(output, "test-model-1") &&
					strings.Contains(output, "test-model-2")
			},
		},
		{
			name:        "list model-boosters with name filter",
			args:        []string{"get", "model-boosters", "test-model-1"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "test-model-1") &&
					!strings.Contains(output, "test-model-2")
			},
		},
		{
			name:        "alias model-booster works",
			args:        []string{"get", "model-booster"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset flags
			getNamespace = ""
			getAllNamespaces = false

			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none. Output: %s", output)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v, output: %s", err, output)
				}

				if tt.checkOutput != nil && !tt.checkOutput(output) {
					t.Errorf("Output validation failed. Output: %s", output)
				}
			}
		})
	}
}

func TestGetModelServingsCommand(t *testing.T) {
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-serving-1",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-30 * time.Minute)},
			},
		},
	)

	originalGetClientFunc := getKthenaClientFunc
	getKthenaClientFunc = func() (versioned.Interface, error) {
		return fakeClient, nil
	}
	defer func() {
		getKthenaClientFunc = originalGetClientFunc
	}()

	tests := []struct {
		name        string
		args        []string
		checkOutput func(string) bool
	}{
		{
			name: "list model-servings",
			args: []string{"get", "model-servings"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME") &&
					strings.Contains(output, "test-serving-1")
			},
		},
		{
			name: "alias ms works",
			args: []string{"get", "ms"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME")
			},
		},
		{
			name: "alias model-serving works",
			args: []string{"get", "model-serving"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""
			getAllNamespaces = false

			output, err := executeCommand(rootCmd, tt.args...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if tt.checkOutput != nil && !tt.checkOutput(output) {
				t.Errorf("Output validation failed. Output: %s", output)
			}
		})
	}
}

func TestGetAutoscalingPoliciesCommand(t *testing.T) {
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.AutoscalingPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-policy",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			},
		},
	)

	originalGetClientFunc := getKthenaClientFunc
	getKthenaClientFunc = func() (versioned.Interface, error) {
		return fakeClient, nil
	}
	defer func() {
		getKthenaClientFunc = originalGetClientFunc
	}()

	tests := []struct {
		name        string
		args        []string
		checkOutput func(string) bool
	}{
		{
			name: "list autoscaling-policies",
			args: []string{"get", "autoscaling-policies"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME") &&
					strings.Contains(output, "test-policy")
			},
		},
		{
			name: "alias asp works",
			args: []string{"get", "asp"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""
			getAllNamespaces = false

			output, err := executeCommand(rootCmd, tt.args...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if tt.checkOutput != nil && !tt.checkOutput(output) {
				t.Errorf("Output validation failed. Output: %s", output)
			}
		})
	}
}

func TestGetAutoscalingPolicyBindingsCommand(t *testing.T) {
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.AutoscalingPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-binding",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-45 * time.Minute)},
			},
		},
	)

	originalGetClientFunc := getKthenaClientFunc
	getKthenaClientFunc = func() (versioned.Interface, error) {
		return fakeClient, nil
	}
	defer func() {
		getKthenaClientFunc = originalGetClientFunc
	}()

	tests := []struct {
		name        string
		args        []string
		checkOutput func(string) bool
	}{
		{
			name: "list autoscaling-policy-bindings",
			args: []string{"get", "autoscaling-policy-bindings"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME") &&
					strings.Contains(output, "test-binding")
			},
		},
		{
			name: "alias aspb works",
			args: []string{"get", "aspb"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "NAME")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""
			getAllNamespaces = false

			output, err := executeCommand(rootCmd, tt.args...)
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if tt.checkOutput != nil && !tt.checkOutput(output) {
				t.Errorf("Output validation failed. Output: %s", output)
			}
		})
	}
}

func TestResolveGetNamespace(t *testing.T) {
	tests := []struct {
		name             string
		getNamespaceVal  string
		allNamespacesVal bool
		expected         string
	}{
		{
			name:             "all namespaces flag set",
			getNamespaceVal:  "",
			allNamespacesVal: true,
			expected:         "",
		},
		{
			name:             "specific namespace set",
			getNamespaceVal:  "production",
			allNamespacesVal: false,
			expected:         "production",
		},
		{
			name:             "default namespace",
			getNamespaceVal:  "",
			allNamespacesVal: false,
			expected:         "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = tt.getNamespaceVal
			getAllNamespaces = tt.allNamespacesVal

			result := resolveGetNamespace()
			if result != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}
