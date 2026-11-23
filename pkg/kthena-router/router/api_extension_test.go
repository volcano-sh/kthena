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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// prefixExtension claims every path under its prefix and records what it served.
type prefixExtension struct {
	prefix string
	served []string
}

func (e *prefixExtension) Handles(_, path string) bool {
	return strings.HasPrefix(path, e.prefix)
}

func (e *prefixExtension) Serve(c *gin.Context) {
	e.served = append(e.served, c.Request.Method+" "+c.Request.URL.Path)
	c.String(http.StatusOK, "served by the extension")
}

func serveThroughRouter(t *testing.T, router *Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(`{"model":"no-such-model"}`))
	router.HandlerFunc()(c)
	return recorder
}

func TestAPIExtension(t *testing.T) {
	tests := []struct {
		name string
		// prefix is what the extension claims; "/" claims every path.
		prefix     string
		method     string
		path       string
		wantServed bool
	}{
		{name: "a claimed path goes to the extension", prefix: "/v1/files", method: http.MethodPost, path: "/v1/files", wantServed: true},
		{name: "a claimed sub path goes to the extension", prefix: "/v1/files", method: http.MethodGet, path: "/v1/files/file-1/content", wantServed: true},
		{name: "a model request stays on the model path", prefix: "/v1/files", method: http.MethodPost, path: "/v1/chat/completions"},
		{name: "a path outside /v1 is never offered, as authentication skips it", prefix: "/", method: http.MethodPost, path: "/files"},
		{name: "the model list is answered before the extension", prefix: "/", method: http.MethodGet, path: "/v1/models"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, _, backend := setupTestRouter(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			defer backend.Close()
			extension := &prefixExtension{prefix: tt.prefix}
			router.SetAPIExtension(extension)

			recorder := serveThroughRouter(t, router, tt.method, tt.path)

			if !tt.wantServed {
				assert.Empty(t, extension.served)
				assert.NotEqual(t, "served by the extension", recorder.Body.String())
				return
			}
			assert.Equal(t, []string{tt.method + " " + tt.path}, extension.served)
			assert.Equal(t, http.StatusOK, recorder.Code)
			assert.Equal(t, "served by the extension", recorder.Body.String())
		})
	}
}

func TestWithoutAnAPIExtensionEveryPathIsAModelRequest(t *testing.T) {
	router, _, backend := setupTestRouter(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	recorder := serveThroughRouter(t, router, http.MethodPost, "/v1/files")

	assert.Equal(t, http.StatusNotFound, recorder.Code, "the model path answers, as it did before extensions existed")
}
