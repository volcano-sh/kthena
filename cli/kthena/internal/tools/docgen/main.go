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

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/volcano-sh/kthena/cli/kthena/cmd"

	"github.com/spf13/cobra/doc"
)

func main() {
	// Get the root command from the CLI application
	rootCmd := cmd.GetRootCmd()

	// Disable the auto-generated tag, including the date
	rootCmd.DisableAutoGenTag = true

	// Define output directory relative to the project root (assumes running from repo root)
	// Target: docs/kthena/docs/reference/cli
	outputDir := "docs/kthena/docs/reference/kthena-cli"

	// Clear existing documentation files to avoid outdated docs
	if err := clearDirectory(outputDir); err != nil {
		log.Fatalf("Error clearing output directory: %v", err)
	}

	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		log.Fatalf("Error creating doc output directory: %v", err)
	}

	// Generate Markdown documentation
	if err := doc.GenMarkdownTree(rootCmd, outputDir); err != nil {
		log.Fatalf("Error generating Markdown documentation: %v", err)
	}

	// Add front matter to all generated markdown files
	if err := addFrontMatter(outputDir); err != nil {
		log.Fatalf("Error adding front matter: %v", err)
	}

	fmt.Printf("Markdown documentation generated in %s\n", outputDir)
}

// clearDirectory removes all .md files in the specified directory
func clearDirectory(dir string) error {
	// Check if directory exists
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// Directory doesn't exist, nothing to clear
		return nil
	}

	// Read all files in the directory
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("error reading directory %s: %w", dir, err)
	}

	// Remove all .md files
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".md" {
			filePath := filepath.Join(dir, file.Name())
			if err := os.Remove(filePath); err != nil {
				return fmt.Errorf("error removing file %s: %w", filePath, err)
			}
			fmt.Printf("Removed outdated doc: %s\n", filePath)
		}
	}

	return nil
}

// addFrontMatter prepends front matter to all .md files in the directory
func addFrontMatter(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("error reading directory %s: %w", dir, err)
	}
	for _, file := range files {
		if !file.IsDir() && filepath.Ext(file.Name()) == ".md" {
			filePath := filepath.Join(dir, file.Name())
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("error reading file %s: %w", filePath, err)
			}
			content := string(data)
			// Check if front matter already exists (simple check)
			if strings.HasPrefix(content, "---\n") {
				// Already has front matter, skip
				continue
			}
			// Prepend front matter
			newContent := "---\ntitle: Kthena CLI\n---\n" + content
			if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
				return fmt.Errorf("error writing file %s: %w", filePath, err)
			}
			fmt.Printf("Added front matter to %s\n", filePath)
		}
	}
	return nil
}
