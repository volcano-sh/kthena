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

package utils

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestReplaceEmbeddedPlaceholders(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		values   map[string]interface{}
		expected string
		wantErr  bool
	}{
		{
			name:     "NoPlaceholders",
			input:    "hello world",
			values:   map[string]interface{}{},
			expected: "hello world",
		},
		{
			name:  "SingleStringPlaceholder",
			input: "hello ${name}",
			values: map[string]interface{}{
				"name": "world",
			},
			expected: "hello world",
		},
		{
			name:  "MultiplePlaceholders",
			input: "${greeting} ${name}",
			values: map[string]interface{}{
				"greeting": "hello",
				"name":     "world",
			},
			expected: "hello world",
		},
		{
			name:  "IntegerPlaceholder",
			input: "port: ${port}",
			values: map[string]interface{}{
				"port": 8080,
			},
			expected: "port: 8080",
		},
		{
			name:  "BooleanPlaceholder",
			input: "enabled: ${enabled}",
			values: map[string]interface{}{
				"enabled": true,
			},
			expected: "enabled: true",
		},
		{
			name:    "MissingKey",
			input:   "hello ${missing}",
			values:  map[string]interface{}{},
			wantErr: true,
		},
		{
			name:    "UnclosedPlaceholder",
			input:   "hello ${name",
			values:  map[string]interface{}{"name": "world"},
			wantErr: true,
		},
		{
			name:  "PlaceholderAtStart",
			input: "${prefix}-suffix",
			values: map[string]interface{}{
				"prefix": "my",
			},
			expected: "my-suffix",
		},
		{
			name:  "PlaceholderAtEnd",
			input: "prefix-${suffix}",
			values: map[string]interface{}{
				"suffix": "end",
			},
			expected: "prefix-end",
		},
		{
			name:  "JSONObjectPlaceholder",
			input: "config: ${cfg}",
			values: map[string]interface{}{
				"cfg": map[string]interface{}{"key": "val"},
			},
			expected: `config: {"key":"val"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ReplaceEmbeddedPlaceholders(tt.input, &tt.values)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestConvertVLLMArgsFromJson(t *testing.T) {
	tests := []struct {
		name     string
		input    *apiextensionsv1.JSON
		expected []string
		wantErr  bool
	}{
		{
			name:     "NilConfig",
			input:    nil,
			expected: []string{},
		},
		{
			name:     "NilRaw",
			input:    &apiextensionsv1.JSON{Raw: nil},
			expected: []string{},
		},
		{
			name: "SingleStringArg",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"model": "deepseek-r1"}`),
			},
			expected: []string{"--model", "deepseek-r1"},
		},
		{
			name: "BooleanArgTrue",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"trust_remote_code": true}`),
			},
			expected: []string{"--trust-remote-code", "true"},
		},
		{
			name: "BooleanArgFalse",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"trust_remote_code": false}`),
			},
			expected: []string{"--trust-remote-code", "false"},
		},
		{
			name: "MultipleArgsSorted",
			// Keys are sorted alphabetically so output is deterministic
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"model": "deepseek", "dtype": "float16"}`),
			},
			expected: []string{"--dtype", "float16", "--model", "deepseek"},
		},
		{
			name: "UnderscoreConvertedToDash",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"max_model_len": "4096"}`),
			},
			expected: []string{"--max-model-len", "4096"},
		},
		{
			name:    "InvalidJSON",
			input:   &apiextensionsv1.JSON{Raw: []byte(`invalid`)},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ConvertVLLMArgsFromJson(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestReplacePlaceholders(t *testing.T) {
	tests := []struct {
		name     string
		data     interface{}
		values   map[string]interface{}
		expected interface{}
		wantErr  bool
	}{
		{
			name:     "StringExactPlaceholder",
			data:     "${name}",
			values:   map[string]interface{}{"name": "kthena"},
			expected: "kthena",
		},
		{
			name:     "StringEmbeddedPlaceholder",
			data:     "hello-${name}",
			values:   map[string]interface{}{"name": "world"},
			expected: "hello-world",
		},
		{
			name: "MapWithPlaceholders",
			data: map[string]interface{}{
				"model": "${model_name}",
				"port":  "${port}",
			},
			values: map[string]interface{}{
				"model_name": "deepseek",
				"port":       float64(8080),
			},
			expected: map[string]interface{}{
				"model": "deepseek",
				"port":  float64(8080),
			},
		},
		{
			name: "SliceWithPlaceholders",
			data: []interface{}{"${a}", "${b}"},
			values: map[string]interface{}{
				"a": "foo",
				"b": "bar",
			},
			expected: []interface{}{"foo", "bar"},
		},
		{
			name:    "MissingPlaceholder",
			data:    "${missing}",
			values:  map[string]interface{}{},
			wantErr: true,
		},
		{
			name:     "NoPlaceholder",
			data:     "plain string",
			values:   map[string]interface{}{},
			expected: "plain string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data
			err := ReplacePlaceholders(&data, &tt.values)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, data)
			}
		})
	}
}

func TestGetBackendResourceName(t *testing.T) {
	tests := []struct {
		name        string
		modelName   string
		backendName string
		expected    string
	}{
		{
			name:        "WithBackendName",
			modelName:   "my-model",
			backendName: "vllm",
			expected:    "my-model-vllm",
		},
		{
			name:        "EmptyBackendName",
			modelName:   "my-model",
			backendName: "",
			expected:    "my-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetBackendResourceName(tt.modelName, tt.backendName)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetModelControllerLabels(t *testing.T) {
	// Verifies that GetModelControllerLabels returns the correct label map
	// with all expected keys populated from model and backend info.
	model := makeTestModelBooster("test-model", "test-uid")
	labels := GetModelControllerLabels(model, "vllm", "v1")

	assert.Equal(t, "test-model", labels[ModelNameLabelKey])
	assert.Equal(t, "vllm", labels[BackendNameLabelKey])
	assert.Equal(t, "v1", labels[RevisionLabelKey])
	assert.Equal(t, "test-uid", labels[OwnerUIDKey])
	assert.NotEmpty(t, labels[ManageBy])
}

// helper to build a minimal JSON raw bytes
func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
