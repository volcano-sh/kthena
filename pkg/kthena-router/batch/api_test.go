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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v3/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type testAPI struct {
	service *Service
	files   *FSStore
	root    string
	store   *RedisStore
	mini    *miniredis.Miniredis
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mini, store := newTestStore(t)
	files, root := newTestFileStore(t)
	service := NewService(store, files, Config{MaxFileBytes: 1024, FileExpiration: 720 * time.Hour})
	return &testAPI{service: service, files: files, root: root, store: store, mini: mini}
}

// call runs one request through the same entry point the router hook uses.
func (a *testAPI) call(t *testing.T, tenant string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	if tenant != "" {
		c.Set(common.UserIdKey, tenant)
	}
	require.True(t, a.service.Handles(req.Method, req.URL.Path), "%s %s must be handled", req.Method, req.URL.Path)
	a.service.Serve(c)
	return rec
}

func uploadRequest(t *testing.T, body string, purposeFirst bool, extra map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writePurpose := func() {
		if purpose, ok := extra["purpose"]; ok {
			require.NoError(t, writer.WriteField("purpose", purpose))
		}
	}
	if purposeFirst {
		writePurpose()
	}
	if body != "" {
		part, err := writer.CreateFormFile("file", extra["filename"])
		require.NoError(t, err)
		_, err = io.WriteString(part, body)
		require.NoError(t, err)
	}
	if !purposeFirst {
		writePurpose()
	}
	for key, value := range extra {
		if key == "purpose" || key == "filename" {
			continue
		}
		require.NoError(t, writer.WriteField(key, value))
	}
	require.NoError(t, writer.Close())
	req := httptest.NewRequest(http.MethodPost, filesPath, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "body: %s", rec.Body.String())
	return out
}

func mustUpload(t *testing.T, api *testAPI, tenant, body string) string {
	t.Helper()
	rec := api.call(t, tenant, uploadRequest(t, body, true,
		map[string]string{"purpose": PurposeBatch, "filename": "questions.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return decode(t, rec)["id"].(string)
}

func TestUploadAndReadBack(t *testing.T) {
	api := newTestAPI(t)
	body := `{"custom_id":"a"}` + "\n"

	rec := api.call(t, "alice", uploadRequest(t, body, true,
		map[string]string{"purpose": PurposeBatch, "filename": "questions.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode(t, rec)
	assert.Equal(t, "file", out["object"])
	assert.Equal(t, float64(len(body)), out["bytes"])
	assert.Equal(t, "questions.jsonl", out["filename"])
	assert.Equal(t, PurposeBatch, out["purpose"])
	assert.Equal(t, FileProcessed, out["status"])
	assert.NotZero(t, out["created_at"])
	assert.NotZero(t, out["expires_at"])
	id := out["id"].(string)
	assert.True(t, strings.HasPrefix(id, "file-"), "id is server generated: %s", id)

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, id, decode(t, rec)["id"])

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, body, rec.Body.String())
}

func TestUploadAcceptsPurposeAfterTheFile(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", uploadRequest(t, "line\n", false,
		map[string]string{"purpose": PurposeBatch, "filename": "q.jsonl"}))
	assert.Equal(t, http.StatusOK, rec.Code,
		"clients may send the fields in any order, so the file streams first")
}

func TestUploadRejectsTheWrongPurpose(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": "assistants", "filename": "q.jsonl"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	out := decode(t, rec)["error"].(map[string]any)
	assert.Equal(t, "purpose", out["param"])
	assert.Contains(t, out["message"], PurposeBatch)

	assert.Empty(t, storedFiles(t, api.root), "the rejected upload leaves no bytes behind")
}

func TestUploadRejectsMissingParts(t *testing.T) {
	api := newTestAPI(t)

	rec := api.call(t, "alice", uploadRequest(t, "", true, map[string]string{"purpose": PurposeBatch}))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a request with no file part is refused")

	plain := httptest.NewRequest(http.MethodPost, filesPath, strings.NewReader("not multipart"))
	plain.Header.Set("Content-Type", "application/json")
	rec = api.call(t, "alice", plain)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// An operator who sets the largest possible limit means "no limit", not "refuse everything".
func TestTheLargestUploadLimitStillAcceptsFiles(t *testing.T) {
	api := newTestAPI(t)
	api.service = NewService(api.store, api.files, Config{MaxFileBytes: math.MaxInt64})
	rec := api.call(t, "alice", uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestUploadRefusesOversizedFiles(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", uploadRequest(t, strings.Repeat("x", 2048), true,
		map[string]string{"purpose": PurposeBatch, "filename": "big.jsonl"}))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Empty(t, storedFiles(t, api.root), "an oversized upload leaves no bytes behind")
}

func TestFilenameCannotSteerTheFilesystem(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": PurposeBatch, "filename": "../../etc/passwd"}))
	require.Equal(t, http.StatusOK, rec.Code)

	for _, path := range storedFiles(t, api.root) {
		assert.True(t, strings.HasPrefix(filepath.Base(path), "file-"),
			"stored name comes from our id, not the client: %s", path)
	}
	_, err := os.Stat(filepath.Join(filepath.Dir(api.root), "etc", "passwd"))
	assert.Error(t, err, "nothing is written outside the root")
}

func TestFilesBelongToOneTenant(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")

	for _, path := range []string{filesPath + "/" + id, filesPath + "/" + id + "/content"} {
		rec := api.call(t, "bob", httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "another tenant gets not found for %s", path)
	}
	rec := api.call(t, "bob", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "another tenant cannot delete it")

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the owner is unaffected")
}

func TestDeleteRemovesTheBytes(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	require.NotEmpty(t, storedFiles(t, api.root))

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	out := decode(t, rec)
	assert.Equal(t, true, out["deleted"])
	assert.Equal(t, id, out["id"])
	assert.Empty(t, storedFiles(t, api.root), "the bytes are gone, not just the record")

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a deleted file is gone from the API")
	rec = api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDeleteKeepsBytesABatchStillNeeds(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	ctx := context.Background()
	require.NoError(t, api.store.client.SAdd(ctx, api.store.fileRefsKey(id), "batch_1").Err())

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, storedFiles(t, api.root),
		"a running batch still reads this file, so only the record is hidden")
}

func TestMissingFileAnswersInTheOpenAIShape(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/file-nope", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	out := decode(t, rec)
	errBody, ok := out["error"].(map[string]any)
	require.True(t, ok, "errors use the OpenAI envelope, not the router's bare strings: %s", rec.Body)
	assert.Equal(t, "invalid_request_error", errBody["type"])
	assert.Contains(t, errBody["message"], "file-nope")
}

func TestHandlesOnlyTheBatchRoutes(t *testing.T) {
	api := newTestAPI(t)
	assert.True(t, api.service.Handles(http.MethodPost, filesPath))
	assert.True(t, api.service.Handles(http.MethodGet, filesPath+"/file-1/content"))
	assert.False(t, api.service.Handles(http.MethodPost, "/v1/chat/completions"))
	assert.False(t, api.service.Handles(http.MethodGet, "/v1/models"))
	assert.False(t, api.service.Handles(http.MethodGet, "/v1/filesystem"))
	assert.True(t, api.service.Handles(http.MethodPost, batchesPath))
	assert.True(t, api.service.Handles(http.MethodGet, batchesPath+"/batch_1"))
	assert.False(t, api.service.Handles(http.MethodGet, "/v1/batchesx"))

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodPut, filesPath+"/file-1", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "an unknown method on a known path is refused")
}

func storedFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// The reserved directory holds the probe and the volume ID, not uploaded bytes.
		if info.IsDir() && info.Name() == reservedDir {
			return filepath.SkipDir
		}
		if !info.IsDir() {
			found = append(found, path)
		}
		return nil
	}))
	return found
}

