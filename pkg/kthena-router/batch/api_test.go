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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

func TestStoreDownIsServiceUnavailableNotNotFound(t *testing.T) {
	api := newTestAPI(t)
	id := mustUpload(t, api, "alice", "line\n")
	api.mini.Close()

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

func TestTokenWithoutASubjectIsNotAnonymous(t *testing.T) {
	api := newTestAPI(t)
	rec := api.callAs(t, "https://issuer-a", "", uploadRequest(t, "line\n", true,
		map[string]string{"purpose": PurposeBatch, "filename": "a.jsonl"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := decode(t, rec)["id"].(string)

	rec = api.call(t, "", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "a caller with no token must not read a file owned by a verified token")
	rec = api.callAs(t, "https://issuer-b", "", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusNotFound, rec.Code, "another issuer must not read it either")
	rec = api.callAs(t, "https://issuer-a", "", httptest.NewRequest(http.MethodGet, filesPath+"/"+id, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
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

// brokenStore fails every record write, stored or not, and can go silent after it.
type brokenStore struct {
	Store
	stores  bool
	answers bool
	wrote   bool
}

func (s *brokenStore) CreateFile(ctx context.Context, file *File) error {
	s.wrote = true
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
		status  int
		kept    int
	}{
		{name: "the record was stored and only the reply was lost", stores: true, answers: true, status: http.StatusOK, kept: 1},
		{name: "the record was not stored", answers: true, status: http.StatusServiceUnavailable, kept: 0},
		{name: "the store went silent, so the bytes stay", stores: true, status: http.StatusServiceUnavailable, kept: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newTestAPI(t)
			api.service.store = &brokenStore{Store: api.store, stores: tt.stores, answers: tt.answers}
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
