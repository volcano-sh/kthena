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
	"path/filepath"
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
		RendererStableWindow:   time.Second,
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

func TestRendererManagerRestartsStartupFailures(t *testing.T) {
	// A renderer that crashes on its first start is retried and recovers.
	cfg := testConfig(t, 18340, map[string]string{"served": "some/source"})
	marker := filepath.Join(t.TempDir(), "started-once")
	// Run via `sh -c` (rather than a script file) to avoid ETXTBSY races on
	// freshly written executables: fail the first start, then serve health.
	cfg.RendererCommand = []string{"sh", "-c", fmt.Sprintf(
		`if [ ! -f %q ]; then touch %q; exit 1; fi; exec %q -test.run=TestHelperRenderer -- "$@"`,
		marker, marker, os.Args[0]), "renderer"}
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusFailed)

	// The monitor loop is not running in this test, so drive retries here
	// until the renderer recovers.
	require.Eventually(t, func() bool {
		m.restartCrashed()
		_, ok := m.EndpointFor("served")
		return ok
	}, 20*time.Second, 100*time.Millisecond, "renderer did not recover after startup failure")
	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.GreaterOrEqual(t, infos[0].Restarts, 1)
}

func TestRendererManagerStartupRestartBudget(t *testing.T) {
	// A renderer that always fails to start stops being retried once the
	// restart budget is exhausted.
	cfg := testConfig(t, 18350, map[string]string{"served": "some/source"})
	cfg.RendererCommand = []string{filepath.Join(t.TempDir(), "no-such-renderer")}
	cfg.RendererMaxRestarts = 2
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusFailed)

	for i := 0; i < 4; i++ {
		m.restartCrashed()
		waitForStatus(t, m, "served", StatusFailed)
	}
	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, 2, infos[0].Restarts)
}

func TestRendererManagerUnloadsRendererThatNeverStarted(t *testing.T) {
	// Removing a model whose renderer failed to spawn must not panic: the
	// renderer has no process, so there is nothing to cancel.
	cfg := testConfig(t, 18360, map[string]string{"served": "some/source"})
	cfg.RendererCommand = []string{filepath.Join(t.TempDir(), "no-such-renderer")}
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusFailed)

	require.NotPanics(t, func() {
		m.SetModels(map[string]struct{}{})
	})
	assert.Empty(t, m.Snapshot())
}

// crashRenderer kills a renderer's subprocess and waits for the manager to
// observe the exit.
func crashRenderer(t *testing.T, m *RendererManager, model string) {
	t.Helper()
	m.mu.Lock()
	r := m.renderers[model]
	require.NotNil(t, r, "no renderer for model %s", model)
	cmd, done := r.cmd, r.done
	m.mu.Unlock()
	require.NotNil(t, cmd, "renderer for model %s never spawned", model)
	require.NoError(t, cmd.Process.Kill())
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("renderer for model %s did not exit after kill", model)
	}
}

func TestRendererManagerRestartBudgetResetsAfterStableRun(t *testing.T) {
	// A renderer that crashes, recovers and serves for a while must still be
	// restarted the next time it dies.
	cfg := testConfig(t, 18370, map[string]string{"served": "some/source"})
	cfg.RendererMaxRestarts = 1
	cfg.RendererStableWindow = 200 * time.Millisecond
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusReady)

	for i := 1; i <= 2; i++ {
		time.Sleep(2 * cfg.RendererStableWindow)
		crashRenderer(t, m, "served")
		// The monitor loop is not running in this test, so drive retries here.
		require.Eventually(t, func() bool {
			m.restartCrashed()
			_, ok := m.EndpointFor("served")
			return ok
		}, 20*time.Second, 100*time.Millisecond, "renderer did not recover from crash %d", i)
	}

	infos := m.Snapshot()
	require.Len(t, infos, 1)
	// Restarts is the lifetime total, not the budget.
	assert.Equal(t, 2, infos[0].Restarts)
}