func TestUploadDoesNotBufferTheFileInMemory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	_, store := newTestStore(t)
	files, root := newTestFileStore(t)
	service := NewService(store, files, Config{MaxFileBytes: 64 << 20})

	const size = 8 << 20
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	go func() {
		defer writer.Close()
		_ = form.WriteField("purpose", PurposeBatch)
		part, err := form.CreateFormFile("file", "big.jsonl")
		if err != nil {
			return
		}
		_, _ = io.CopyN(part, zeroLines{}, size)
		_ = form.Close()
	}()

	req := httptest.NewRequest(http.MethodPost, filesPath, reader)
	req.Header.Set("Content-Type", form.FormDataContentType())
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(common.UserIdKey, "alice")

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	service.Serve(c)
	runtime.ReadMemStats(&after)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, float64(size), decode(t, rec)["bytes"], "the whole file was stored")

	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(size/4),
		"an %d byte upload allocated %d bytes; it must stream, not buffer", size, allocated)

	stored := storedFiles(t, root)
	require.Len(t, stored, 1)
	info, err := os.Stat(stored[0])
	require.NoError(t, err)
	assert.Equal(t, int64(size), info.Size())
}

type zeroLines struct{}

func (zeroLines) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
		if i%64 == 63 {
			p[i] = '\n'
		}
	}
	return len(p), nil
}

func TestFileContentServesRanges(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "0123456789")

	req := httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil)
	req.Header.Set("Range", "bytes=2-5")
	rec := api.call(t, "alice", req)
	assert.Equal(t, http.StatusPartialContent, rec.Code, "large result files are fetched in ranges")
	assert.Equal(t, "2345", rec.Body.String())
	assert.Equal(t, "bytes 2-5/10", rec.Header().Get("Content-Range"))

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil))
	assert.Equal(t, "application/jsonl", rec.Header().Get("Content-Type"))
}

func TestUploadAcceptsAnEmptyFile(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", uploadRequest(t, "", true, map[string]string{
		"purpose": PurposeBatch, "filename": "empty.jsonl", "empty": "1",
	}))
	require.Equal(t, http.StatusBadRequest, rec.Code, "a request with no file part is still refused")

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("purpose", PurposeBatch))
	_, err := writer.CreateFormFile("file", "empty.jsonl")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	req := httptest.NewRequest(http.MethodPost, filesPath, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec = api.call(t, "alice", req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, float64(0), decode(t, rec)["bytes"],
		"an empty file uploads fine; the batch that uses it fails validation later")
}

func TestUnknownSubpathsAreRefused(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")

	for _, path := range []string{filesPath + "/" + id + "/content/extra", filesPath + "/" + id + "/unknown"} {
		rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s must not be served", path)
	}

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "a trailing slash still means the collection")
}

func TestDeleteWorksWhenTheBytesAreAlreadyGone(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	require.NoError(t, api.files.Remove(context.Background(), tenantID("", "alice"), id))

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code, "a half-deleted file must still delete cleanly")
	assert.Equal(t, true, decode(t, rec)["deleted"])
}

func TestWithAuthDisabledEveryoneIsOneTenant(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": PurposeBatch, "filename": "q.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := decode(t, rec)["id"].(string)

	rec = api.call(t, "", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code, "without JWT auth there is a single tenant")

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a named tenant still cannot read the anonymous one")
}

// callAs runs a request as a caller whose token carries this issuer and subject.
func (a *testAPI) callAs(t *testing.T, issuer, subject string, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	token, err := jwt.NewBuilder().Issuer(issuer).Subject(subject).Build()
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(common.UserIdKey, subject)
	c.Set(common.JWTTokenKey, token)
	a.service.Serve(c)
	return rec
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body, ok := decode(t, rec)["error"].(map[string]any)
	require.True(t, ok, "body: %s", rec.Body.String())
	return body
}

