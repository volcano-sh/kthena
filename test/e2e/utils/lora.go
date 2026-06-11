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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// LoadLoRAAdapterOnPod port-forwards to the pod and loads a LoRA adapter via /v1/load_lora_adapter.
func LoadLoRAAdapterOnPod(t *testing.T, pod corev1.Pod, loraName, loraPath string) {
	t.Helper()
	localPort := AllocateLocalPort(t)
	pf, err := SetupPortForwardToPod(pod.Namespace, pod.Name, localPort, "8000")
	require.NoError(t, err, "Failed to setup port-forward to pod %s", pod.Name)
	defer pf.Close()
	LoadLoRAAdapter(t, fmt.Sprintf("http://127.0.0.1:%s", localPort), loraName, loraPath)
}

// LoadLoRAAdapter loads a LoRA adapter directly on the LLM-Mock pod by sending a request to /v1/load_lora_adapter
// The request is sent directly to the specified pod URL (e.g., http://127.0.0.1:9000/v1/load_lora_adapter)
// Note: This should NOT be sent through the router, as /v1/load_lora_adapter is a management endpoint
func LoadLoRAAdapter(t *testing.T, podURL string, loraName string, loraPath string) {
	loadURL := strings.TrimSuffix(podURL, "/") + "/v1/load_lora_adapter"

	requestBody := map[string]interface{}{
		"lora_name": loraName,
		"lora_path": loraPath,
	}

	jsonData, err := json.Marshal(requestBody)
	require.NoError(t, err, "Failed to marshal load LoRA adapter request body")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("POST", loadURL, bytes.NewBuffer(jsonData))
	require.NoError(t, err, "Failed to create HTTP request")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err, "Failed to send HTTP request")
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read response body")

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected HTTP 200 for loading LoRA adapter")
	t.Logf("Successfully loaded LoRA adapter %s: %s", loraName, string(responseBody))
}

// UnloadLoRAAdapter unloads a LoRA adapter directly on the LLM-Mock pod by sending a request to /v1/unload_lora_adapter
// The request is sent directly to the specified pod URL (e.g., http://127.0.0.1:9000/v1/unload_lora_adapter)
// Note: This should NOT be sent through the router, as /v1/unload_lora_adapter is a management endpoint
func UnloadLoRAAdapter(t *testing.T, podURL string, loraName string) {
	unloadURL := strings.TrimSuffix(podURL, "/") + "/v1/unload_lora_adapter"

	requestBody := map[string]interface{}{
		"lora_name": loraName,
	}

	jsonData, err := json.Marshal(requestBody)
	require.NoError(t, err, "Failed to marshal unload LoRA adapter request body")

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	req, err := http.NewRequest("POST", unloadURL, bytes.NewBuffer(jsonData))
	require.NoError(t, err, "Failed to create HTTP request")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(t, err, "Failed to send HTTP request")
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "Failed to read response body")

	assert.Equal(t, http.StatusOK, resp.StatusCode, "Expected HTTP 200 for unloading LoRA adapter")
	t.Logf("Successfully unloaded LoRA adapter %s: %s", loraName, string(responseBody))
}
