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

func TestDescribeTemplateCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		errorMsg    string
		checkOutput func(string) bool
	}{
		{
			name:        "describe existing template",
			args:        []string{"describe", "template", "Qwen/Qwen3-8B"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "Template Content") &&
					strings.Contains(output, "ModelBooster")
			},
		},
		{
			name:        "describe nonexistent template",
			args:        []string{"describe", "template", "nonexistent-template"},
			expectError: true,
			errorMsg:    "not found",
		},
		{
			name:        "describe template without name",
			args:        []string{"describe", "template"},
			expectError: true,
		},
		{
			name:        "describe template with backward compatibility",
			args:        []string{"describe", "template", "Qwen3-8B"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "Template Content")
			},
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
			getNamespace = ""
		})
	}
}

func TestDescribeModelBoosterCommand(t *testing.T) {
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.ModelBooster{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-model",
				Namespace:         "default",
				CreationTimestamp: metav1.Time{Time: time.Now().Add(-1 * time.Hour)},
			},
			Spec: workloadv1alpha1.ModelBoosterSpec{
				Name: "test-model",
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
		expectError bool
		errorMsg    string
		checkOutput func(string) bool
	}{
		{
			name:        "describe existing model booster",
			args:        []string{"describe", "model-booster", "test-model"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "Model:") &&
					strings.Contains(output, "test-model") &&
					strings.Contains(output, "Namespace:") &&
					strings.Contains(output, "default")
			},
		},
		{
			name:        "describe nonexistent model booster",
			args:        []string{"describe", "model-booster", "nonexistent"},
			expectError: true,
			errorMsg:    "failed to get Model",
		},
		{
			name:        "describe model booster in specific namespace",
			args:        []string{"describe", "model-booster", "test-model", "-n", "default"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "test-model")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""

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
		})
	}
}

func TestDescribeModelServingCommand(t *testing.T) {
	fakeClient := fake.NewClientset(
		&workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "test-serving",
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
		expectError bool
		checkOutput func(string) bool
	}{
		{
			name:        "describe existing model serving",
			args:        []string{"describe", "model-serving", "test-serving"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "ModelServing:") &&
					strings.Contains(output, "test-serving")
			},
		},
		{
			name:        "describe with alias ms",
			args:        []string{"describe", "ms", "test-serving"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "test-serving")
			},
		},
		{
			name:        "describe nonexistent model serving",
			args:        []string{"describe", "model-serving", "nonexistent"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""

			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				if tt.checkOutput != nil && !tt.checkOutput(output) {
					t.Errorf("Output validation failed. Output: %s", output)
				}
			}
		})
	}
}

func TestDescribeAutoscalingPolicyCommand(t *testing.T) {
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
		expectError bool
		checkOutput func(string) bool
	}{
		{
			name:        "describe existing autoscaling policy",
			args:        []string{"describe", "autoscaling-policy", "test-policy"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "AutoscalingPolicy:") &&
					strings.Contains(output, "test-policy")
			},
		},
		{
			name:        "describe with alias asp",
			args:        []string{"describe", "asp", "test-policy"},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "test-policy")
			},
		},
		{
			name:        "describe nonexistent policy",
			args:        []string{"describe", "autoscaling-policy", "nonexistent"},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getNamespace = ""

			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				if tt.checkOutput != nil && !tt.checkOutput(output) {
					t.Errorf("Output validation failed. Output: %s", output)
				}
			}
		})
	}
}