func TestErrorBodyAlwaysHasTheOpenAIFields(t *testing.T) {
	api := newTestAPI(t)
	rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/file-missing", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	body := errorOf(t, rec)
	assert.Equal(t, "invalid_request_error", body["type"])
	assert.Equal(t, "id", body["param"])
	assert.Contains(t, body, "code")
	assert.Nil(t, body["code"])

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodPatch, filesPath, nil))
	body = errorOf(t, rec)
	assert.Contains(t, body, "param")
	assert.Nil(t, body["param"], "an error with no parameter still carries the field, as null")
}

func TestTwoIssuersWithOneSubjectAreTwoTenants(t *testing.T) {
	api := newTestAPI(t)
	rec := api.callAs(t, "https://issuer-a", "user-1", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": PurposeBatch, "filename": "a.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := decode(t, rec)["id"].(string)

	rec = api.callAs(t, "https://issuer-a", "user-1", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code, "the owner reads its file")
	for _, path := range []string{"/" + id, "/" + id + "/content"} {
		rec = api.callAs(t, "https://issuer-b", "user-1", httptest.NewRequest(http.MethodGet, filesPath+path, nil))
		assert.Equal(t, http.StatusNotFound, rec.Code, "the same subject from another issuer must not see %s", path)
	}
	rec = api.callAs(t, "https://issuer-b", "user-1", httptest.NewRequest(http.MethodGet, filesPath, nil))
	assert.Empty(t, decode(t, rec)["data"])
	assert.NotEqual(t, tenantID("https://issuer-a", "user-1"), tenantID("https://issuer-b", "user-1"))
	assert.NotEqual(t, tenantID("a", "bc"), tenantID("ab", "c"), "the separator keeps issuer and subject apart")
}

// countedCreates counts how often the API asks the volume to store a file.
type countedCreates struct {
	FileStore
	creates *atomic.Int32
}

func (v countedCreates) Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error) {
	v.creates.Add(1)
	return v.FileStore.Create(ctx, tenant, id, r, limit)
}

func TestStoreDownIsServiceUnavailableNotNotFound(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	api.mini.Close()
	creates := &atomic.Int32{}
	api.service.files = countedCreates{FileStore: api.files, creates: creates}

	tests := []struct {
		name string
		req  *http.Request
	}{
		{name: "get", req: httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil)},
		{name: "content", req: httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil)},
		{name: "delete", req: httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil)},
		{name: "list", req: httptest.NewRequest(http.MethodGet, filesPath, nil)},
		{name: "upload", req: uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch, "filename": "b.jsonl"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.call(t, "alice", tt.req)
			assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			assert.Equal(t, "api_error", errorOf(t, rec)["type"])
		})
	}
	assert.Len(t, storedFiles(t, api.root), 1, "an upload whose record cannot be written leaves no bytes behind")
	assert.Equal(t, int32(0), creates.Load(), "the upload is refused before any byte reaches the volume")
}

func TestUploadRules(t *testing.T) {
	long := strings.Repeat("n", maxFilename+1)
	tests := []struct {
		name      string
		fields    map[string]string
		wantCode  int
		wantParam string
	}{
		{name: "no expiry uses the default", fields: map[string]string{}, wantCode: http.StatusOK},
		{name: "anchor and seconds", fields: map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "7200"}, wantCode: http.StatusOK},
		{name: "seconds that are not a number", fields: map[string]string{"expires_after[seconds]": "soon"}, wantCode: http.StatusBadRequest, wantParam: "expires_after"},
		{name: "an anchor without seconds", fields: map[string]string{"expires_after[anchor]": "created_at"}, wantCode: http.StatusBadRequest, wantParam: "expires_after"},
		{name: "an unknown anchor", fields: map[string]string{"expires_after[anchor]": "last_used", "expires_after[seconds]": "7200"}, wantCode: http.StatusBadRequest, wantParam: "expires_after"},
		{name: "the shortest expiry", fields: map[string]string{"expires_after[seconds]": "3600"}, wantCode: http.StatusOK},
		{name: "the longest expiry", fields: map[string]string{"expires_after[seconds]": "2592000"}, wantCode: http.StatusOK},
		{name: "one second under the shortest expiry", fields: map[string]string{"expires_after[seconds]": "3599"}, wantCode: http.StatusBadRequest, wantParam: "expires_after"},
		{name: "one second over the longest expiry", fields: map[string]string{"expires_after[seconds]": "2592001"}, wantCode: http.StatusBadRequest, wantParam: "expires_after"},
		{name: "a filename at the limit", fields: map[string]string{"filename": long[:maxFilename]}, wantCode: http.StatusOK},
		{name: "a filename that is too long", fields: map[string]string{"filename": long}, wantCode: http.StatusBadRequest, wantParam: "file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			fields := map[string]string{"purpose": PurposeBatch, "filename": "in.jsonl"}
			for k, v := range tt.fields {
				fields[k] = v
			}
			rec := api.call(t, "alice", uploadRequest(t, "line\n", true, fields))
			require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			if tt.wantCode != http.StatusOK {
				assert.Equal(t, tt.wantParam, errorOf(t, rec)["param"])
				assert.Empty(t, storedFiles(t, api.root), "a refused upload leaves no bytes behind")
				return
			}
			body := decode(t, rec)
			created := int64(body["created_at"].(float64))
			want := created + int64((720 * time.Hour).Seconds())
			if seconds := tt.fields["expires_after[seconds]"]; seconds != "" {
				ttl, err := strconv.ParseInt(seconds, 10, 64)
				require.NoError(t, err)
				want = created + ttl
			}
			assert.EqualValues(t, want, body["expires_at"])
		})
	}
}

