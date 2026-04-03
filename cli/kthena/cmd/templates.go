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
	"embed"
	"fmt"
	"os"
	"strings"
)

var templatesFS embed.FS
var templatesBasePath = "helm/templates" // Can be overridden in tests

type ManifestInfo struct {
	Name        string
	Description string
	FilePath    string
}

// InitTemplates initializes the templates filesystem
func InitTemplates(fs embed.FS) {
	templatesFS = fs
}

// findTemplatePath finds the full path to a template file
func findTemplatePath(templateName string) (string, error) {
	// If templateName contains a slash, it's in vendor/model format, use it directly
	if strings.Contains(templateName, "/") {
		templatePath := fmt.Sprintf("%s/%s.yaml", templatesBasePath, templateName)
		_, err := templatesFS.Open(templatePath)
		if err == nil {
			return templatePath, nil
		}
	}

	// Fallback: search through all vendor directories (for backward compatibility)
	vendors, err := templatesFS.ReadDir(templatesBasePath)
	if err != nil {
		return "", fmt.Errorf("failed to read templates directory: %v", err)
	}

	for _, vendor := range vendors {
		if vendor.IsDir() {
			vendorPath := fmt.Sprintf("%s/%s/%s.yaml", templatesBasePath, vendor.Name(), templateName)
			_, err := templatesFS.Open(vendorPath)
			if err == nil {
				return vendorPath, nil
			}
		}
	}

	return "", fmt.Errorf("template '%s' not found", templateName)
}

// GetTemplateContent returns the content of a template by name (vendor/model format)
func GetTemplateContent(templateName string) (string, error) {
	templatePath, err := findTemplatePath(templateName)
	if err != nil {
		return "", err
	}

	content, err := templatesFS.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to read template '%s': %v", templateName, err)
	}

	return string(content), nil
}

// ListTemplates returns a list of all available template names in vendor/model format
func ListTemplates() ([]string, error) {
	vendors, err := templatesFS.ReadDir(templatesBasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read templates directory: %v", err)
	}

	var templates []string
	for _, vendor := range vendors {
		if vendor.IsDir() {
			vendorPath := fmt.Sprintf("%s/%s", templatesBasePath, vendor.Name())
			models, err := templatesFS.ReadDir(vendorPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not read vendor directory %s: %v\n", vendorPath, err)
				continue // Skip if can't read vendor directory
			}

			for _, model := range models {
				if !model.IsDir() && strings.HasSuffix(model.Name(), ".yaml") {
					templateName := strings.TrimSuffix(model.Name(), ".yaml")
					fullTemplateName := fmt.Sprintf("%s/%s", vendor.Name(), templateName)
					templates = append(templates, fullTemplateName)
				}
			}
		}
	}

	return templates, nil
}

// TemplateExists checks if a template with the given name exists
func TemplateExists(templateName string) bool {
	_, err := findTemplatePath(templateName)
	return err == nil
}

// GetTemplateInfo returns template information including name, description, and file path
func GetTemplateInfo(templateName string) (ManifestInfo, error) {
	content, err := GetTemplateContent(templateName)
	if err != nil {
		return ManifestInfo{}, err
	}

	description := extractManifestDescriptionFromContent(content)
	return ManifestInfo{
		Name:        templateName,
		Description: description,
		FilePath:    fmt.Sprintf("%s.yaml", templateName),
	}, nil
}

// extractManifestDescriptionFromContent extracts description from template content
func extractManifestDescriptionFromContent(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Look for description in comments at the top of the file
		if strings.HasPrefix(trimmed, "# Description:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# Description:"))
		}
		if strings.HasPrefix(trimmed, "# ") && strings.Contains(strings.ToLower(trimmed), "description") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "# "))
		}
		// Stop looking after the first non-comment, non-empty line
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
	}

	return "No description available"
}
