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

func TestRootCommand(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		checkOutput func(string) bool
	}{
		{
			name: "root command help",
			args: []string{"--help"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "kthena") &&
					strings.Contains(output, "CLI") &&
					strings.Contains(output, "inference workloads")
			},
		},
		{
			name: "root command version info",
			args: []string{},
			checkOutput: func(output string) bool {
				// Root command with no args should show help or usage
				return strings.Contains(output, "kthena") ||
					strings.Contains(output, "Usage")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executeCommand(rootCmd, tt.args...)

			// Help command doesn't return error
			if err != nil && !strings.Contains(tt.args[0], "--help") {
				t.Errorf("Unexpected error: %v", err)
			}

			if tt.checkOutput != nil && !tt.checkOutput(output) {
				t.Errorf("Output validation failed. Output: %s", output)
			}
		})
	}
}

func TestGetRootCmd(t *testing.T) {
	cmd := GetRootCmd()

	if cmd == nil {
		t.Fatal("GetRootCmd() returned nil")
	}

	if cmd.Use != "kthena" {
		t.Errorf("Expected command use to be 'kthena', got '%s'", cmd.Use)
	}

	if !strings.Contains(cmd.Short, "Kthena CLI") {
		t.Errorf("Expected short description to contain 'Kthena CLI', got '%s'", cmd.Short)
	}

	// Check that subcommands are registered
	expectedCommands := []string{"create", "get", "describe"}
	for _, expectedCmd := range expectedCommands {
		found := false
		for _, subCmd := range cmd.Commands() {
			if subCmd.Use == expectedCmd {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected subcommand '%s' to be registered", expectedCmd)
		}
	}
}

func TestSubcommands(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		expectError bool
		checkOutput func(string) bool
	}{
		{
			name: "create help",
			args: []string{"create", "--help"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "create") &&
					strings.Contains(output, "manifest")
			},
		},
		{
			name: "get help",
			args: []string{"get", "--help"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "get") &&
					strings.Contains(output, "resources")
			},
		},
		{
			name: "describe help",
			args: []string{"describe", "--help"},
			checkOutput: func(output string) bool {
				return strings.Contains(output, "describe") &&
					strings.Contains(output, "detailed information")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := executeCommand(rootCmd, tt.args...)

			if tt.expectError && err == nil {
				t.Errorf("Expected error but got none")
			}

			if tt.checkOutput != nil && !tt.checkOutput(output) {
				t.Errorf("Output validation failed. Output: %s", output)
			}
		})
	}
}

func TestCommandAliases(t *testing.T) {
	tests := []struct {
		name     string
		commands [][]string // Different ways to invoke the same command
	}{
		{
			name: "model-booster aliases",
			commands: [][]string{
				{"get", "model-boosters", "--help"},
				{"get", "model-booster", "--help"},
			},
		},
		{
			name: "model-serving aliases",
			commands: [][]string{
				{"get", "model-servings", "--help"},
				{"get", "model-serving", "--help"},
				{"get", "ms", "--help"},
			},
		},
		{
			name: "autoscaling-policy aliases",
			commands: [][]string{
				{"get", "autoscaling-policies", "--help"},
				{"get", "autoscaling-policy", "--help"},
				{"get", "asp", "--help"},
			},
		},
		{
			name: "autoscaling-policy-binding aliases",
			commands: [][]string{
				{"get", "autoscaling-policy-bindings", "--help"},
				{"get", "autoscaling-policy-binding", "--help"},
				{"get", "aspb", "--help"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, args := range tt.commands {
				output, err := executeCommand(rootCmd, args...)
				if err != nil {
					t.Errorf("Command %v failed: %v", args, err)
				}
				if output == "" {
					t.Errorf("Command %v produced no output", args)
				}
			}
		})
	}
}