func TestUploadRefusesAFieldOverTheLimit(t *testing.T) {
	padded := func(value string, size int) string { return value + strings.Repeat(" ", size-len(value)) }
	tests := []struct {
		name     string
		fields   map[string]string
		wantCode int
	}{
		{name: "a padded purpose that is not batch", fields: map[string]string{"purpose": padded(PurposeBatch, maxFormValue) + "x"}, wantCode: http.StatusBadRequest},
		{name: "padded seconds after the file", fields: map[string]string{"purpose": PurposeBatch, "expires_after[seconds]": padded("7200", maxFormValue) + "1"}, wantCode: http.StatusBadRequest},
		{name: "a value at the limit", fields: map[string]string{"purpose": padded(PurposeBatch, maxFormValue)}, wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			rec := api.call(t, "alice", uploadRequest(t, "line\n", true, tt.fields))
			require.Equal(t, tt.wantCode, rec.Code, "a cut value must not pass a check the whole value fails: %s", rec.Body.String())
			if tt.wantCode != http.StatusOK {
				assert.Equal(t, errInvalidRequest, errorOf(t, rec)["type"])
				assert.Empty(t, storedFiles(t, api.root), "a refused upload leaves no bytes behind")
			}
		})
	}
}

func TestUploadRejectsASecondFile(t *testing.T) {
	api := newTestAPI(t)
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("purpose", PurposeBatch))
	for _, name := range []string{"one.jsonl", "two.jsonl"} {
		part, err := writer.CreateFormFile("file", name)
		require.NoError(t, err)
		_, err = io.WriteString(part, "line\n")
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	req := httptest.NewRequest(http.MethodPost, filesPath, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rec := api.call(t, "alice", req)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, storedFiles(t, api.root), "the first file is removed when the upload is refused")
}

func TestFileWithoutExpiryReportsNull(t *testing.T) {
	api := newTestAPI(t)
	api.service.config.FileExpiration = 0
	rec := api.call(t, "alice", uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch, "filename": "in.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code)
	body := decode(t, rec)
	assert.Contains(t, body, "expires_at")
	assert.Nil(t, body["expires_at"])
}

// A file is gone at the expires_at the API reported for it, even before its bytes are cleaned up.
func TestAFileIsGoneAtItsExpiry(t *testing.T) {
	api := newTestAPI(t)
	created := time.Unix(1800000000, 0)
	at := func(now time.Time) { api.service.now = func() time.Time { return now } }
	at(created)
	rec := api.call(t, "alice", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": PurposeBatch, "expires_after[anchor]": "created_at", "expires_after[seconds]": "3600"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := decode(t, rec)["id"].(string)
	routes := func() map[string]*http.Request {
		return map[string]*http.Request{
			"get":     httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil),
			"content": httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil),
		}
	}
	listed := func() []any {
		return decode(t, api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath, nil)))["data"].([]any)
	}

	at(created.Add(3599 * time.Second))
	for name, req := range routes() {
		assert.Equal(t, http.StatusOK, api.call(t, "alice", req).Code, "%s a second before expiry", name)
	}
	assert.Len(t, listed(), 1, "listed a second before expiry")

	at(created.Add(3600 * time.Second))
	for name, req := range routes() {
		assert.Equal(t, http.StatusNotFound, api.call(t, "alice", req).Code, "%s at expiry", name)
	}
	assert.Empty(t, listed(), "not listed at expiry")
	deleted := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, deleted.Code, "an expired file cannot be deleted either")
}

func TestListFilesParameters(t *testing.T) {
	api := newTestAPI(t)
	var ids []string
	clock := time.Unix(1700000000, 0)
	api.service.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i < 3; i++ {
		ids = append(ids, mustUpload(t, api, "alice", "line\n"))
	}
	list := func(query string) *httptest.ResponseRecorder {
		return api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+query, nil))
	}
	listed := func(rec *httptest.ResponseRecorder) []string {
		var got []string
		for _, item := range decode(t, rec)["data"].([]any) {
			got = append(got, item.(map[string]any)["id"].(string))
		}
		return got
	}

	assert.Equal(t, []string{ids[2], ids[1], ids[0]}, listed(list("")), "newest first by default")
	assert.Equal(t, ids, listed(list("?order=asc")))
	assert.Equal(t, ids, listed(list("?order=asc&limit=10000")), "a limit above the page size is clamped, not refused")

	rec := list("?limit=2")
	body := decode(t, rec)
	assert.Equal(t, true, body["has_more"])
	assert.Equal(t, ids[2], body["first_id"])
	assert.Equal(t, ids[1], body["last_id"])
	assert.Equal(t, []string{ids[0]}, listed(list("?limit=2&after="+ids[1])))

	for query, param := range map[string]string{"?limit=0": "limit", "?limit=x": "limit", "?order=up": "order", "?after=file-unknown": "after"} {
		rec := list(query)
		assert.Equal(t, http.StatusBadRequest, rec.Code, query)
		assert.Equal(t, param, errorOf(t, rec)["param"], query)
	}

	rec = api.call(t, "bob", httptest.NewRequest(http.MethodGet, filesPath, nil))
	body = decode(t, rec)
	assert.Empty(t, body["data"])
	assert.Nil(t, body["first_id"])
	assert.Equal(t, false, body["has_more"])
}

func TestContentOfAFileWhoseBytesAreGone(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	require.NoError(t, api.files.Remove(context.Background(), tenantID("", "alice"), id))
	rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// stuckFiles is a volume that refuses to remove anything.
type stuckFiles struct{ FileStore }

func (stuckFiles) Remove(context.Context, string, string) error {
	return errors.New("volume is read-only")
}

func TestDeleteSucceedsWhenTheBytesCannotBeRemoved(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	api.service.files = stuckFiles{api.files}

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, rec.Code, "the record is already deleted, so a 503 would send the client into a 404 on retry")
	assert.Equal(t, true, decode(t, rec)["deleted"])

	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// All verified tokens without a subject would share one tenant, so they are refused instead.
func TestTokenWithoutASubjectIsRefused(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	for name, req := range map[string]*http.Request{
		"upload": uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}),
		"list":   httptest.NewRequest(http.MethodGet, filesPath, nil),
		"get":    httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil),
	} {
		rec := api.callAs(t, "https://issuer-a", "", req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Equal(t, "invalid_request_error", errorOf(t, rec)["type"], name)
	}
	assert.Len(t, storedFiles(t, api.root), 1, "the refused upload stored nothing")
}

