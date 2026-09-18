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
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

const (
	healthPollInterval = 2 * time.Second
	monitorInterval    = 5 * time.Second
	// stopGracePeriod is how long a renderer gets to exit after SIGTERM
	// before it is killed.
	stopGracePeriod = 10 * time.Second
)

// RendererStatus is the lifecycle state of one renderer subprocess.
type RendererStatus string

const (
	StatusLoading RendererStatus = "loading"
	StatusReady   RendererStatus = "ready"
	StatusFailed  RendererStatus = "failed"
)

// RendererInfo is the /models view of a renderer.
type RendererInfo struct {
	Model     string `json:"model"`
	Source    string `json:"source"`
	Status    string `json:"status"`
	Port      int    `json:"port"`
	Restarts  int    `json:"restarts"`
	LastError string `json:"lastError"`
}

// renderer tracks one `vllm launch render` subprocess serving one model.
type renderer struct {
	model    string
	source   string
	port     int
	status   RendererStatus
	restarts int
	lastErr  string

	cmd    *exec.Cmd
	cancel context.CancelFunc
	// done is closed when the current subprocess exits.
	done chan struct{}
}

func (r *renderer) endpoint() string {
	return "http://127.0.0.1:" + strconv.Itoa(r.port)
}

// RendererManager reconciles desired models against running renderer
// subprocesses: it launches one `vllm launch render` per served model with a
// configured tokenizer source, waits for it to become healthy, and restarts
// crashed renderers within a bounded budget.
type RendererManager struct {
	config Config
	client *http.Client

	mu        sync.Mutex
	renderers map[string]*renderer
	closed    bool
}

func NewRendererManager(config Config) *RendererManager {
	return &RendererManager{
		config:    config,
		client:    &http.Client{Timeout: 5 * time.Second},
		renderers: make(map[string]*renderer),
	}
}

// Start runs the monitor loop until ctx is cancelled, then stops all
// renderers.
func (m *RendererManager) Start(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-ticker.C:
			m.restartCrashed()
		}
	}
}

// SetModels reconciles the set of served model names the service should hold
// tokenizers for (called by the ModelServer watcher).
func (m *RendererManager) SetModels(models map[string]struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}

	// Stop renderers whose model is no longer desired.
	for model, r := range m.renderers {
		if _, ok := models[model]; !ok {
			klog.Infof("Unloading tokenizer for model %s", model)
			r.cancel()
			delete(m.renderers, model)
		}
	}

	// Start renderers for newly desired models with a configured source.
	desired := make([]string, 0, len(models))
	for model := range models {
		desired = append(desired, model)
	}
	sort.Strings(desired)
	for _, model := range desired {
		if _, ok := m.renderers[model]; ok {
			continue
		}
		source, ok := m.config.ModelTokenizers[model]
		if !ok {
			klog.Warningf("No tokenizer source configured for served model %s "+
				"(set MODEL_TOKENIZERS); the router will fall back to engine-side "+
				"tokenization for it", model)
			continue
		}
		if len(m.renderers) >= m.config.MaxTokenizers {
			klog.Warningf("Max tokenizers (%d) reached, not loading model %s",
				m.config.MaxTokenizers, model)
			continue
		}
		r := &renderer{
			model:  model,
			source: source,
			port:   m.allocatePortLocked(),
			status: StatusLoading,
		}
		m.renderers[model] = r
		go m.launch(r)
	}
}

// EndpointFor returns the local endpoint of the ready renderer for a model.
func (m *RendererManager) EndpointFor(model string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.renderers[model]
	if !ok || r.status != StatusReady {
		return "", false
	}
	return r.endpoint(), true
}

// Snapshot returns the state of all renderers for the /models endpoint.
func (m *RendererManager) Snapshot() []RendererInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	infos := make([]RendererInfo, 0, len(m.renderers))
	for _, r := range m.renderers {
		infos = append(infos, RendererInfo{
			Model:     r.model,
			Source:    r.source,
			Status:    string(r.status),
			Port:      r.port,
			Restarts:  r.restarts,
			LastError: r.lastErr,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Model < infos[j].Model })
	return infos
}

