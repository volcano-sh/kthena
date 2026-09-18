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

package tokenizer

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperRenderer is not a real test: it is re-executed as a fake
// `vllm launch render` subprocess that serves /health on the requested port.
func TestHelperRenderer(t *testing.T) {
	if os.Getenv("GO_TEST_HELPER_RENDERER") != "1" {
		return
	}
	port := 0
	args := os.Args
	for i, arg := range args {
		if arg == "--port" && i+1 < len(args) {
			port, _ = strconv.Atoi(args[i+1])
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func testConfig(t *testing.T, basePort int, models map[string]string) Config {
	t.Setenv("GO_TEST_HELPER_RENDERER", "1")
	return Config{
		ModelTokenizers:        models,
		MaxTokenizers:          8,
		RendererBasePort:       basePort,
		RendererCommand:        []string{os.Args[0], "-test.run=TestHelperRenderer", "--"},
		RendererStartupTimeout: 30 * time.Second,
		RendererMaxRestarts:    3,
		ProxyTimeout:           5 * time.Second,
	}
}

func waitForStatus(t *testing.T, m *RendererManager, model string, status RendererStatus) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, info := range m.Snapshot() {
			if info.Model == model && info.Status == string(status) {
				return true
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "model %s did not reach status %s", model, status)
}

func TestRendererManagerLifecycle(t *testing.T) {
	m := NewRendererManager(testConfig(t, 18300, map[string]string{"served": "some/source"}))
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusReady)

	endpoint, ok := m.EndpointFor("served")
	require.True(t, ok)
	assert.Equal(t, "http://127.0.0.1:18300", endpoint)

	// Removing the model stops and forgets its renderer.
	m.SetModels(map[string]struct{}{})
	_, ok = m.EndpointFor("served")
	assert.False(t, ok)
	assert.Empty(t, m.Snapshot())
}

func TestRendererManagerSkipsUnmappedModels(t *testing.T) {
	m := NewRendererManager(testConfig(t, 18310, map[string]string{"served": "some/source"}))
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}, "unmapped": {}})
	waitForStatus(t, m, "served", StatusReady)

	assert.Len(t, m.Snapshot(), 1)
	_, ok := m.EndpointFor("unmapped")
	assert.False(t, ok)
}

func TestRendererManagerRespectsCap(t *testing.T) {
	cfg := testConfig(t, 18320, map[string]string{"a": "src/a", "b": "src/b"})
	cfg.MaxTokenizers = 1
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"a": {}, "b": {}})
	assert.Len(t, m.Snapshot(), 1)
}

func TestRendererManagerNotReadyWhileLoading(t *testing.T) {
	// A command that never serves /health stays loading and is not routable.
	cfg := testConfig(t, 18330, map[string]string{"served": "some/source"})
	cfg.RendererCommand = []string{"sleep", "60"}
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	_, ok := m.EndpointFor("served")
	assert.False(t, ok)
	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, string(StatusLoading), infos[0].Status)
}