// serveHanging sends the start of a form and then nothing, like a client that stopped sending.
func serveHanging(t *testing.T, api *testAPI, write func(*multipart.Writer)) *httptest.ResponseRecorder {
	t.Helper()
	body, sender := io.Pipe()
	t.Cleanup(func() { _ = body.Close() })
	writer := multipart.NewWriter(sender)
	go write(writer)
	req := httptest.NewRequest(http.MethodPost, filesPath, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _ := gin.CreateTestContext(rec)
		c.Request = req
		c.Set(common.UserIdKey, "alice")
		api.service.Serve(c)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler waits for the rest of a part it already rejected")
	}
	return rec
}

func TestUploadAnswersWithoutReadingTheRestOfARejectedPart(t *testing.T) {
	tests := []struct {
		name   string
		write  func(*multipart.Writer)
		status int
	}{
		{name: "oversized file", status: http.StatusRequestEntityTooLarge, write: func(w *multipart.Writer) {
			part, _ := w.CreateFormFile("file", "big.jsonl")
			_, _ = part.Write(bytes.Repeat([]byte("x"), 4096))
		}},
		{name: "second file", status: http.StatusBadRequest, write: func(w *multipart.Writer) {
			_ = w.WriteField("purpose", PurposeBatch)
			part, _ := w.CreateFormFile("file", "a.jsonl")
			_, _ = part.Write([]byte("line\n"))
			part, _ = w.CreateFormFile("file", "b.jsonl")
			_, _ = part.Write(bytes.Repeat([]byte("x"), 4096))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			rec := serveHanging(t, api, tt.write)
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Empty(t, storedFiles(t, api.root))
		})
	}
}

// A real HTTP/1.1 server reads an unread body before it answers, unless the connection is closed instead.
func TestRefusedUploadDoesNotWaitForTheRestOfTheBody(t *testing.T) {
	api := newTestAPI(t)
	engine := gin.New()
	engine.Any("/v1/*path", func(c *gin.Context) {
		c.Set(common.UserIdKey, "alice")
		api.service.Serve(c)
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	part := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"big.jsonl\"\r\n\r\n" + strings.Repeat("x", 4096)
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kthena\r\nContent-Type: multipart/form-data; boundary=b\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", filesPath, len(part), part)
	require.NoError(t, err)
	// The client sends nothing more and waits for the answer.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the 413 must not wait for the rest of the body")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	assert.True(t, resp.Close, "the connection is closed instead of read to the end")
}

// dialService serves the API on a real HTTP/1.1 server and returns a raw connection to it.
func dialService(t *testing.T, api *testAPI) net.Conn {
	t.Helper()
	engine := gin.New()
	engine.Any("/v1/*path", func(c *gin.Context) {
		c.Set(common.UserIdKey, "alice")
		api.service.Serve(c)
	})
	server := httptest.NewServer(engine)
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// readStatus reads the response on conn and fails the test if none comes within wait.
func readStatus(t *testing.T, conn net.Conn, wait time.Duration) int {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(wait)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err, "the upload must be answered, not held open")
	_ = resp.Body.Close()
	return resp.StatusCode
}

// uploadForm is a whole multipart upload of content with purpose batch.
func uploadForm(t *testing.T, content string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	require.NoError(t, form.WriteField("purpose", PurposeBatch))
	part, err := form.CreateFormFile("file", "a.jsonl")
	require.NoError(t, err)
	_, err = io.WriteString(part, content)
	require.NoError(t, err)
	require.NoError(t, form.Close())
	return form.FormDataContentType(), body.Bytes()
}

// A client that starts an upload and then sends nothing must not hold a handler, a file and a connection forever.
func TestAStalledUploadIsCutOff(t *testing.T) {
	api := newTestAPI(t)
	api.service.uploadIdle = 200 * time.Millisecond
	conn := dialService(t, api)
	start := "--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.jsonl\"\r\n\r\nline\n"
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kthena\r\nContent-Type: multipart/form-data; boundary=b\r\nContent-Length: 100000\r\n\r\n%s", filesPath, start)
	require.NoError(t, err)

	assert.Equal(t, http.StatusRequestTimeout, readStatus(t, conn, 3*time.Second))
	assert.Empty(t, storedFiles(t, api.root), "the stalled upload leaves no bytes, not even its temporary file")
}

// The limit is on silence, not on the whole upload: pauses shorter than it never add up to a cut-off.
func TestASlowUploadThatKeepsSendingIsNotCutOff(t *testing.T) {
	api := newTestAPI(t)
	api.service.uploadIdle = 400 * time.Millisecond
	conn := dialService(t, api)
	contentType, body := uploadForm(t, strings.Repeat("line\n", 120))
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kthena\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", filesPath, contentType, len(body))
	require.NoError(t, err)
	// Eight pieces 100 ms apart take twice the limit, with every pause a quarter of it.
	for piece := 0; piece < 8; piece++ {
		from, to := len(body)*piece/8, len(body)*(piece+1)/8
		_, err = conn.Write(body[from:to])
		require.NoError(t, err)
		time.Sleep(100 * time.Millisecond)
	}

	assert.Equal(t, http.StatusOK, readStatus(t, conn, 3*time.Second))
}

// slowStore takes its time to store a record, like a Redis under load.
type slowStore struct {
	Store
	delay time.Duration
}

func (s slowStore) CreateFile(ctx context.Context, file *File) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Store.CreateFile(ctx, file)
}

// Once the body has arrived the limit no longer applies, however long the record takes.
func TestAnUploadThatIsSlowToRecordIsNotCutOff(t *testing.T) {
	api := newTestAPI(t)
	api.service.uploadIdle = 200 * time.Millisecond
	api.service.store = slowStore{Store: api.store, delay: 600 * time.Millisecond}
	conn := dialService(t, api)
	contentType, body := uploadForm(t, "line\n")
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: kthena\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", filesPath, contentType, len(body), body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, readStatus(t, conn, 3*time.Second))
}

// pausingVolume stops reading after the first chunk for a while, like a volume that hangs during a write.
type pausingVolume struct {
	FileStore
	pause time.Duration
}

func (v pausingVolume) Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error) {
	return v.FileStore.Create(ctx, tenant, id, &pausingReader{Reader: r, pause: v.pause}, limit)
}

