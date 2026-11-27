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

package batch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chatLine = `{"custom_id":"q1","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[]}}` + "\n"

func createRequest(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, batchesPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func createBody(input string) string {
	return `{"input_file_id":"` + input + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
}

func mustCreate(t *testing.T, api *testAPI, tenant, input string) map[string]any {
	t.Helper()
	rec := api.call(t, tenant, createRequest(createBody(input)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decode(t, rec)
}

func TestCreateAndGetABatch(t *testing.T) {
	api := newTestAPI(t)
	api.service.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	input := mustUpload(t, api, "alice", chatLine)

	body := `{"input_file_id":"` + input + `","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"run":"7"}}`
	rec := api.call(t, "alice", createRequest(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	created := decode(t, rec)
	id, _ := created["id"].(string)
	assert.True(t, strings.HasPrefix(id, "batch_"), id)
	want := map[string]any{
		"id":                id,
		"object":            "batch",
		"endpoint":          "/v1/chat/completions",
		"model":             nil,
		"errors":            nil,
		"input_file_id":     input,
		"completion_window": "24h",
		"status":            "validating",
		"output_file_id":    nil,
		"error_file_id":     nil,
		"created_at":        float64(1_800_000_000),
		"in_progress_at":    nil,
		"expires_at":        float64(1_800_000_000 + 24*3600),
		"finalizing_at":     nil,
		"completed_at":      nil,
		"failed_at":         nil,
		"expired_at":        nil,
		"cancelling_at":     nil,
		"cancelled_at":      nil,
		"request_counts":    map[string]any{"total": float64(0), "completed": float64(0), "failed": float64(0)},
		"usage":             nil,
		"metadata":          map[string]any{"run": "7"},
	}
	assert.Equal(t, want, created, "every field of the OpenAI batch object is there, unset ones as null")

	got := api.call(t, "alice", httptest.NewRequest(http.MethodGet, batchesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Equal(t, want, decode(t, got))

	job, err := api.store.GetJob(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "alice", job.Subject, "the lines will run as the caller")
	assert.Equal(t, tenantID("", "alice"), job.Tenant)
}

func TestCreateBatchChecksTheRequest(t *testing.T) {
	long := func(n int) string { return strings.Repeat("é", n) }
	pairs := func(n int) string {
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			parts = append(parts, fmt.Sprintf(`"k%d":"v"`, i))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	cases := []struct {
		name   string
		body   string
		status int
		param  any
	}{
		{"not JSON", `input_file_id=file-1`, http.StatusBadRequest, nil},
		{"a JSON array", `[]`, http.StatusBadRequest, nil},
		{"two objects", createBody("INPUT") + `{}`, http.StatusBadRequest, nil},
		{"an unknown parameter", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","output_expires_after":{"anchor":"created_at","seconds":3600}}`, http.StatusBadRequest, "output_expires_after"},
		{"no input file", `{"endpoint":"/v1/chat/completions","completion_window":"24h"}`, http.StatusBadRequest, "input_file_id"},
		{"another endpoint", `{"input_file_id":"INPUT","endpoint":"/v1/embeddings","completion_window":"24h"}`, http.StatusBadRequest, "endpoint"},
		{"no endpoint", `{"input_file_id":"INPUT","completion_window":"24h"}`, http.StatusBadRequest, "endpoint"},
		{"another window", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"48h"}`, http.StatusBadRequest, "completion_window"},
		{"metadata that is not text", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"n":1}}`, http.StatusBadRequest, nil},
		{"16 metadata pairs", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":` + pairs(16) + `}`, http.StatusOK, nil},
		{"17 metadata pairs", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":` + pairs(17) + `}`, http.StatusBadRequest, "metadata"},
		{"a 64 character key", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"` + long(64) + `":"v"}}`, http.StatusOK, nil},
		{"a 65 character key", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"` + long(65) + `":"v"}}`, http.StatusBadRequest, "metadata"},
		{"a 512 character value", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"k":"` + long(512) + `"}}`, http.StatusOK, nil},
		{"a 513 character value", `{"input_file_id":"INPUT","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"k":"` + long(513) + `"}}`, http.StatusBadRequest, "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPI(t)
			input := mustUpload(t, api, "alice", chatLine)
			rec := api.call(t, "alice", createRequest(strings.ReplaceAll(tc.body, "INPUT", input)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status != http.StatusOK {
				body := errorOf(t, rec)
				assert.Equal(t, errInvalidRequest, body["type"])
				assert.Equal(t, tc.param, body["param"])
				assert.False(t, api.mini.Exists(api.store.queueKey()), "a refused request queues nothing")
			}
		})
	}
}

func TestCreateBatchSaysTheInputFileIsMissing(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", createRequest(`{"endpoint":"/v1/chat/completions","completion_window":"24h"}`))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "input_file_id is required", errorOf(t, rec)["message"], "an empty ID is named as missing, not looked up")
}

func TestCreateBatchRefusesAnOversizedBody(t *testing.T) {
	api := newTestAPI(t)
	body := `{"input_file_id":"` + strings.Repeat("x", maxCreateBody) + `"}`
	rec := api.call(t, "alice", createRequest(body))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
}

func TestCreateBatchRefusesAnUnusableInput(t *testing.T) {
	api := newTestAPI(t)
	bobs := mustUpload(t, api, "bob", chatLine)
	for name, input := range map[string]string{"missing": "file-none", "another tenant's": bobs} {
		t.Run(name, func(t *testing.T) {
			rec := api.call(t, "alice", createRequest(createBody(input)))
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			body := errorOf(t, rec)
			assert.Equal(t, "input_file_id", body["param"])
			assert.Equal(t, "no such file: "+input, body["message"], "another tenant's file looks exactly like a missing one")
		})
	}
	t.Run("a result file", func(t *testing.T) {
		require.NoError(t, api.store.CreateFile(context.Background(), &File{ID: "file-out", Tenant: tenantID("", "alice"), Purpose: PurposeBatchOutput, CreatedAt: 1}))
		rec := api.call(t, "alice", createRequest(createBody("file-out")))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, "input_file_id", errorOf(t, rec)["param"])
	})
}

func TestGetBatchIsOnlyForItsOwner(t *testing.T) {
	api := newTestAPI(t)
	id := mustCreate(t, api, "alice", mustUpload(t, api, "alice", chatLine))["id"].(string)
	for name, call := range map[string]func() *httptest.ResponseRecorder{
		"another tenant": func() *httptest.ResponseRecorder {
			return api.call(t, "bob", httptest.NewRequest(http.MethodGet, batchesPath+"/"+id, nil))
		},
		"a missing batch": func() *httptest.ResponseRecorder {
			return api.call(t, "alice", httptest.NewRequest(http.MethodGet, batchesPath+"/batch_none", nil))
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := call()
			require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.Equal(t, "id", errorOf(t, rec)["param"])
		})
	}
}

// failingJobs answers every batch call with an outage, while files keep working.
type failingJobs struct{ Store }

func (failingJobs) CreateJob(context.Context, *Job) error {
	return errors.New("i/o timeout")
}

func (failingJobs) GetJob(context.Context, string) (*Job, error) {
	return nil, errors.New("i/o timeout")
}

func TestBatchesAnswer503WhenTheStoreIsDown(t *testing.T) {
	api := newTestAPI(t)
	input := mustUpload(t, api, "alice", chatLine)
	api.service.store = failingJobs{api.store}
	for name, req := range map[string]*http.Request{
		"create": createRequest(createBody(input)),
		"get":    httptest.NewRequest(http.MethodGet, batchesPath+"/batch_1", nil),
	} {
		t.Run(name, func(t *testing.T) {
			rec := api.call(t, "alice", req)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Equal(t, errAPI, errorOf(t, rec)["type"])
		})
	}
}

func TestCreateBatchRecordsHowTheRequestArrived(t *testing.T) {
	api := newTestAPI(t)
	api.service.config.Gateway = func(c *gin.Context) (string, string, int) {
		return "default/gw", "http", 8080
	}
	input := mustUpload(t, api, "alice", chatLine)
	req := createRequest(createBody(input))
	req.Host = "api.example.com"
	rec := api.call(t, "alice", req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	job, err := api.store.GetJob(context.Background(), decode(t, rec)["id"].(string))
	require.NoError(t, err)
	assert.Equal(t, Origin{Host: "api.example.com", Gateway: "default/gw", Listener: "http", Port: 8080}, job.Origin)
}

func TestUnknownBatchRoutesAre404(t *testing.T) {
	api := newTestAPI(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodDelete, batchesPath+"/batch_1", nil),
		httptest.NewRequest(http.MethodPost, batchesPath+"/batch_1", nil),
		httptest.NewRequest(http.MethodGet, batchesPath+"/batch_1/results", nil),
		httptest.NewRequest(http.MethodPut, batchesPath, nil),
	} {
		rec := api.call(t, "alice", req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", req.Method, req.URL.Path)
	}
}

func TestCreateBatchOverHTTP2(t *testing.T) {
	api := newTestAPI(t)
	input := mustUpload(t, api, "alice", chatLine)
	server := http2Service(t, api)
	req, err := http.NewRequest(http.MethodPost, server.URL+batchesPath, strings.NewReader(createBody(input)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, 2, resp.ProtoMajor)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(body))
}
