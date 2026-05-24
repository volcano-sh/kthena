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

package framework

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/volcano-sh/kthena/test/e2e/utils"
)

var (
	pfForwarder utils.PortForwarder
)

// KthenaConfig holds the configuration for installing kthena
type KthenaConfig struct {
	Namespace                 string
	WorkloadEnabled           bool
	NetworkingEnabled         bool
	GatewayAPIEnabled         bool
	InferenceExtensionEnabled bool
	ImageTag                  string
	ChartPath                 string
}

// NewDefaultConfig returns a default configuration for kthena installation
// consistent with charts/kthena/values.yaml
func NewDefaultConfig() *KthenaConfig {
	_, filename, _, _ := runtime.Caller(0)
	// filename is /.../test/e2e/framework/framework.go
	dir := filepath.Dir(filename)
	// dir is /.../test/e2e/framework
	chartPath := filepath.Join(dir, "../../../charts/kthena")

	return &KthenaConfig{
		Namespace:                 "dev",
		WorkloadEnabled:           true,
		NetworkingEnabled:         true,
		GatewayAPIEnabled:         false,
		InferenceExtensionEnabled: false,
		ImageTag:                  os.Getenv("TAG"),
		ChartPath:                 chartPath,
	}
}

// InstallKthena installs kthena via helm
func InstallKthena(cfg *KthenaConfig) error {
	if cfg.ImageTag == "" {
		cfg.ImageTag = "latest"
	}

	args := []string{
		"install", "kthena", cfg.ChartPath,
		"--namespace", cfg.Namespace,
		"--create-namespace",
		"--set", fmt.Sprintf("workload.enabled=%v", cfg.WorkloadEnabled),
		"--set", fmt.Sprintf("networking.enabled=%v", cfg.NetworkingEnabled),
		"--set", fmt.Sprintf("networking.kthenaRouter.gatewayAPI.enabled=%v", cfg.GatewayAPIEnabled),
		"--set", fmt.Sprintf("networking.kthenaRouter.gatewayAPI.inferenceExtension=%v", cfg.InferenceExtensionEnabled),
		"--set", fmt.Sprintf("networking.kthenaRouter.image.tag=%s", cfg.ImageTag),
		"--set", fmt.Sprintf("networking.webhook.image.tag=%s", cfg.ImageTag),
		"--set", fmt.Sprintf("workload.controllerManager.image.tag=%s", cfg.ImageTag),
		"--set", fmt.Sprintf("workload.controllerManager.downloaderImage.tag=%s", cfg.ImageTag),
		"--set", fmt.Sprintf("workload.controllerManager.runtimeImage.tag=%s", cfg.ImageTag),
	}

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("Installing kthena: %s\n", strings.Join(cmd.Args, " "))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to install kthena: %v", err)
	}

	// Wait for pods to be ready
	fmt.Println("Waiting for kthena pods to be ready...")
	waitCmd := exec.Command("kubectl", "wait", "--for=condition=Ready", "pod", "--all", "-n", cfg.Namespace, "--timeout=300s")
	waitCmd.Stdout = os.Stdout
	waitCmd.Stderr = os.Stderr
	if err := waitCmd.Run(); err != nil {
		return fmt.Errorf("failed to wait for kthena pods: %v", err)
	}

	// Wait for auto-generated Gateway if Gateway API is enabled
	if cfg.GatewayAPIEnabled {
		fmt.Printf("Waiting for auto-generated Gateway 'default' in '%s' namespace...\n", cfg.Namespace)
		// We use a simple loop with timeout as Gateway API resources might take a moment to be created by the controller
		start := time.Now()
		timeout := 2 * time.Minute
		for {
			cmd := exec.Command("kubectl", "get", "gateway", "default", "-n", cfg.Namespace)
			if err := cmd.Run(); err == nil {
				fmt.Println("Gateway 'default' is ready")
				break
			}
			if time.Since(start) > timeout {
				return fmt.Errorf("timeout waiting for auto-generated Gateway 'default' in namespace %s", cfg.Namespace)
			}
			time.Sleep(5 * time.Second)
		}
	}

	// Setup port-forward to router service if networking is enabled
	if cfg.NetworkingEnabled {
		fmt.Println("Setting up port-forward to router service...")
		var err error
		pfForwarder, err = utils.SetupPortForward(cfg.Namespace, "kthena-router", "8080", "80")
		if err != nil {
			return fmt.Errorf("failed to setup port-forward: %v", err)
		}
		// Note: SetupPortForward already waits for the port-forward to be ready.
		// Cleanup is handled by UninstallKthena via the global pfForwarder.
	}

	return nil
}

// UninstallKthena uninstalls kthena via helm
func UninstallKthena(namespace string) error {
	fmt.Printf("Uninstalling kthena from namespace %s\n", namespace)

	// Kill the port-forward process if it was started
	if pfForwarder != nil {
		fmt.Println("Stopping port-forward process...")
		pfForwarder.Close()
	}

	cmd := exec.Command("helm", "uninstall", "kthena", "--namespace", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Ignore error if already uninstalled
	_ = cmd.Run()

	// Kill any leftover port-forward processes as a fallback
	_ = exec.Command("pkill", "-f", "kubectl port-forward.*svc/kthena-router").Run()

	return nil
}