type pausingReader struct {
	io.Reader
	pause  time.Duration
	paused bool
}

func (p *pausingReader) Read(b []byte) (int, error) {
	n, err := p.Reader.Read(b)
	if !p.paused {
		p.paused = true
		time.Sleep(p.pause)
	}
	return n, err
}

// http2Service serves the API over TLS with HTTP/2 on, as any TLS client of the router can negotiate.
func http2Service(t *testing.T, api *testAPI) *httptest.Server {
	t.Helper()
	engine := gin.New()
	engine.Any("/v1/*path", func(c *gin.Context) {
		c.Set(common.UserIdKey, "alice")
		api.service.Serve(c)
	})
	server := httptest.NewUnstartedServer(engine)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// Over HTTP/2 too, an upload that stops sending is cut off and leaves nothing behind.
func TestAStalledUploadOverHTTP2IsCutOff(t *testing.T) {
	api := newTestAPI(t)
	api.service.uploadIdle = 200 * time.Millisecond
	server := http2Service(t, api)
	body, sender := io.Pipe()
	t.Cleanup(func() { _ = sender.Close() })
	form := multipart.NewWriter(sender)
	// The start of a form, and then nothing more.
	go func() {
		_ = form.WriteField("purpose", PurposeBatch)
		part, _ := form.CreateFormFile("file", "a.jsonl")
		_, _ = io.WriteString(part, "line\n")
	}()
	req, err := http.NewRequest(http.MethodPost, server.URL+filesPath, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	client := server.Client()
	client.Timeout = 5 * time.Second

	resp, err := client.Do(req)
	require.NoError(t, err, "the stalled upload must be answered")
	defer resp.Body.Close()
	require.Equal(t, "HTTP/2.0", resp.Proto)
	assert.Equal(t, http.StatusRequestTimeout, resp.StatusCode)
	assert.Empty(t, storedFiles(t, api.root), "the stalled upload leaves no bytes, not even its temporary file")
}

// Over HTTP/2 the read deadline is a timer, so time the server spends between reads must not count as the client going quiet.
func TestAnUploadOverHTTP2IsNotCutOffWhileTheVolumeIsSlow(t *testing.T) {
	api := newTestAPI(t)
	api.service.config.MaxFileBytes = 8 << 20
	api.service.uploadIdle = 200 * time.Millisecond
	api.service.files = pausingVolume{FileStore: api.files, pause: 600 * time.Millisecond}
	server := http2Service(t, api)
	// More than the flow-control window, so the client is still sending while the volume pauses.
	contentType, body := uploadForm(t, strings.Repeat("x", 4<<20))

	resp, err := server.Client().Post(server.URL+filesPath, contentType, bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "HTTP/2.0", resp.Proto)
	answer, _ := io.ReadAll(resp.Body)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(answer))
}

// A body that breaks the HTTP framing is the client's fault, so it is a 400 and not blamed on the volume.
func TestABrokenChunkedBodyIsTheClientsFault(t *testing.T) {
	api := newTestAPI(t)
	conn := dialService(t, api)
	contentType, form := uploadForm(t, "line\n")
	// The form is cut inside the file part and followed by a chunk size that is not a number.
	cut := bytes.Index(form, []byte("line\n")) + 2
	_, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: test\r\nContent-Type: %s\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\nzz\r\n", filesPath, contentType, cut, form[:cut])
	require.NoError(t, err)

	assert.Equal(t, http.StatusBadRequest, readStatus(t, conn, 3*time.Second))
	assert.Empty(t, storedFiles(t, api.root))
}

// A refused upload with its body unread must end with a half-close, so a client still sending reads the answer, not a reset.
func TestARefusedUploadClosesWithoutAReset(t *testing.T) {
	api := newTestAPI(t)
	mustUpload(t, api, "alice", "line\n")
	api.mini.Close()
	conn := dialService(t, api)
	// The early store check refuses the upload with almost all of this body unread.
	contentType, form := uploadForm(t, strings.Repeat("x", 900<<10))
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: test\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", filesPath, contentType, len(form))
	go func() { _, _ = conn.Write(append([]byte(head), form...)) }()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	_, err = reader.ReadByte()
	assert.ErrorIs(t, err, io.EOF, "net/http half-closes a connection whose body it stopped reading")
}

func TestOnlyARefusedPostClosesTheConnection(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	tests := []struct {
		name   string
		req    *http.Request
		closes bool
	}{
		{name: "refused upload", req: uploadRequest(t, "line\n", true, map[string]string{"purpose": "assistants"}), closes: true},
		{name: "unknown POST route", req: httptest.NewRequest(http.MethodPost, filesPath+"/"+id+"/content", strings.NewReader("x")), closes: true},
		{name: "stored upload", req: uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}), closes: false},
		{name: "missing file", req: httptest.NewRequest(http.MethodGet, filesPath+"/file-missing", nil), closes: false},
		{name: "list", req: httptest.NewRequest(http.MethodGet, filesPath, nil), closes: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.call(t, "alice", tt.req)
			assert.Equal(t, tt.closes, rec.Header().Get("Connection") == "close", "%d %s", rec.Code, rec.Body.String())
		})
	}
}

