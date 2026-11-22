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

package app

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	routerpkg "github.com/volcano-sh/kthena/pkg/kthena-router/router"
)

const testRouterConfig = "../../../pkg/kthena-router/scheduler/testdata/configmap.yaml"

// enableBatch points the batch API at a fresh Redis and a fresh directory.
func enableBatch(t *testing.T) (*miniredis.Miniredis, string) {
	t.Helper()
	redisServer := miniredis.RunT(t)
	root := t.TempDir()
	t.Setenv("BATCH_ENABLED", "true")
	t.Setenv("BATCH_STORAGE_PATH", root)
	t.Setenv("BATCH_MAX_FILE_BYTES", "")
	t.Setenv("REDIS_HOST", redisServer.Host())
	t.Setenv("REDIS_PORT", redisServer.Port())
	t.Setenv("REDIS_PASSWORD", "")
	return redisServer, root
}

// newBatchRouter builds a router and starts the batch API from the environment, as Run does.
func newBatchRouter(t *testing.T, configFile string) *routerpkg.Router {
	t.Helper()
	t.Setenv("ACCESS_LOG_ENABLED", "false")
	r := NewRouter(datastore.New(), configFile, common.NewTransportRegistry())
	// The storage checks end with the test, before its temporary volume is removed.
	if done := startBatch(t.Context(), r); done != nil {
		t.Cleanup(func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("the batch storage checks did not stop")
			}
		})
	}
	return r
}

func quietGin(t *testing.T) {
	t.Helper()
	writer := gin.DefaultWriter
	gin.DefaultWriter = io.Discard
	t.Cleanup(func() { gin.DefaultWriter = writer })
}

func defaultListener(t *testing.T, r *routerpkg.Router) http.Handler {
	t.Helper()
	quietGin(t)
	return newListenerHandler(listenerConfig{defaultRouter: r, readyCheck: func() bool { return true }})
}

func gatewayListener(t *testing.T, r *routerpkg.Router) http.Handler {
	t.Helper()
	quietGin(t)
	lm := &ListenerManager{
		router: r,
		server: &Server{Port: "8080"},
		portListeners: map[int32]*PortListenerInfo{
			80: {Listeners: []ListenerConfig{{GatewayKey: "default/gateway", ListenerName: "http", Port: 80}}},
		},
	}
	return newListenerHandler(listenerConfig{gateway: &listenerGatewayConfig{lm: lm, port: 80}})
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func request(method, path, token string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func uploadRequest(t *testing.T, content, token string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require.NoError(t, form.WriteField("purpose", "batch"))
	part, err := form.CreateFormFile("file", "input.jsonl")
	require.NoError(t, err)
	_, err = io.WriteString(part, content)
	require.NoError(t, err)
	require.NoError(t, form.Close())
	req := httptest.NewRequest(http.MethodPost, "/v1/files", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body), recorder.Body.String())
	return body
}

func errorType(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	failure, ok := decodeBody(t, recorder)["error"].(map[string]any)
	require.True(t, ok, recorder.Body.String())
	kind, _ := failure["type"].(string)
	return kind
}

func listedIDs(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	ids := []string{}
	for _, item := range decodeBody(t, recorder)["data"].([]any) {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	return ids
}

func TestBatchOptionsFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want batchOptions
	}{
		{name: "nothing set", want: batchOptions{storagePath: "/var/lib/kthena/batch"}},
		{
			name: "everything set",
			env:  map[string]string{"BATCH_ENABLED": "true", "BATCH_STORAGE_PATH": "/data", "BATCH_MAX_FILE_BYTES": "1024"},
			want: batchOptions{enabled: true, storagePath: "/data", maxFileBytes: 1024},
		},
		{
			name: "a switch that is not a boolean leaves batch off",
			env:  map[string]string{"BATCH_ENABLED": "on"},
			want: batchOptions{storagePath: defaultBatchStoragePath},
		},
		{
			name: "a size that is not a number keeps the default",
			env:  map[string]string{"BATCH_ENABLED": "true", "BATCH_MAX_FILE_BYTES": "200MB"},
			want: batchOptions{enabled: true, storagePath: defaultBatchStoragePath},
		},
		{
			name: "a negative size keeps the default",
			env:  map[string]string{"BATCH_ENABLED": "true", "BATCH_MAX_FILE_BYTES": "-1"},
			want: batchOptions{enabled: true, storagePath: defaultBatchStoragePath},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"BATCH_ENABLED", "BATCH_STORAGE_PATH", "BATCH_MAX_FILE_BYTES"} {
				t.Setenv(key, tt.env[key])
			}
			assert.Equal(t, tt.want, batchOptionsFromEnv())
		})
	}
}

