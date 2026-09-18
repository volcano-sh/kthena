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

// Package tokenizer implements the Kthena tokenizer service: a GPU-free
// component that serves the vLLM-compatible /tokenize API for the router's
// kvcache-aware plugin, so scheduling does not have to call the backend
// inference engines.
//
// The control plane (ModelServer watching, renderer supervision, request
// routing) is implemented here in Go. The tokenization itself is delegated to
// `vllm launch render` — vLLM's lightweight, weight-free frontend — which only
// runs as a Python subprocess with an HTTP interface, so one renderer
// subprocess is supervised per model and requests are proxied to it.
package tokenizer

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// Config is the environment-based configuration of the tokenizer service.
type Config struct {
	// Host and Port the HTTP frontend binds to.
	Host string
	Port int
	// ModelTokenizers maps a served model name to the model source passed to
	// `vllm launch render`. The served name (ModelServer.spec.model) is an
	// engine-side alias and not necessarily a loadable model identifier, so
	// the source — a Hugging Face repository id or a mounted local path —
	// must be configured explicitly; the alias itself is passed to the
	// renderer via --served-model-name. Served models without a mapping are
	// skipped and the router falls back to engine-side tokenization for them.
	ModelTokenizers map[string]string
	// MaxTokenizers caps the number of concurrently running renderers.
	MaxTokenizers int
	// WatchNamespace restricts the ModelServer watch; empty means all
	// namespaces.
	WatchNamespace string
	// ResyncPeriod is the informer resync period.
	ResyncPeriod time.Duration
	// RendererBasePort is the first local port assigned to a renderer; each
	// renderer gets its own port starting from this value.
	RendererBasePort int
	// RendererCommand is the command used to launch a renderer. The model
	// source, --served-model-name and --host/--port flags are appended.
	RendererCommand []string
	// RendererExtraArgs is appended to every renderer command, e.g.
	// --trust-remote-code.
	RendererExtraArgs []string
	// RendererStartupTimeout is how long a renderer may take to become
	// healthy before it is marked failed.
	RendererStartupTimeout time.Duration
	// RendererMaxRestarts is the restart budget for a crashed renderer.
	RendererMaxRestarts int
	// ProxyTimeout bounds a proxied /tokenize request.
	ProxyTimeout time.Duration
}

// ConfigFromEnv builds the service configuration from environment variables,
// falling back to defaults for unset or invalid values.
func ConfigFromEnv() Config {
	return Config{
		Host:                   envString("TOKENIZER_HOST", "0.0.0.0"),
		Port:                   envInt("TOKENIZER_PORT", 8100),
		ModelTokenizers:        envModelTokenizers(),
		MaxTokenizers:          envInt("MAX_TOKENIZERS", 8),
		WatchNamespace:         envString("WATCH_NAMESPACE", ""),
		ResyncPeriod:           time.Duration(envInt("RESYNC_PERIOD_SECONDS", 300)) * time.Second,
		RendererBasePort:       envInt("RENDERER_BASE_PORT", 8200),
		RendererCommand:        envFields("VLLM_RENDER_COMMAND", "vllm launch render"),
		RendererExtraArgs:      envFields("VLLM_RENDER_EXTRA_ARGS", ""),
		RendererStartupTimeout: time.Duration(envInt("RENDERER_STARTUP_TIMEOUT_SECONDS", 600)) * time.Second,
		RendererMaxRestarts:    envInt("RENDERER_MAX_RESTARTS", 3),
		ProxyTimeout:           time.Duration(envInt("PROXY_TIMEOUT_SECONDS", 5)) * time.Second,
	}
}

func envString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		klog.Warningf("Invalid %s value %q, using default %d", name, v, def)
		return def
	}
	return n
}

// envFields splits a command-style env var on whitespace (no quoting support).
func envFields(name, def string) []string {
	return strings.Fields(envString(name, def))
}

// envModelTokenizers parses MODEL_TOKENIZERS, a JSON object mapping a served
// model name to its tokenizer source.
func envModelTokenizers() map[string]string {
	raw := os.Getenv("MODEL_TOKENIZERS")
	if raw == "" {
		return map[string]string{}
	}
	parsed := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		klog.Errorf("Invalid MODEL_TOKENIZERS JSON, ignoring: %v", err)
		return map[string]string{}
	}
	for k, v := range parsed {
		if v == "" {
			delete(parsed, k)
		}
	}
	return parsed
}
