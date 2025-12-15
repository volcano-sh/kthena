package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestCreateManifestCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		errorMsg    string
	}{
		{
			name:        "missing template flag",
			args:        []string{"create", "manifest"},
			expectError: true,
			errorMsg:    "required flag",
		},
		{
			name:        "template not found",
			args:        []string{"create", "manifest", "--template", "nonexistent-template"},
			expectError: true,
			errorMsg:    "not found",
		},
		{
			name:        "dry run with valid template",
			args:        []string{"create", "manifest", "--template", "Qwen3-8B", "--dry-run"},
			expectError: false,
		},
		{
			name:        "dry run with name flag",
			args:        []string{"create", "manifest", "--template", "Qwen3-8B", "--name", "test-model", "--dry-run"},
			expectError: false,
		},
		{
			name:        "dry run with set values",
			args:        []string{"create", "manifest", "--template", "Qwen/Qwen3-8B", "--set", "name=my-model,owner=test-user", "--dry-run"},
			expectError: false,
		},
		{
			name:        "dry run with namespace",
			args:        []string{"create", "manifest", "--template", "Qwen/Qwen3-8B", "--namespace", "test-namespace", "--dry-run"},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset flags before each test
			templateName = ""
			valuesFile = ""
			dryRun = false
			namespace = "default"
			name = ""
			manifestFlags = make(map[string]string)

			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none. Output: %s", output)
				} else if tt.errorMsg != "" && !strings.Contains(err.Error(), tt.errorMsg) && !strings.Contains(output, tt.errorMsg) {
					t.Errorf("Expected error containing '%s', got: %v, output: %s", tt.errorMsg, err, output)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v, output: %s", err, output)
				}
			}
		})
	}
}

func TestLoadTemplateValues(t *testing.T) {
	tests := []struct {
		name         string
		setupFlags   func()
		expectError  bool
		expectedKeys []string
	}{
		{
			name: "load from manifest flags",
			setupFlags: func() {
				manifestFlags = map[string]string{
					"key1": "value1",
					"key2": "value2",
				}
				name = ""
				namespace = "default"
			},
			expectError:  false,
			expectedKeys: []string{"key1", "key2", "namespace"},
		},
		{
			name: "load with name flag",
			setupFlags: func() {
				manifestFlags = make(map[string]string)
				name = "test-model"
				namespace = "test-ns"
			},
			expectError:  false,
			expectedKeys: []string{"name", "namespace"},
		},
		{
			name: "default namespace when not specified",
			setupFlags: func() {
				manifestFlags = make(map[string]string)
				name = ""
				namespace = "default"
			},
			expectError:  false,
			expectedKeys: []string{"namespace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupFlags()

			values, err := loadTemplateValues()

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				for _, key := range tt.expectedKeys {
					if _, exists := values[key]; !exists {
						t.Errorf("Expected key '%s' not found in values", key)
					}
				}
			}
		})
	}
}

func TestRenderTemplate(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		values       map[string]interface{}
		expectError  bool
		checkOutput  func(string) bool
	}{
		{
			name:         "render valid template",
			templateName: "Qwen/Qwen3-8B",
			values: map[string]interface{}{
				"name":      "test-model",
				"namespace": "default",
			},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "ModelBooster") &&
					strings.Contains(output, "test-model")
			},
		},
		{
			name:         "template not found",
			templateName: "nonexistent-template",
			values:       map[string]interface{}{},
			expectError:  true,
		},
		{
			name:         "render with custom values",
			templateName: "Qwen/Qwen3-8B",
			values: map[string]interface{}{
				"name":           "custom-model",
				"namespace":      "prod",
				"owner":          "test-owner",
				"workerReplicas": 2,
			},
			expectError: false,
			checkOutput: func(output string) bool {
				return strings.Contains(output, "custom-model") &&
					strings.Contains(output, "prod") &&
					strings.Contains(output, "test-owner")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := renderTemplate(tt.templateName, tt.values)

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

func TestAskForConfirmation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"yes", "y\n", true},
		{"yes full", "yes\n", true},
		{"Yes capital", "Yes\n", true},
		{"no", "n\n", false},
		{"no full", "no\n", false},
		{"empty", "\n", false},
		{"random", "maybe\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldStdin := os.Stdin
			r, w, _ := os.Pipe()
			os.Stdin = r

			go func() {
				defer w.Close()
				w.Write([]byte(tt.input))
			}()

			result := askForConfirmation("Test question")
			os.Stdin = oldStdin

			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}