func TestBatchAPIIsOffUnlessEnabled(t *testing.T) {
	t.Setenv("ACCESS_LOG_ENABLED", "false")
	untouched := NewRouter(datastore.New(), testRouterConfig, common.NewTransportRegistry())
	want := serve(defaultListener(t, untouched), request(http.MethodGet, "/v1/files", ""))

	for _, value := range []string{"", "false", "on"} {
		t.Run("BATCH_ENABLED="+value, func(t *testing.T) {
			enableBatch(t)
			t.Setenv("BATCH_ENABLED", value)
			handler := defaultListener(t, newBatchRouter(t, testRouterConfig))

			got := serve(handler, request(http.MethodGet, "/v1/files", ""))

			assert.Equal(t, want.Code, got.Code, "the request is answered as it was before the batch API existed")
			assert.Equal(t, want.Body.String(), got.Body.String())
		})
	}
}

func TestBatchFilesAPIThroughTheListener(t *testing.T) {
	tests := []struct {
		name     string
		listener func(*testing.T, *routerpkg.Router) http.Handler
	}{
		{name: "default listener", listener: defaultListener},
		{name: "gateway listener", listener: gatewayListener},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, root := enableBatch(t)
			handler := tt.listener(t, newBatchRouter(t, testRouterConfig))

			uploaded := serve(handler, uploadRequest(t, "{\"custom_id\":\"1\"}\n", ""))
			require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
			id, _ := decodeBody(t, uploaded)["id"].(string)
			require.NotEmpty(t, id)
			stored, err := filepath.Glob(filepath.Join(root, "*", id))
			require.NoError(t, err)
			assert.Len(t, stored, 1, "the bytes are kept under BATCH_STORAGE_PATH")

			assert.Equal(t, []string{id}, listedIDs(t, serve(handler, request(http.MethodGet, "/v1/files", ""))))
			content := serve(handler, request(http.MethodGet, "/v1/files/"+id+"/content", ""))
			assert.Equal(t, http.StatusOK, content.Code)
			assert.Equal(t, "{\"custom_id\":\"1\"}\n", content.Body.String())

			deleted := serve(handler, request(http.MethodDelete, "/v1/files/"+id, ""))
			assert.Equal(t, http.StatusOK, deleted.Code, deleted.Body.String())
			gone := serve(handler, request(http.MethodGet, "/v1/files/"+id+"/content", ""))
			assert.Equal(t, http.StatusNotFound, gone.Code)
			assert.Equal(t, "invalid_request_error", errorType(t, gone))
		})
	}
}

func TestBatchUploadLimitComesFromTheEnvironment(t *testing.T) {
	enableBatch(t)
	t.Setenv("BATCH_MAX_FILE_BYTES", "8")
	handler := defaultListener(t, newBatchRouter(t, testRouterConfig))

	fits := serve(handler, uploadRequest(t, "12345678", ""))
	assert.Equal(t, http.StatusOK, fits.Code, fits.Body.String())
	tooLarge := serve(handler, uploadRequest(t, "123456789", ""))
	assert.Equal(t, http.StatusRequestEntityTooLarge, tooLarge.Code, tooLarge.Body.String())
}

func TestBatchFileWithoutExpiryLivesThirtyDays(t *testing.T) {
	enableBatch(t)
	handler := defaultListener(t, newBatchRouter(t, testRouterConfig))

	uploaded := serve(handler, uploadRequest(t, "line\n", ""))
	require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
	body := decodeBody(t, uploaded)
	createdAt, _ := body["created_at"].(float64)
	expiresAt, ok := body["expires_at"].(float64)
	require.True(t, ok, "expires_at must be set: %v", body["expires_at"])
	assert.Equal(t, float64(2592000), expiresAt-createdAt)
}

func TestBatchAPIAnswers503UntilRedisIsBack(t *testing.T) {
	redisServer, _ := enableBatch(t)
	redisServer.Close()
	handler := defaultListener(t, newBatchRouter(t, testRouterConfig))

	down := serve(handler, request(http.MethodGet, "/v1/files", ""))
	require.Equal(t, http.StatusServiceUnavailable, down.Code, "Redis down at startup must not turn the API off")
	assert.Equal(t, "api_error", errorType(t, down))

	require.NoError(t, redisServer.Restart())
	assert.Empty(t, listedIDs(t, serve(handler, request(http.MethodGet, "/v1/files", ""))), "the API works again without a restart")
}

