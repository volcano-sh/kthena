//go:build e2e

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

package router

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/test/e2e/framework"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

// TestDebugServer tests that debug server is bound to localhost only and all debug endpoints work correctly
func TestDebugServer(t *testing.T) {
	kthenaNamespace := framework.NewDefaultConfig().Namespace

	routerPod := utils.GetRouterPod(t, testCtx.KubeClient, kthenaNamespace)
	require.NotEmpty(t, routerPod.Status.PodIP, "Router pod should have an IP address")
	t.Logf("Testing debug server on pod: %s/%s (IP: %s)", routerPod.Namespace, routerPod.Name, routerPod.Status.PodIP)

	// First, verify that debug server is bound to localhost only (not 0.0.0.0)
	// by attempting to access it via pod IP, which should fail
	t.Run("LocalhostOnly", func(t *testing.T) {
		debugURL := "http://" + routerPod.Status.PodIP + ":15000/debug/config_dump/modelroutes"

		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		resp, err := client.Get(debugURL)
		if err != nil {
			// Expected: connection should fail because debug server is only bound to localhost
			t.Logf("As expected, connection to debug server via pod IP failed: %v", err)
			assert.Error(t, err, "Debug server should not be accessible via pod IP (localhost only)")
			return
		}
		defer resp.Body.Close()

		// If we get here, the connection succeeded, which means the server is accessible via pod IP
		// This is a failure case - debug server should only be accessible via localhost
		t.Errorf("Debug server should not be accessible via pod IP %s, but connection succeeded with status %d", routerPod.Status.PodIP, resp.StatusCode)
	})

	// Then, test all debug endpoints via port-forward, use port-forward to access localhost:15000
	localPort := "15001" // Use a different local port to avoid conflicts
	podPort := "15000"   // Debug server port in the pod

	// Set up port-forward to access debug port
	pf, err := utils.SetupPortForwardToPod(routerPod.Namespace, routerPod.Name, localPort, podPort)
	require.NoError(t, err, "Failed to setup port-forward")
	defer pf.Close()

	t.Logf("Port-forward to %s:%s is ready", routerPod.Name, podPort)

	// List endpoints to test
	listEndpoints := []string{
		"/debug/config_dump/modelroutes",
		"/debug/config_dump/modelservers",
		"/debug/config_dump/pods",
		"/debug/config_dump/gateways",
		"/debug/config_dump/httproutes",
		"/debug/config_dump/inferencepools",
	}

	// Test list endpoints via port-forward
	for _, endpoint := range listEndpoints {
		t.Run("List"+strings.TrimPrefix(endpoint, "/debug/config_dump/"), func(t *testing.T) {
			debugURL := "http://localhost:" + localPort + endpoint

			client := &http.Client{
				Timeout: 5 * time.Second,
			}

			resp, err := client.Get(debugURL)
			require.NoError(t, err, "Failed to access debug endpoint: %s", debugURL)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusOK, resp.StatusCode, "Response should be 200 OK")

			// Verify response is valid JSON
			var result map[string]interface{}
			err = json.NewDecoder(resp.Body).Decode(&result)
			assert.NoError(t, err, "Response should be valid JSON")

			t.Logf("Successfully accessed endpoint %s", endpoint)
		})
	}
}
