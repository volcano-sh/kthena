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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	TokenizerDeploymentName = "kthena-tokenizer"
	TokenizerServiceName    = "kthena-tokenizer"
	TokenizerPort           = "8100"
)

// tokenizerHTTPClient is used for all direct tokenizer HTTP calls. The 10 s timeout
// prevents a stalled renderer or port-forward from blocking a test indefinitely.
var tokenizerHTTPClient = &http.Client{Timeout: 10 * time.Second}

// GetTokenizerPod returns the first ready pod for the kthena-tokenizer standalone deployment.
func GetTokenizerPod(t *testing.T, kubeClient kubernetes.Interface, namespace string) *corev1.Pod {
	t.Helper()
	ctx := context.Background()
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=kthena-tokenizer",
	})
	require.NoError(t, err, "failed to list kthena-tokenizer pods")
	require.NotEmpty(t, pods.Items, "no kthena-tokenizer pods found")
	for i := range pods.Items {
		if IsPodReady(pods.Items[i]) {
			return &pods.Items[i]
		}
	}
	require.Fail(t, "no ready kthena-tokenizer pod found")
	return nil
}

// WaitForTokenizerModelReady polls the tokenizer pod logs until it logs that the model is ready.
func WaitForTokenizerModelReady(t *testing.T, kubeClient kubernetes.Interface, namespace, podName, model string, timeout time.Duration) {
	t.Helper()
	WaitForPodLogsContain(t, kubeClient, namespace, podName,
		timeout, // use the full timeout as the "since" window
		[]string{fmt.Sprintf("Tokenizer for model %s ready at", model)},
		timeout, defaultPollingInterval,
	)
}

// TokenizeRequest is the body sent to the tokenizer service's /tokenize endpoint.
type TokenizeRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// TokenizeResponse is the response from the tokenizer service's /tokenize endpoint.
type TokenizeResponse struct {
	Count       int   `json:"count"`
	MaxModelLen int   `json:"max_model_len"`
	Tokens      []int `json:"tokens"`
}

// SendTokenizeRequest sends a POST /tokenize to the tokenizer service and returns the response.
func SendTokenizeRequest(t *testing.T, baseURL, model, prompt string) (int, TokenizeResponse, error) {
	t.Helper()
	body, _ := json.Marshal(TokenizeRequest{Model: model, Prompt: prompt})
	resp, err := tokenizerHTTPClient.Post(baseURL+"/tokenize", "application/json", bytes.NewReader(body)) //nolint:gosec
	if err != nil {
		return 0, TokenizeResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, TokenizeResponse{}, fmt.Errorf("tokenize returned %d: %s", resp.StatusCode, raw)
	}
	var tr TokenizeResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return resp.StatusCode, TokenizeResponse{}, fmt.Errorf("decode tokenize response: %w", err)
	}
	return resp.StatusCode, tr, nil
}

// QueryTokenizerModels calls GET /models on the tokenizer service and returns the raw JSON.
func QueryTokenizerModels(t *testing.T, baseURL string) (map[string]interface{}, error) {
	t.Helper()
	resp, err := tokenizerHTTPClient.Get(baseURL + "/models") //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode /models response: %w", err)
	}
	return out, nil
}

// WaitForTokenizerHealthy polls GET /healthz until it returns 200 or the timeout expires.
func WaitForTokenizerHealthy(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		resp, err := tokenizerHTTPClient.Get(baseURL + "/healthz") //nolint:gosec
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, timeout, defaultPollingInterval, "tokenizer service did not become healthy at %s", baseURL)
}
