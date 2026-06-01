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
			// this verifies that a string with no placeholders is returned unchanged
			name:     "NoPlaceholders",
			input:    "hello world",
			values:   map[string]interface{}{},
			expected: "hello world",
		},
		{
			// this verifies that a single string placeholder is replaced correctly
			name:  "SingleStringPlaceholder",
			input: "hello ${name}",
			values: map[string]interface{}{
				"name": "world",
			},
			expected: "hello world",
		},
		{
			// this verifies that multiple placeholders in one string are all replaced
			name:  "MultiplePlaceholders",
			input: "${greeting} ${name}",
			values: map[string]interface{}{
				"greeting": "hello",
				"name":     "world",
			},
			expected: "hello world",
		},
		{
			// this verifies that integer values are converted to string correctly
			name:  "IntegerPlaceholder",
			input: "port: ${port}",
			values: map[string]interface{}{
				"port": 8080,
			},
			expected: "port: 8080",
		},
		{
			// verifies that boolean values are converted to string correctly
			name:  "BooleanPlaceholder",
			input: "enabled: ${enabled}",
			values: map[string]interface{}{
				"enabled": true,
			},
			expected: "enabled: true",
		},
		{
			// verifies that a missing key returns an error
			name:    "MissingKey",
			input:   "hello ${missing}",
			values:  map[string]interface{}{},
			wantErr: true,
		},
		{
			// verifies that an unclosed placeholder returns an error
			name:    "UnclosedPlaceholder",
			input:   "hello ${name",
			values:  map[string]interface{}{"name": "world"},
			wantErr: true,
		},
		{
			// verifies that a placeholder at the start of the string is replaced
			name:  "PlaceholderAtStart",
			input: "${prefix}-suffix",
			values: map[string]interface{}{
				"prefix": "my",
			},
			expected: "my-suffix",
		},
		{
			// verifies that a placeholder at the end of the string is replaced
			name:  "PlaceholderAtEnd",
			input: "prefix-${suffix}",
			values: map[string]interface{}{
				"suffix": "end",
			},
			expected: "prefix-end",
		},
		{
			// verifies that a JSON object value is marshaled and embedded correctly
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
			// verifies that nil config returns an empty slice without error
			name:     "NilConfig",
			input:    nil,
			expected: []string{},
		},
		{
			// verifies that a config with nil Raw returns an empty slice
			name:     "NilRaw",
			input:    &apiextensionsv1.JSON{Raw: nil},
			expected: []string{},
		},
		{
			// verifies that a single string arg is converted to --key value format
			name: "SingleStringArg",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"model": "deepseek-r1"}`),
			},
			expected: []string{"--model", "deepseek-r1"},
		},
		{
			// verifies that a boolean true arg is converted correctly
			name: "BooleanArgTrue",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"trust_remote_code": true}`),
			},
			expected: []string{"--trust-remote-code", "true"},
		},
		{
			// verifies that a boolean false arg is converted correctly
			name: "BooleanArgFalse",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"trust_remote_code": false}`),
			},
			expected: []string{"--trust-remote-code", "false"},
		},
		{
			// verifies that multiple args are sorted alphabetically for determinism
			name: "MultipleArgsSorted",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"model": "deepseek", "dtype": "float16"}`),
			},
			expected: []string{"--dtype", "float16", "--model", "deepseek"},
		},
		{
			// verifies that underscores in keys are converted to dashes
			name: "UnderscoreConvertedToDash",
			input: &apiextensionsv1.JSON{
				Raw: []byte(`{"max_model_len": "4096"}`),
			},
			expected: []string{"--max-model-len", "4096"},
		},
		{
			// verifies that invalid JSON returns an error
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
			// verifies that an exact placeholder string is replaced with its value
			name:     "StringExactPlaceholder",
			data:     "${name}",
			values:   map[string]interface{}{"name": "kthena"},
			expected: "kthena",
		},
		{
			// verifies that an embedded placeholder within a string is replaced
			name:     "StringEmbeddedPlaceholder",
			data:     "hello-${name}",
			values:   map[string]interface{}{"name": "world"},
			expected: "hello-world",
		},
		{
			// verifies that placeholders within a map are replaced recursively
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
			// verifies that placeholders within a slice are replaced recursively
			name: "SliceWithPlaceholders",
			data: []interface{}{"${a}", "${b}"},
			values: map[string]interface{}{
				"a": "foo",
				"b": "bar",
			},
			expected: []interface{}{"foo", "bar"},
		},
		{
			// verifies that a missing placeholder key returns an error
			name:    "MissingPlaceholder",
			data:    "${missing}",
			values:  map[string]interface{}{},
			wantErr: true,
		},
		{
			// verifies that a plain string with no placeholders is returned unchanged
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
			// verifies that model and backend names are joined with a dash
			name:        "WithBackendName",
			modelName:   "my-model",
			backendName: "vllm",
			expected:    "my-model-vllm",
		},
		{
			// verifies that an empty backend name returns just the model name
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
	// verifies that GetModelControllerLabels returns the correct label map
	// with all expected keys populated from the model and backend info.
	model := makeTestModelBooster("test-model", "test-uid")
	labels := GetModelControllerLabels(model, "vllm", "v1")

	assert.Equal(t, "test-model", labels[ModelNameLabelKey])
	assert.Equal(t, "vllm", labels[BackendNameLabelKey])
	assert.Equal(t, "v1", labels[RevisionLabelKey])
	assert.Equal(t, "test-uid", labels[OwnerUIDKey])
	assert.Equal(t, "workload.serving.volcano.sh", labels[ManageBy])
}
