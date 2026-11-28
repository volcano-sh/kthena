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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lineRequest() *Request {
	return &Request{
		JobID:     "batch_1",
		RequestID: "batch_req_1",
		Endpoint:  chatCompletions,
		Tenant:    "tenant-1",
		Subject:   "alice",
		Origin:    Origin{Host: "api.example.com", Gateway: "default/gw", Listener: "http", Port: 80},
		Body:      []byte(`{"model":"m"}`),
	}
}

func TestDispatchSendsTheLineLikeAUserRequest(t *testing.T) {
	var got *http.Request
	var body string
	dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	response := dispatcher.Dispatch(context.Background(), lineRequest())

	require.NoError(t, response.Err)
	assert.Equal(t, http.StatusCreated, response.StatusCode)
	assert.Equal(t, `{"ok":true}`, string(response.Body))
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, chatCompletions, got.URL.Path)
	assert.Equal(t, "api.example.com", got.Host, "an HTTPRoute that lists hostnames sees the host the batch came in on")
	assert.Equal(t, "batch_req_1", got.Header.Get("X-Request-ID"), "the line can be found in the access log")
	assert.Equal(t, "application/json", got.Header.Get("Content-Type"))
	assert.Equal(t, `{"model":"m"}`, body)
	identity, ok := IdentityFromContext(got.Context())
	require.True(t, ok, "the identity travels in the Go context")
	assert.Equal(t, Identity{Tenant: "tenant-1", Subject: "alice", Origin: lineRequest().Origin}, identity)
}

func TestANetworkRequestCarriesNoBatchIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, chatCompletions, nil)
	req.Header.Set("X-Batch-Identity", "alice")
	_, ok := IdentityFromContext(req.Context())
	assert.False(t, ok)
}

func TestDispatchKeepsAtMostTheLimit(t *testing.T) {
	for name, size := range map[string]int{"exactly the limit": defaultMaxResponse, "one byte over": defaultMaxResponse + 1} {
		t.Run(name, func(t *testing.T) {
			dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// Two writes, so the limit holds across writes and not only for one.
				_, _ = io.WriteString(w, strings.Repeat("a", size-1))
				_, _ = io.WriteString(w, "a")
			}))
			response := dispatcher.Dispatch(context.Background(), lineRequest())
			if size == defaultMaxResponse {
				require.NoError(t, response.Err)
				assert.Len(t, response.Body, size)
				return
			}
			assert.ErrorIs(t, response.Err, ErrResponseTooLarge)
			assert.Empty(t, response.Body, "a too large answer is never cut short into the output")
		})
	}
}

func TestDispatchTurnsAPanicIntoTheLinesError(t *testing.T) {
	dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	response := dispatcher.Dispatch(context.Background(), lineRequest())
	require.Error(t, response.Err)
	assert.Contains(t, response.Err.Error(), "boom")
}

func TestDispatchStopsAtTheLineTimeout(t *testing.T) {
	dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	dispatcher.lineTimeout = 20 * time.Millisecond
	response := dispatcher.Dispatch(context.Background(), lineRequest())
	assert.ErrorIs(t, response.Err, context.DeadlineExceeded)
}

func TestDispatchOfAnAnswerWithoutABodyIsOK(t *testing.T) {
	dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	response := dispatcher.Dispatch(context.Background(), lineRequest())
	require.NoError(t, response.Err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
}

func TestTheBatchAPIRefusesABatchLine(t *testing.T) {
	api := newTestAPI(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, filesPath, nil),
		createRequest(createBody("file-1")),
	} {
		req = req.WithContext(WithIdentity(req.Context(), Identity{Tenant: tenantID("", "alice"), Subject: "alice"}))
		rec := api.call(t, "alice", req)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s", req.Method, req.URL.Path)
	}
	assert.False(t, api.mini.Exists(api.store.queueKey()))
}

func TestDispatchOfAnAnswerCutOffByTheTimeoutIsAnError(t *testing.T) {
	dispatcher := NewHandlerDispatcher(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choi`)
		<-r.Context().Done()
	}))
	dispatcher.lineTimeout = 20 * time.Millisecond
	response := dispatcher.Dispatch(context.Background(), lineRequest())
	assert.ErrorIs(t, response.Err, context.DeadlineExceeded, "a 200 that was written before the end is not an answer")
}
