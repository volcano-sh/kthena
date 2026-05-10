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

package connectors

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestHTTPConnector(t *testing.T) {
	connector := NewHTTPConnector()

	if connector.Name() != "default" {
		t.Errorf("Expected HTTP connector name 'default', got '%s'", connector.Name())
	}
}

func TestNIXLConnector(t *testing.T) {
	connector := NewNIXLConnector()

	if connector.Name() != "nixl" {
		t.Errorf("Expected NIXL connector name 'nixl', got '%s'", connector.Name())
	}
}

func TestFactory(t *testing.T) {
	factory := NewDefaultFactory()

	// Test HTTP connector
	httpConnector := factory.GetConnector(v1alpha1.ConnectorTypeHTTP)
	if httpConnector == nil {
		t.Error("Expected HTTP connector to be registered")
	}
	if httpConnector != nil && httpConnector.Name() != "default" {
		t.Errorf("Expected HTTP connector name 'default', got '%s'", httpConnector.Name())
	}

	// Test NIXL connector
	nixlConnector := factory.GetConnector(v1alpha1.ConnectorTypeNIXL)
	if nixlConnector == nil {
		t.Error("Expected NIXL connector to be registered")
	}
	if nixlConnector != nil && nixlConnector.Name() != "nixl" {
		t.Errorf("Expected NIXL connector name 'nixl', got '%s'", nixlConnector.Name())
	}

	// Test LMCache connector (currently uses HTTP implementation)
	lmcacheConnector := factory.GetConnector(v1alpha1.ConnectorTypeLMCache)
	if lmcacheConnector == nil {
		t.Error("Expected LMCache connector to be registered")
	}
	if lmcacheConnector != nil && lmcacheConnector.Name() != "default" {
		t.Errorf("Expected LMCache connector name 'default' (using HTTP implementation), got '%s'", lmcacheConnector.Name())
	}

	// Test unknown connector type
	unknownConnector := factory.GetConnector("unknown")
	if unknownConnector == nil {
		t.Error("Expected LMCache connector to be registered")
	}
	if unknownConnector != nil && unknownConnector.Name() != "default" {
		t.Errorf("Expected unknown connector name 'default' (using HTTP implementation), got '%s'", unknownConnector.Name())
	}
}

// Helper function to parse request body
func parseRequestBody(req *http.Request) (map[string]interface{}, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	// Reset the body for potential future reads
	req.Body = io.NopCloser(bytes.NewBuffer(body))

	var result map[string]interface{}
	err = json.Unmarshal(body, &result)
	return result, err
}

func TestHTTPConnectorProxy(t *testing.T) {
	t.Run("StreamingRequestBuildsLocalRequests", func(t *testing.T) {
		var prefillBody, decodeBody map[string]interface{}

		prefillServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			prefillBody, err = parseRequestBody(r)
			if err != nil {
				t.Errorf("Failed to parse prefill request body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer prefillServer.Close()

		decodeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var err error
			decodeBody, err = parseRequestBody(r)
			if err != nil {
				t.Errorf("Failed to parse decode request body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"completion_tokens":2}}`))
		}))
		defer decodeServer.Close()

		req, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req

		_, err := NewHTTPConnector().Proxy(c, map[string]interface{}{
			"model":      "test-model",
			"stream":     true,
			"max_tokens": 200,
			"stream_options": map[string]interface{}{
				"some_other_option": "value",
			},
		}, prefillServer.Listener.Addr().String(), decodeServer.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Expected proxy to succeed, got %v", err)
		}

		if prefillBody["max_tokens"] != float64(1) {
			t.Errorf("Expected prefill max_tokens to be 1, got %v", prefillBody["max_tokens"])
		}
		if _, ok := prefillBody["stream"]; ok {
			t.Error("Expected prefill request to remove stream")
		}
		if _, ok := prefillBody["stream_options"]; ok {
			t.Error("Expected prefill request to remove stream_options")
		}
		if decodeBody["stream"] != true {
			t.Errorf("Expected decode request stream to be true, got %v", decodeBody["stream"])
		}
		if decodeBody["max_tokens"] != float64(200) {
			t.Errorf("Expected decode max_tokens to be 200, got %v", decodeBody["max_tokens"])
		}
		streamOptions, ok := decodeBody["stream_options"].(map[string]interface{})
		if !ok || streamOptions["include_usage"] != true {
			t.Errorf("Expected decode stream_options include_usage, got %+v", decodeBody["stream_options"])
		}
	})
}