func (m *RendererManager) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, r := range m.renderers {
		if r.cancel != nil {
			r.cancel()
		}
	}
	m.renderers = make(map[string]*renderer)
}

func (m *RendererManager) allocatePortLocked() int {
	used := make(map[int]struct{}, len(m.renderers))
	for _, r := range m.renderers {
		used[r.port] = struct{}{}
	}
	port := m.config.RendererBasePort
	for {
		if _, ok := used[port]; !ok {
			return port
		}
		port++
	}
}

// launch starts the renderer subprocess and waits for it to become healthy.
func (m *RendererManager) launch(r *renderer) {
	args := append([]string{}, m.config.RendererCommand...)
	// The model source is what the renderer loads; the served name is only
	// advertised, since it is an alias that may not be a loadable model id.
	args = append(args, r.source,
		"--served-model-name", r.model,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(r.port))
	args = append(args, m.config.RendererExtraArgs...)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Terminate gracefully on cancel; kill if the renderer lingers.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = stopGracePeriod
	done := make(chan struct{})

	klog.Infof("Starting renderer for model %s: %v", r.model, args)
	if err := cmd.Start(); err != nil {
		cancel()
		close(done)
		m.setFailed(r, fmt.Sprintf("failed to spawn renderer: %v", err))
		return
	}

	m.mu.Lock()
	if m.closed || m.renderers[r.model] != r {
		// The model was removed (or the service stopped) while starting.
		m.mu.Unlock()
		cancel()
		_ = cmd.Wait()
		close(done)
		return
	}
	r.cmd = cmd
	r.cancel = cancel
	r.done = done
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			klog.Warningf("Renderer for model %s exited: %v", r.model, err)
		}
		close(done)
	}()

	m.waitReady(r)
}

// waitReady polls the renderer health endpoint until it responds, the process
// exits, or the startup timeout elapses.
func (m *RendererManager) waitReady(r *renderer) {
	deadline := time.Now().Add(m.config.RendererStartupTimeout)
	url := r.endpoint() + "/health"
	for time.Now().Before(deadline) {
		select {
		case <-r.done:
			m.setFailed(r, "renderer exited during startup")
			return
		case <-time.After(healthPollInterval):
		}
		resp, err := m.client.Get(url)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			m.mu.Lock()
			if m.renderers[r.model] == r {
				r.status = StatusReady
				r.lastErr = ""
				klog.Infof("Tokenizer for model %s ready at %s", r.model, r.endpoint())
			}
			m.mu.Unlock()
			return
		}
	}
	m.setFailed(r, "renderer did not become healthy before timeout")
	r.cancel()
}

func (m *RendererManager) setFailed(r *renderer, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.renderers[r.model] != r {
		return
	}
	r.status = StatusFailed
	r.lastErr = msg
	klog.Errorf("Renderer for model %s failed: %s", r.model, msg)
}

// restartCrashed relaunches ready renderers whose subprocess exited, within
// the restart budget.
func (m *RendererManager) restartCrashed() {
	m.mu.Lock()
	var toRestart []*renderer
	for _, r := range m.renderers {
		if r.status != StatusReady || r.done == nil {
			continue
		}
		select {
		case <-r.done:
		default:
			continue
		}
		if r.restarts >= m.config.RendererMaxRestarts {
			r.status = StatusFailed
			r.lastErr = "restart budget exhausted"
			klog.Errorf("Renderer for model %s failed: restart budget exhausted", r.model)
			continue
		}
		r.restarts++
		r.status = StatusLoading
		toRestart = append(toRestart, r)
	}
	m.mu.Unlock()

	for _, r := range toRestart {
		klog.Warningf("Renderer for model %s exited, restarting (%d/%d)",
			r.model, r.restarts, m.config.RendererMaxRestarts)
		go m.launch(r)
	}
}
