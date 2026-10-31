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

// Package tokenizer contains E2E tests for the kthena-tokenizer service in both
// standalone and sidecar deployment modes.
package tokenizer

import (
	"fmt"
	"os"
	"testing"

	"github.com/volcano-sh/kthena/test/e2e/framework"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

var (
	testCtx         *tokenizercontext.TokenizerTestContext
	testNamespace   string
	kthenaNamespace string
)

// TestMain installs kthena with the tokenizer service in standalone mode, sets up
// the test namespace and the common tokenizer-engine-mock backend, then runs all tests.
func TestMain(m *testing.M) {
	testNamespace = "kthena-e2e-tokenizer-" + utils.RandomString(5)

	config := framework.NewDefaultConfig()
	kthenaNamespace = config.Namespace

	config.TokenizerEnabled = true
	config.TokenizerMode = "standalone"
	// Override VLLM_RENDER_COMMAND to the in-image stub so the tokenizer service can
	// launch renderers without a real vllm installation.
	config.TokenizerRenderCommand = "/usr/local/bin/kthena-tokenizer-stub"
	// Map the test model to a dummy source; the stub ignores the source argument.
	config.TokenizerModels = map[string]string{
		"tokenizer-test-model": "stub-source",
	}

	if err := framework.InstallKthena(config); err != nil {
		fmt.Printf("Failed to install kthena: %v\n", err)
		os.Exit(1)
	}

	var err error
	testCtx, err = tokenizercontext.NewTokenizerTestContextWithPortForward(testNamespace, kthenaNamespace)
	if err != nil {
		fmt.Printf("Failed to create tokenizer test context: %v\n", err)
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	if err := testCtx.CreateTestNamespace(); err != nil {
		fmt.Printf("Failed to create test namespace: %v\n", err)
		testCtx.ClosePortForward()
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	if err := testCtx.SetupCommonComponents(); err != nil {
		fmt.Printf("Failed to setup common components: %v\n", err)
		_ = testCtx.DeleteTestNamespace()
		testCtx.ClosePortForward()
		_ = framework.UninstallKthena(config.Namespace)
		os.Exit(1)
	}

	code := m.Run()

	if err := testCtx.CleanupCommonComponents(); err != nil {
		fmt.Printf("Failed to cleanup common components: %v\n", err)
	}
	if err := testCtx.DeleteTestNamespace(); err != nil {
		fmt.Printf("Failed to delete test namespace: %v\n", err)
	}
	testCtx.ClosePortForward()
	if err := framework.UninstallKthena(config.Namespace); err != nil {
		fmt.Printf("Failed to uninstall kthena: %v\n", err)
	}

	os.Exit(code)
}
