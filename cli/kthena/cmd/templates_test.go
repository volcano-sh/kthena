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
)

func TestListTemplates(t *testing.T) {
	templates, err := ListTemplates()
	if err != nil {
		t.Fatalf("Failed to list templates: %v", err)
	}

	if len(templates) == 0 {
		t.Error("Expected at least one template, got none")
	}

	// Check that templates are in vendor/model format
	for _, template := range templates {
		if !strings.Contains(template, "/") {
			t.Errorf("Template '%s' is not in vendor/model format", template)
		}
	}

	// Check for known templates (at least Qwen should exist)
	foundQwen := false
	for _, template := range templates {
		if strings.Contains(template, "Qwen") {
			foundQwen = true
			break
		}
	}

	if !foundQwen {
		t.Error("Expected to find at least one Qwen template")
	}
}

func TestTemplateExists(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		expected     bool
	}{
		{
			name:         "existing template with vendor prefix",
			templateName: "Qwen/Qwen3-8B",
			expected:     true,
		},
		{
			name:         "existing template without vendor prefix (backward compatibility)",
			templateName: "Qwen3-8B",
			expected:     true,
		},
		{
			name:         "nonexistent template",
			templateName: "nonexistent-template",
			expected:     false,
		},
		{
			name:         "empty template name",
			templateName: "",
			expected:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := TemplateExists(tt.templateName)
			if result != tt.expected {
				t.Errorf("TemplateExists(%s) = %v, expected %v", tt.templateName, result, tt.expected)
			}
		})
	}
}

func TestGetTemplateContent(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		expectError  bool
		checkContent func(string) bool
	}{
		{
			name:         "get existing template",
			templateName: "Qwen/Qwen3-8B",
			expectError:  false,
			checkContent: func(content string) bool {
				return strings.Contains(content, "ModelBooster") &&
					strings.Contains(content, "apiVersion") &&
					strings.Contains(content, "workload.serving.volcano.sh")
			},
		},
		{
			name:         "get template without vendor prefix",
			templateName: "Qwen3-8B",
			expectError:  false,
			checkContent: func(content string) bool {
				return strings.Contains(content, "ModelBooster")
			},
		},
		{
			name:         "nonexistent template",
			templateName: "nonexistent-template",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := GetTemplateContent(tt.templateName)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				if tt.checkContent != nil && !tt.checkContent(content) {
					t.Errorf("Content validation failed for template %s", tt.templateName)
				}
			}
		})
	}
}

func TestGetTemplateInfo(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		expectError  bool
		checkInfo    func(ManifestInfo) bool
	}{
		{
			name:         "get info for existing template",
			templateName: "Qwen/Qwen3-8B",
			expectError:  false,
			checkInfo: func(info ManifestInfo) bool {
				return info.Name == "Qwen/Qwen3-8B" &&
					info.Description != "" &&
					info.FilePath != ""
			},
		},
		{
			name:         "nonexistent template",
			templateName: "nonexistent-template",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := GetTemplateInfo(tt.templateName)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				if tt.checkInfo != nil && !tt.checkInfo(info) {
					t.Errorf("Info validation failed. Got: %+v", info)
				}
			}
		})
	}
}

func TestExtractManifestDescriptionFromContent(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			name: "description with 'Description:' prefix",
			content: `# Description: This is a test template
apiVersion: v1
kind: Test`,
			expected: "This is a test template",
		},
		{
			name: "description without prefix",
			content: `# This template description contains the word description
apiVersion: v1
kind: Test`,
			expected: "This template description contains the word description",
		},
		{
			name: "no description",
			content: `apiVersion: v1
kind: Test`,
			expected: "No description available",
		},
		{
			name: "description after non-comment lines",
			content: `apiVersion: v1
# Description: This should be ignored
kind: Test`,
			expected: "No description available",
		},
		{
			name:     "empty content",
			content:  "",
			expected: "No description available",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractManifestDescriptionFromContent(tt.content)
			if result != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestFindTemplatePath(t *testing.T) {
	tests := []struct {
		name         string
		templateName string
		expectError  bool
	}{
		{
			name:         "find with vendor prefix",
			templateName: "Qwen/Qwen3-8B",
			expectError:  false,
		},
		{
			name:         "find without vendor prefix (fallback)",
			templateName: "Qwen3-8B",
			expectError:  false,
		},
		{
			name:         "template not found",
			templateName: "nonexistent/template",
			expectError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := findTemplatePath(tt.templateName)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none, path: %s", path)
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if path == "" {
					t.Errorf("Expected non-empty path")
				}
			}
		})
	}
}