func TestUploadRejectsAFormThatWasCutOff(t *testing.T) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.WriteField("purpose", PurposeBatch))
	part, err := writer.CreateFormFile("file", "questions.jsonl")
	require.NoError(t, err)
	_, err = io.WriteString(part, "line\n")
	require.NoError(t, err)
	require.NoError(t, writer.WriteField("expires_after[seconds]", "7200"))
	require.NoError(t, writer.Close())
	closing := "\r\n--" + writer.Boundary() + "--\r\n"

	tests := []struct {
		name string
		keep int
	}{
		{name: "inside the file", keep: bytes.Index(buf.Bytes(), []byte("line\n")) + 2},
		{name: "inside the last field", keep: buf.Len() - len(closing) - 2},
		{name: "before the closing boundary", keep: buf.Len() - len(closing)},
		{name: "inside the closing boundary", keep: buf.Len() - 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			req := httptest.NewRequest(http.MethodPost, filesPath, bytes.NewReader(buf.Bytes()[:tt.keep]))
			req.Header.Set("Content-Type", writer.FormDataContentType())
			rec := api.call(t, "alice", req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "half a form must not become a file: %s", rec.Body.String())
			assert.Empty(t, storedFiles(t, api.root))
		})
	}
}

// brokenStore fails every record write, stored or not, and can go silent after it or refuse it outright.
type brokenStore struct {
	Store
	stores  bool
	answers bool
	refuses bool
	wrote   bool
}

func (s *brokenStore) CreateFile(ctx context.Context, file *File) error {
	s.wrote = true
	if s.refuses {
		return fmt.Errorf("%w: OOM command not allowed", ErrRefused)
	}
	if s.stores {
		if err := s.Store.CreateFile(ctx, file); err != nil {
			return err
		}
	}
	return errors.New("i/o timeout")
}

func (s *brokenStore) GetFile(ctx context.Context, id string) (*File, error) {
	if s.wrote && !s.answers {
		return nil, errors.New("i/o timeout")
	}
	return s.Store.GetFile(ctx, id)
}

func TestUploadWhenTheStoreFailsToAnswer(t *testing.T) {
	tests := []struct {
		name    string
		stores  bool
		answers bool
		refuses bool
		status  int
		kept    int
	}{
		{name: "the record was stored and only the reply was lost", stores: true, answers: true, status: http.StatusOK, kept: 1},
		{name: "a write without a reply may still land after the lookup, so the bytes stay", answers: true, status: http.StatusServiceUnavailable, kept: 1},
		{name: "the store went silent, so the bytes stay", stores: true, status: http.StatusServiceUnavailable, kept: 1},
		{name: "the store refused the write, so the bytes go", refuses: true, answers: true, status: http.StatusServiceUnavailable, kept: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.service.store = &brokenStore{Store: api.store, stores: tt.stores, answers: tt.answers, refuses: tt.refuses}
			rec := api.call(t, "alice", uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}))
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Len(t, storedFiles(t, api.root), tt.kept, "bytes go only when no record can point at them")
		})
	}
}

func TestFileContentErrorsHaveTheOpenAIShape(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
		status int
	}{
		{name: "range outside the file", header: "Range", value: "bytes=50-60", status: http.StatusRequestedRangeNotSatisfiable},
		{name: "failed precondition", header: "If-Match", value: `"other"`, status: http.StatusPreconditionFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			id := mustUpload(t, api, "alice", "0123456789")
			req := httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil)
			req.Header.Set(tt.header, tt.value)
			rec := api.call(t, "alice", req)
			assert.Equal(t, tt.status, rec.Code)
			assert.Equal(t, errInvalidRequest, errorOf(t, rec)["type"])
		})
	}
}

func TestListAndDeleteLoopReachesEveryFile(t *testing.T) {
	api := newTestAPI(t)
	for i := 0; i < 5; i++ {
		mustUpload(t, api, "alice", "line\n")
	}
	deleted, after := 0, ""
	for more, pages := true, 0; more; pages++ {
		require.Less(t, pages, 10, "the listing never says it is done")
		rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"?limit=2"+after, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		page := decode(t, rec)
		for _, item := range page["data"].([]any) {
			id := item.(map[string]any)["id"].(string)
			rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			deleted++
			after = "&after=" + id
		}
		more = page["has_more"].(bool)
	}
	assert.Equal(t, 5, deleted, "paging continues from a file the loop just deleted")
	assert.Empty(t, storedFiles(t, api.root))
}

func TestIssuerIsComparedExactly(t *testing.T) {
	api := newTestAPI(t)
	rec := api.callAs(t, " https://issuer ", "user-1", uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := decode(t, rec)["id"].(string)

	rec = api.callAs(t, "https://issuer", "user-1", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "an issuer that differs only by spaces is another issuer")
	assert.NotEqual(t, tenantID("a\x00b", "c"), tenantID("a", "b\x00c"), "no two pairs share one tenant")
}

// unseekable is a stored file whose volume fails once it is open.
type unseekable struct{ ReadFile }

func (unseekable) Seek(int64, int) (int64, error) { return 0, errors.New("stale file handle") }

// failingVolume opens files that cannot be read.
type failingVolume struct{ FileStore }

func (s failingVolume) Open(ctx context.Context, tenant, id string) (ReadFile, error) {
	file, err := s.FileStore.Open(ctx, tenant, id)
	if err != nil {
		return nil, err
	}
	return unseekable{file}, nil
}

func TestFileContentErrorsAreJSON(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "0123456789")

	req := httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil)
	req.Header.Set("Range", "bytes=50-60")
	rec := api.call(t, "alice", req)
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json", "the body is JSON, so the header must say so")

	api.service.files = failingVolume{api.files}
	rec = api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a volume failure is not the caller's fault")
	assert.Equal(t, errAPI, errorOf(t, rec)["type"])
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

// unreadBody fails the test when anything reads it.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body was read")
	return 0, io.EOF
}