func TestBatchUploadAnswers503UntilTheVolumeIsUsable(t *testing.T) {
	_, root := enableBatch(t)
	volume := filepath.Join(root, "volume")
	require.NoError(t, os.WriteFile(volume, nil, 0o600), "a file sits where the volume should be")
	t.Setenv("BATCH_STORAGE_PATH", volume)
	handler := defaultListener(t, newBatchRouter(t, testRouterConfig))

	refused := serve(handler, uploadRequest(t, "line\n", ""))
	require.Equal(t, http.StatusServiceUnavailable, refused.Code, refused.Body.String())
	assert.Equal(t, "api_error", errorType(t, refused))
	listed := serve(handler, request(http.MethodGet, "/v1/files", ""))
	assert.Equal(t, http.StatusServiceUnavailable, listed.Code, "no files route answers while the volume is unusable: %s", listed.Body.String())

	require.NoError(t, os.Remove(volume))
	accepted := serve(handler, uploadRequest(t, "line\n", ""))
	assert.Equal(t, http.StatusOK, accepted.Code, "uploads work again without a restart")
}

// tokenIssuer signs tokens and serves the key set the router verifies them with.
type tokenIssuer struct {
	key     jwk.Key
	jwksURI string
}

func newTokenIssuer(t *testing.T) *tokenIssuer {
	t.Helper()
	raw, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	key, err := jwk.Import(raw)
	require.NoError(t, err)
	require.NoError(t, key.Set(jwk.KeyIDKey, "test-key"))
	public, err := key.PublicKey()
	require.NoError(t, err)
	keySet := jwk.NewSet()
	require.NoError(t, keySet.AddKey(public))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(keySet)
	}))
	t.Cleanup(server.Close)
	return &tokenIssuer{key: key, jwksURI: server.URL}
}

func (i *tokenIssuer) token(t *testing.T, issuer, subject string) string {
	t.Helper()
	token, err := jwt.NewBuilder().Issuer(issuer).Subject(subject).Expiration(time.Now().Add(time.Hour)).Build()
	require.NoError(t, err)
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.ES256(), i.key))
	require.NoError(t, err)
	return string(signed)
}

// routerConfig is the test router configuration with authentication against this issuer's keys.
func (i *tokenIssuer) routerConfig(t *testing.T) string {
	t.Helper()
	base, err := os.ReadFile(testRouterConfig)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "routerConfiguration.yaml")
	require.NoError(t, os.WriteFile(path, append(base, []byte("\nauth:\n  jwksUri: "+i.jwksURI+"\n")...), 0o600))
	return path
}

func TestSignedCallersOnlyReachTheirOwnFiles(t *testing.T) {
	enableBatch(t)
	issuer := newTokenIssuer(t)
	handler := defaultListener(t, newBatchRouter(t, issuer.routerConfig(t)))
	alice := issuer.token(t, "https://issuer.example", "alice")

	unsigned := serve(handler, uploadRequest(t, "line\n", ""))
	require.Equal(t, http.StatusUnauthorized, unsigned.Code, "authentication runs before the batch API")

	uploaded := serve(handler, uploadRequest(t, "line\n", alice))
	require.Equal(t, http.StatusOK, uploaded.Code, uploaded.Body.String())
	id, _ := decodeBody(t, uploaded)["id"].(string)
	assert.Equal(t, []string{id}, listedIDs(t, serve(handler, request(http.MethodGet, "/v1/files", alice))))

	strangers := []struct {
		name  string
		token string
	}{
		{name: "another subject of the same issuer", token: issuer.token(t, "https://issuer.example", "bob")},
		{name: "the same subject of another issuer", token: issuer.token(t, "https://other.example", "alice")},
	}
	for _, stranger := range strangers {
		t.Run(stranger.name, func(t *testing.T) {
			assert.Empty(t, listedIDs(t, serve(handler, request(http.MethodGet, "/v1/files", stranger.token))))
			for _, req := range []*http.Request{
				request(http.MethodGet, "/v1/files/"+id, stranger.token),
				request(http.MethodGet, "/v1/files/"+id+"/content", stranger.token),
				request(http.MethodDelete, "/v1/files/"+id, stranger.token),
			} {
				assert.Equal(t, http.StatusNotFound, serve(handler, req).Code, req.Method+" "+req.URL.Path)
			}
		})
	}

	kept := serve(handler, request(http.MethodGet, "/v1/files/"+id+"/content", alice))
	assert.Equal(t, http.StatusOK, kept.Code, "a stranger's delete did not touch the file")
	assert.Equal(t, "line\n", kept.Body.String())
}
