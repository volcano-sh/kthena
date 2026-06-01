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

package debug

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// TestDebugServerIntegration exercises the whole router wiring (gin engine +
// recovery + route table) by serving via httptest.NewServer and making a real
// HTTP round trip; it complements the per-handler unit tests in
// handlers_test.go.
func TestDebugServerIntegration(t *testing.T) {
	store := datastore.New()
	handler := NewDebugHandler(store)

	engine := gin.New()
	engine.Use(gin.Recovery())
	debugGroup := engine.Group("/debug/config_dump")
	{
		debugGroup.GET("/modelroutes", handler.ListModelRoutes)
		debugGroup.GET("/modelservers", handler.ListModelServers)
		debugGroup.GET("/pods", handler.ListPods)
		debugGroup.GET("/gateways", handler.ListGateways)
		debugGroup.GET("/httproutes", handler.ListHTTPRoutes)
		debugGroup.GET("/inferencepools", handler.ListInferencePools)

		debugGroup.GET("/namespaces/:namespace/modelroutes/:name", handler.GetModelRoute)
		debugGroup.GET("/namespaces/:namespace/modelservers/:name", handler.GetModelServer)
		debugGroup.GET("/namespaces/:namespace/pods/:name", handler.GetPod)
		debugGroup.GET("/namespaces/:namespace/gateways/:name", handler.GetGateway)
		debugGroup.GET("/namespaces/:namespace/httproutes/:name", handler.GetHTTPRoute)
		debugGroup.GET("/namespaces/:namespace/inferencepools/:name", handler.GetInferencePool)
	}

	srv := httptest.NewServer(engine.Handler())
	defer srv.Close()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(srv.URL + "/debug/config_dump/modelroutes")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
