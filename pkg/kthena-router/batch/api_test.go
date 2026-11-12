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

func TestUploadChecksTheExpiryWindow(t *testing.T) {
	api := newTestAPI(t)
	for _, seconds := range []string{"60", "9999999"} {
		rec := api.call(t, "alice", uploadRequest(t, "line\n", true, map[string]string{
			"purpose": PurposeBatch, "filename": "q.jsonl", "expires_after[seconds]": seconds,
		}))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "seconds=%s is outside 1h..30d", seconds)
	}

	rec := api.call(t, "alice", uploadRequest(t, "line\n", true, map[string]string{
		"purpose": PurposeBatch, "filename": "q.jsonl", "expires_after[seconds]": "7200",
	}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	out := decode(t, rec)
	assert.InDelta(t, out["created_at"].(float64)+7200, out["expires_at"].(float64), 2)
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
	assert.Equal(t, http.StatusOK, rec.Code, "without JWT auth there is a single tenant; document it")

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
			if tt.fields["expires_after[seconds]"] != "" {
				want = created + 7200
			}
			assert.EqualValues(t, want, body["expires_at"])
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