func TestUploadRefusesADeclaredOversizeBeforeReadingIt(t *testing.T) {
	api := newTestAPI(t)
	req := httptest.NewRequest(http.MethodPost, filesPath, unreadBody{t})
	req.ContentLength = 1024 + multipartSlack + 1
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")

	rec := api.call(t, "alice", req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
}

// leavingClient cancels the request once the file is stored and then honours that, like a remote store would.
type leavingClient struct {
	FileStore
	leave context.CancelFunc
}

func (s leavingClient) Create(ctx context.Context, tenant, id string, r io.Reader, limit int64) (int64, error) {
	written, err := s.FileStore.Create(ctx, tenant, id, r, limit)
	s.leave()
	return written, err
}

func (s leavingClient) Remove(ctx context.Context, tenant, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.FileStore.Remove(ctx, tenant, id)
}

func TestUploadCleansUpAfterAClientThatLeft(t *testing.T) {
	tests := []struct {
		name    string
		purpose string
	}{
		{name: "the form is then rejected", purpose: "assistants"},
		{name: "the record is then not stored", purpose: PurposeBatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			ctx, leave := context.WithCancel(context.Background())
			api.service.files = leavingClient{FileStore: api.files, leave: leave}
			req := uploadRequest(t, "line\n", false, map[string]string{"purpose": tt.purpose})

			rec := api.call(t, "alice", req.WithContext(ctx))
			require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, storedFiles(t, api.root), "the bytes go even though the request is already cancelled")
		})
	}
}

// leavingStore cancels the request right after the record is marked deleted.
type leavingStore struct {
	Store
	leave context.CancelFunc
}

func (s leavingStore) DeleteFile(ctx context.Context, id string) (bool, error) {
	removable, err := s.Store.DeleteFile(ctx, id)
	s.leave()
	return removable, err
}

func TestDeleteRemovesTheBytesAfterAClientThatLeft(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	ctx, leave := context.WithCancel(context.Background())
	api.service.store = leavingStore{Store: api.store, leave: leave}
	api.service.files = leavingClient{FileStore: api.files, leave: leave}

	req := httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil).WithContext(ctx)
	rec := api.call(t, "alice", req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, storedFiles(t, api.root), "the record is deleted, so the bytes go whether or not the client waits")
}

func TestUploadRefusesABodyOverTheCap(t *testing.T) {
	api := newTestAPI(t)
	req := uploadRequest(t, "line\n", true, map[string]string{
		"purpose": PurposeBatch, "filename": "q.jsonl", "notes": strings.Repeat("x", 1024+multipartSlack),
	})
	// No declared length, as with a chunked upload, so only reading can find the body too large.
	req.ContentLength = -1

	rec := api.call(t, "alice", req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "parts the API does not use still count toward the cap: %s", rec.Body.String())
	assert.Empty(t, storedFiles(t, api.root), "the file stored before the cap was hit is removed")
}

// A limit too large to read as a number is a large page, like any limit over the maximum.
func TestAListLimitTooLargeForAnIntIsTheLargestPage(t *testing.T) {
	api := newTestAPI(t)
	mustUpload(t, api, "alice", "line\n")
	rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"?limit=99999999999999999999", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, decode(t, rec)["data"], 1)
	negative := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"?limit=-99999999999999999999", nil))
	assert.Equal(t, http.StatusBadRequest, negative.Code, "a negative limit stays refused, however large")
}

// Uploads only take purpose batch, so a listing for any other purpose must not walk a tenant's whole index.
func TestListingAPurposeThatIsNeverStoredReadsNothing(t *testing.T) {
	api := newTestAPI(t)
	for i := 0; i < 3; i++ {
		mustUpload(t, api, "alice", "line\n")
	}
	trips := &roundTrips{}
	api.store.client.AddHook(trips)

	for _, purpose := range []string{"assistants", PurposeBatchOutput} {
		rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"?purpose="+purpose, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Empty(t, decode(t, rec)["data"], purpose)
		assert.Equal(t, int64(0), trips.n.Load(), "nothing to read for %s, which no file has yet", purpose)
	}
}

func TestListFilesPageSizes(t *testing.T) {
	api := newTestAPI(t)
	addFiles(t, api.store, tenantID("", "alice"), PurposeBatch, maxPageSize+5)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{name: "no limit uses the default page", query: "", want: defaultPageSize},
		{name: "a limit over the largest page is clamped", query: "?limit=1000", want: maxPageSize},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := api.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+tt.query, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			body := decode(t, rec)
			assert.Len(t, body["data"], tt.want)
			assert.Equal(t, true, body["has_more"])
		})
	}
}

// failingCreate is a volume that cannot store a file, or a client that left while it was stored.
type failingCreate struct {
	FileStore
	leave context.CancelFunc
}

func (s failingCreate) Create(context.Context, string, string, io.Reader, int64) (int64, error) {
	if s.leave != nil {
		s.leave()
	}
	return 0, errors.New("write failed")
}

func TestUploadFailureIsBlamedOnTheRightSide(t *testing.T) {
	tests := []struct {
		name   string
		gone   bool
		status int
		kind   string
	}{
		{name: "the volume fails while the client waits", status: http.StatusServiceUnavailable, kind: errAPI},
		{name: "the client is gone, so the volume is not blamed", gone: true, status: http.StatusBadRequest, kind: errInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			ctx, leave := context.WithCancel(context.Background())
			defer leave()
			volume := failingCreate{FileStore: api.files}
			if tt.gone {
				volume.leave = leave
			}
			api.service.files = volume
			req := uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch})

			rec := api.call(t, "alice", req.WithContext(ctx))
			assert.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.kind, errorOf(t, rec)["type"])
		})
	}
}

// sweptStore loses the record between the lookup and the delete.
type sweptStore struct{ Store }

func (sweptStore) DeleteFile(context.Context, string) (bool, error) { return false, ErrNotFound }

func TestDeleteOfARecordThatJustWentAwaySucceeds(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	api.service.store = sweptStore{api.store}

	rec := api.call(t, "alice", httptest.NewRequest(http.MethodDelete, filesPath+"/"+id, nil))
	require.Equal(t, http.StatusOK, rec.Code, "the file is gone either way, which is what the caller asked for: %s", rec.Body.String())
	assert.Equal(t, true, decode(t, rec)["deleted"])
}