func TestRendererManagerRestartBudgetIsNotRestoredByFlapping(t *testing.T) {
	// A renderer that becomes ready and dies straight away is still crash
	// looping, so it must stop being restarted once the budget is spent.
	cfg := testConfig(t, 18380, map[string]string{"served": "some/source"})
	cfg.RendererMaxRestarts = 2
	// Short enough that the test can wait past it, well above the time each
	// flap spends ready (the status poll notices within ~100ms).
	cfg.RendererStableWindow = 2 * time.Second
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusReady)

	for i := 1; i <= cfg.RendererMaxRestarts; i++ {
		crashRenderer(t, m, "served")
		require.Eventually(t, func() bool {
			m.restartCrashed()
			_, ok := m.EndpointFor("served")
			return ok
		}, 20*time.Second, 100*time.Millisecond, "renderer did not recover from crash %d", i)
	}

	crashRenderer(t, m, "served")
	require.Eventually(t, func() bool {
		m.restartCrashed()
		for _, info := range m.Snapshot() {
			if info.Model == "served" {
				return info.Status == string(StatusFailed)
			}
		}
		return false
	}, 20*time.Second, 100*time.Millisecond, "renderer kept restarting past its budget")

	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, "restart budget exhausted", infos[0].LastError)
	assert.Equal(t, cfg.RendererMaxRestarts, infos[0].Restarts)

	// The budget is restored by a stable run, not by lying dead for the
	// window. Nothing is running now, so later ticks must leave it failed.
	time.Sleep(2 * cfg.RendererStableWindow)
	m.restartCrashed()
	infos = m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, string(StatusFailed), infos[0].Status,
		"an exhausted renderer was relaunched after the window passed with it dead")
	assert.Equal(t, cfg.RendererMaxRestarts, infos[0].Restarts)
}

// TestRendererManagerBackoffDelaysRestart verifies that with RendererBackoffBase
// set, restartCrashed does not relaunch a failed renderer until the backoff
// window elapses.
func TestRendererManagerBackoffDelaysRestart(t *testing.T) {
	cfg := testConfig(t, 18370, map[string]string{"served": "some/source"})
	cfg.RendererCommand = []string{filepath.Join(t.TempDir(), "no-such-renderer")}
	cfg.RendererMaxRestarts = 3
	// 500 ms backoff: short enough for the test not to be flaky on slow CI,
	// long enough to verify that an immediately following tick is suppressed.
	cfg.RendererBackoffBase = 500 * time.Millisecond
	cfg.RendererBackoffMax = 5 * time.Second
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusFailed)

	// First explicit restart should proceed (nextRetryAfter is zero initially).
	m.restartCrashed()
	waitForStatus(t, m, "served", StatusFailed)
	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, 1, infos[0].Restarts, "first restart should have fired")

	// A second immediate call must be suppressed by the backoff window.
	m.restartCrashed()
	waitForStatus(t, m, "served", StatusFailed)
	infos = m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, 1, infos[0].Restarts, "second restart must be suppressed within backoff window")

	// After the window expires, the restart must proceed.
	require.Eventually(t, func() bool {
		m.restartCrashed()
		for _, info := range m.Snapshot() {
			if info.Model == "served" && info.Restarts >= 2 {
				return true
			}
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "restart should proceed after backoff window")
}

// TestRendererManagerBackoffExponentialIncrease verifies that the delay doubles
// with each consecutive restart.
func TestRendererManagerBackoffExponentialIncrease(t *testing.T) {
	tests := []struct {
		name      string
		base      time.Duration
		max       time.Duration
		attempt   int
		wantDelay time.Duration
	}{
		{name: "attempt_1_no_cap", base: 10 * time.Second, max: 0, attempt: 1, wantDelay: 10 * time.Second},
		{name: "attempt_2_no_cap", base: 10 * time.Second, max: 0, attempt: 2, wantDelay: 20 * time.Second},
		{name: "attempt_3_no_cap", base: 10 * time.Second, max: 0, attempt: 3, wantDelay: 40 * time.Second},
		{name: "attempt_4_capped", base: 10 * time.Second, max: 30 * time.Second, attempt: 4, wantDelay: 30 * time.Second},
		{name: "attempt_2_hits_cap", base: 10 * time.Second, max: 15 * time.Second, attempt: 2, wantDelay: 15 * time.Second},
		{name: "zero_base_returns_zero", base: 0, max: 300 * time.Second, attempt: 1, wantDelay: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{RendererBackoffBase: tt.base, RendererBackoffMax: tt.max}
			got := rendererBackoffDelay(cfg, tt.attempt)
			assert.Equal(t, tt.wantDelay, got)
		})
	}
}

