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
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/volcano-sh/kthena/cli/kthena/cmd"
)

// TestEmbeddedTemplatesRenderValidYAML renders every embedded template through
// `kthena create manifest --dry-run` and decodes the output with the same decoder
// that the apply path uses.
func TestEmbeddedTemplatesRenderValidYAML(t *testing.T) {
	cmd.InitTemplates(templatesFS)

	templates, err := cmd.ListTemplates()
	if err != nil {
		t.Fatalf("ListTemplates() error = %v", err)
	}
	if len(templates) == 0 {
		t.Fatal("ListTemplates() returned no templates")
	}

	for _, name := range templates {
		t.Run(name, func(t *testing.T) {
			output := runDryRun(t, name)

			const separator = "================\n"
			start := strings.Index(output, separator)
			end := strings.LastIndex(output, separator)
			if start < 0 || end <= start {
				t.Fatalf("rendered YAML not found in output:\n%s", output)
			}
			rendered := output[start+len(separator) : end]

			decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 1024)
			documents := 0
			for {
				var obj map[string]interface{}
				if err := decoder.Decode(&obj); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					t.Fatalf("failed to decode rendered YAML: %v\n%s", err, rendered)
				}
				if obj == nil {
					continue
				}
				if obj["kind"] == nil {
					t.Errorf("rendered document has no kind: %v", obj)
				}
				documents++
			}
			if documents == 0 {
				t.Fatalf("template rendered no documents:\n%s", rendered)
			}
		})
	}
}

// runDryRun runs `kthena create manifest --dry-run` for a template and returns its stdout.
func runDryRun(t *testing.T, templateName string) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = stdout }()

	// Drain the pipe while the command runs so that large output cannot block the write.
	var output bytes.Buffer
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(&output, reader)
		copied <- err
	}()

	root := cmd.GetRootCmd()
	root.SetArgs([]string{"create", "manifest", "--template", templateName, "--dry-run"})
	runErr := root.Execute()

	os.Stdout = stdout
	if err := writer.Close(); err != nil {
		t.Fatalf("closing stdout pipe: %v", err)
	}
	if err := <-copied; err != nil {
		t.Fatalf("reading stdout pipe: %v", err)
	}
	if runErr != nil {
		t.Fatalf("kthena create manifest --template %s --dry-run error = %v\n%s", templateName, runErr, output.String())
	}
	return output.String()
}
