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

package kube

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeKubeconfig(t *testing.T, name, server string) string {
	t.Helper()
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user: {}
`, server)
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}
	return path
}

// forceOutOfCluster clears the in-cluster env so BuildConfig takes the
// kubeconfig path regardless of where the test runs.
func forceOutOfCluster(t *testing.T) {
	t.Helper()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func TestBuildConfig_HonorsKubeconfigEnv(t *testing.T) {
	forceOutOfCluster(t)
	envPath := writeKubeconfig(t, "env-config", "https://from-env.example.com")
	t.Setenv("KUBECONFIG", envPath)

	config, err := BuildConfig("", "")
	if err != nil {
		t.Fatalf("BuildConfig failed: %v", err)
	}
	if config.Host != "https://from-env.example.com" {
		t.Errorf("expected host from KUBECONFIG, got %q", config.Host)
	}
}

func TestBuildConfig_ExplicitPathWinsOverEnv(t *testing.T) {
	forceOutOfCluster(t)
	envPath := writeKubeconfig(t, "env-config", "https://from-env.example.com")
	t.Setenv("KUBECONFIG", envPath)
	explicitPath := writeKubeconfig(t, "explicit-config", "https://from-flag.example.com")

	config, err := BuildConfig("", explicitPath)
	if err != nil {
		t.Fatalf("BuildConfig failed: %v", err)
	}
	if config.Host != "https://from-flag.example.com" {
		t.Errorf("expected host from the explicit path, got %q", config.Host)
	}
}