// TestRendererManagerNoBackoffWhenBaseIsZero verifies backward-compatibility:
// when RendererBackoffBase is zero, restartCrashed retries on every call.
func TestRendererManagerNoBackoffWhenBaseIsZero(t *testing.T) {
	cfg := testConfig(t, 18380, map[string]string{"served": "some/source"})
	cfg.RendererCommand = []string{filepath.Join(t.TempDir(), "no-such-renderer")}
	cfg.RendererMaxRestarts = 3
	// Explicit zero disables backoff.
	cfg.RendererBackoffBase = 0
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	waitForStatus(t, m, "served", StatusFailed)

	// Three consecutive restartCrashed calls should each increment the counter
	// (no backoff suppression).
	for want := 1; want <= 3; want++ {
		require.Eventually(t, func() bool {
			m.restartCrashed()
			for _, info := range m.Snapshot() {
				if info.Model == "served" && info.Restarts == want {
					return true
				}
			}
			return false
		}, 5*time.Second, 50*time.Millisecond, "restart %d did not fire", want)
		waitForStatus(t, m, "served", StatusFailed)
	}

	// Budget is now exhausted; a further call must not increment.
	m.restartCrashed()
	waitForStatus(t, m, "served", StatusFailed)
	infos := m.Snapshot()
	require.Len(t, infos, 1)
	assert.Equal(t, 3, infos[0].Restarts, "restart counter must not exceed budget")
}

// TestRendererManagerBackoffResetOnRecovery verifies that nextRetryAfter is
// cleared when a renderer transitions to StatusReady, so a subsequent crash
// starts the backoff sequence fresh rather than reusing a stale future window.
func TestRendererManagerBackoffResetOnRecovery(t *testing.T) {
	cfg := testConfig(t, 18390, map[string]string{"served": "some/source"})
	marker := filepath.Join(t.TempDir(), "started-once")
	cfg.RendererCommand = []string{"sh", "-c", fmt.Sprintf(
		`if [ ! -f %q ]; then touch %q; exit 1; fi; exec %q -test.run=TestHelperRenderer -- "$@"`,
		marker, marker, os.Args[0]), "renderer"}
	cfg.RendererMaxRestarts = 3
	cfg.RendererBackoffBase = 100 * time.Millisecond
	cfg.RendererBackoffMax = 1 * time.Second
	m := NewRendererManager(cfg)
	defer m.shutdown()

	m.SetModels(map[string]struct{}{"served": {}})
	// First launch fails, sets a backoff window.
	waitForStatus(t, m, "served", StatusFailed)

	// Drive restarts until the renderer recovers.
	require.Eventually(t, func() bool {
		m.restartCrashed()
		for _, info := range m.Snapshot() {
			if info.Model == "served" && info.Status == string(StatusReady) {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "renderer did not recover")

	// Verify backoff was cleared: nextRetryAfter must be zero inside the
	// renderer struct (checked via the internal map, which is package-private).
	m.mu.Lock()
	r := m.renderers["served"]
	m.mu.Unlock()
	require.NotNil(t, r)
	assert.True(t, r.nextRetryAfter.IsZero(),
		"nextRetryAfter should be cleared after a successful ready transition")
}
