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
	"bytes"
	"embed"
	"io"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

//go:embed testdata/helm/templates/*/*.yaml
var testTemplatesFS embed.FS

func TestMain(m *testing.M) {
	// Initialize templates from embedded test data
	InitTemplates(testTemplatesFS)
	templatesBasePath = "testdata/helm/templates"

	code := m.Run()
	os.Exit(code)
}

// Helper function to execute command and capture output
func executeCommand(root *cobra.Command, args ...string) (output string, err error) {
	// Create buffers for stdout/stderr
	oldStdout := os.Stdout
	oldStderr := os.Stderr

	r, w, _ := os.Pipe()
	os.Stdout = w
	os.Stderr = w

	// Also set cobra command output
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	// Execute in goroutine and capture output
	errChan := make(chan error, 1)
	go func() {
		errChan <- root.Execute()
		w.Close()
	}()

	// Read captured output
	outBytes, _ := io.ReadAll(r)
	err = <-errChan

	// Restore stdout/stderr
	os.Stdout = oldStdout
	os.Stderr = oldStderr

	// Combine all output
	output = string(outBytes) + buf.String()

	return output, err
}
