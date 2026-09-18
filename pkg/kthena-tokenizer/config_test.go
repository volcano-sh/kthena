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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	cfg := ConfigFromEnv()
	assert.Equal(t, "0.0.0.0", cfg.Host)
	assert.Equal(t, 8100, cfg.Port)
	assert.Empty(t, cfg.ModelTokenizers)
	assert.Equal(t, 8, cfg.MaxTokenizers)
	assert.Equal(t, []string{"vllm", "launch", "render"}, cfg.RendererCommand)
	assert.Empty(t, cfg.RendererExtraArgs)
	assert.Equal(t, 10*time.Minute, cfg.RendererStartupTimeout)
	assert.Equal(t, 5*time.Second, cfg.ProxyTimeout)
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("TOKENIZER_PORT", "9000")
	t.Setenv("MODEL_TOKENIZERS", `{"deepseek-v3": "deepseek-ai/DeepSeek-V3", "empty": ""}`)
	t.Setenv("VLLM_RENDER_EXTRA_ARGS", "--trust-remote-code")
	cfg := ConfigFromEnv()
	assert.Equal(t, 9000, cfg.Port)
	assert.Equal(t, map[string]string{"deepseek-v3": "deepseek-ai/DeepSeek-V3"}, cfg.ModelTokenizers)
	assert.Equal(t, []string{"--trust-remote-code"}, cfg.RendererExtraArgs)
}

func TestConfigFromEnvInvalidValues(t *testing.T) {
	t.Setenv("TOKENIZER_PORT", "not-a-number")
	t.Setenv("MODEL_TOKENIZERS", "not-json")
	cfg := ConfigFromEnv()
	assert.Equal(t, 8100, cfg.Port)
	assert.Empty(t, cfg.ModelTokenizers)
}
