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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"k8s.io/klog/v2"
)

const (
	filesPath       = "/v1/files"
	minExpiry       = int64(3600)
	maxExpiry       = int64(2592000)
	maxFormValue    = 1 << 12
	maxFilename     = 255
	multipartSlack  = 1 << 20
	defaultMaxBytes = int64(200 << 20)
	defaultPageSize = 20
	maxPageSize     = 100
	// settleTimeout bounds the lookup that settles an upload whose record write failed.
	settleTimeout = 5 * time.Second
)

// errFieldTooLong means a form field other than the file went over maxFormValue.
var errFieldTooLong = errors.New("a form field is longer than " + strconv.Itoa(maxFormValue) + " bytes")

// Config holds the settings of the batch API.
type Config struct {
	// MaxFileBytes caps one uploaded file; 0 uses the OpenAI limit of 200 MB.
	MaxFileBytes int64
	// FileExpiration is how long an upload lives when the client sets no expiry; 0 is forever.
	FileExpiration time.Duration
}

// Service serves the OpenAI-compatible files API.
type Service struct {
	store  Store
	files  FileStore
	config Config
	now    func() time.Time
	newID  func(prefix string) string
}

// NewService builds the API on a record store and a file store.
func NewService(store Store, files FileStore, config Config) *Service {
	if config.MaxFileBytes <= 0 {
		config.MaxFileBytes = defaultMaxBytes
	}
	return &Service{store: store, files: files, config: config, now: time.Now, newID: newID}
}

func newID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		panic("batch: cannot read randomness: " + err.Error())
	}
	return prefix + hex.EncodeToString(buf)
}

// Handles reports whether the path belongs to this API.
func (s *Service) Handles(_, path string) bool {
	return path == filesPath || strings.HasPrefix(path, filesPath+"/")
}

// Serve answers one request that Handles accepted.
func (s *Service) Serve(c *gin.Context) {
	method := c.Request.Method
	rest := strings.Trim(strings.TrimPrefix(c.Request.URL.Path, filesPath), "/")
	switch {
	case rest == "" && method == http.MethodGet:
		s.listFiles(c)
	case rest == "" && method == http.MethodPost:
		s.uploadFile(c)
	case strings.HasSuffix(rest, "/content") && method == http.MethodGet:
		s.fileContent(c, strings.TrimSuffix(rest, "/content"))
	case rest != "" && !strings.Contains(rest, "/") && method == http.MethodGet:
		s.getFile(c, rest)
	case rest != "" && !strings.Contains(rest, "/") && method == http.MethodDelete:
		s.deleteFile(c, rest)
	default:
		fail(c, http.StatusNotFound, errInvalidRequest, "", "unknown route "+method+" "+c.Request.URL.Path)
	}
}

func fileObject(file *File) gin.H {
	out := gin.H{
		"id":         file.ID,
		"object":     "file",
		"bytes":      file.Bytes,
		"created_at": file.CreatedAt,
		"expires_at": nil,
		"filename":   file.Filename,
		"purpose":    file.Purpose,
		"status":     file.Status,
	}
	if file.ExpiresAt != 0 {
		out["expires_at"] = file.ExpiresAt
	}
	return out
}

// upload is what the multipart form held.
type upload struct {
	purpose  string
	filename string
	anchor   string
	seconds  string
	written  int64
	gotFile  bool
}

func (s *Service) uploadFile(c *gin.Context) {
	tenant := tenantOf(c)
	ctx := c.Request.Context()
	// cleanup still removes bytes after the client is gone and the request is cancelled.
	cleanup := context.WithoutCancel(ctx)
	// A declared size over the limit is refused before the client sends the body.
	if c.Request.ContentLength > s.config.MaxFileBytes+multipartSlack {
		fail(c, http.StatusRequestEntityTooLarge, errInvalidRequest, "file", s.tooLarge())
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.config.MaxFileBytes+multipartSlack)
	reader, err := c.Request.MultipartReader()
	if err != nil {
		fail(c, http.StatusBadRequest, errInvalidRequest, "file", "expected a multipart upload")
		return
	}
	id := s.newID("file-")
	// Asking for the new ID first refuses the upload before any byte is stored while the store is down.
	if _, err := s.store.GetFile(ctx, id); !errors.Is(err, ErrNotFound) {
		unavailable(c, "cannot record files right now", err)
		return
	}
	var form upload
	// reject removes the bytes already written and answers the error.
	reject := func(status int, kind, param, message string) {
		if form.gotFile {
			_ = s.files.Remove(cleanup, tenant, id)
		}
		fail(c, status, kind, param, message)
	}
	for {
		part, err := reader.NextPart()
		// Only a bare io.EOF is the end of the form; a wrapped one means the body was cut off.
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadFailed(reject, err, false)
			return
		}
		// A part is never closed here: Close would read the rest of it, even of one already rejected.
		storing := false
		switch part.FormName() {
		case "file":
			if form.gotFile {
				reject(http.StatusBadRequest, errInvalidRequest, "file", "only one file part is allowed")
				return
			}
			storing = true
			form.filename = part.FileName()
			form.written, err = s.files.Create(ctx, tenant, id, part, s.config.MaxFileBytes)
			form.gotFile = err == nil
		case "purpose":
			form.purpose, err = readFormValue(part)
		case "expires_after[anchor]":
			form.anchor, err = readFormValue(part)
		case "expires_after[seconds]":
			form.seconds, err = readFormValue(part)
		}
		if err != nil {
			// A client that left cancels the request, and its failed read is not the volume's failure.
			s.uploadFailed(reject, err, storing && !errors.Is(ctx.Err(), context.Canceled))
			return
		}
	}
	ttl, param, message := s.checkUpload(&form)
	if message != "" {
		reject(http.StatusBadRequest, errInvalidRequest, param, message)
		return
	}
	now := s.now()
	file := &File{
		ID:        id,
		Tenant:    tenant,
		Filename:  form.filename,
		Purpose:   form.purpose,
		Status:    FileProcessed,
		Bytes:     form.written,
		CreatedAt: now.Unix(),
	}
	if ttl > 0 {
		file.ExpiresAt = now.Add(ttl).Unix()
	}
	if err := s.recordFile(ctx, file); err != nil {
		unavailable(c, "cannot record the uploaded file", err)
		return
	}
	c.JSON(http.StatusOK, fileObject(file))
}

// recordFile stores the record; after a failure the bytes go only when a lookup shows no record points at them.
func (s *Service) recordFile(ctx context.Context, file *File) error {
	err := s.store.CreateFile(ctx, file)
	if err == nil {
		return nil
	}
	detached := context.WithoutCancel(ctx)
	settle, cancel := context.WithTimeout(detached, settleTimeout)
	defer cancel()
	_, lookupErr := s.store.GetFile(settle, file.ID)
	switch {
	case lookupErr == nil:
		// The record is there, so only the reply to the write was lost.
		return nil
	case errors.Is(lookupErr, ErrNotFound):
		_ = s.files.Remove(detached, file.Tenant, file.ID)
		return err
	default:
		return fmt.Errorf("%w; the record may exist, so the bytes of %s are kept", err, file.ID)
	}
}

// checkUpload validates the form and returns how long the file lives.
func (s *Service) checkUpload(form *upload) (time.Duration, string, string) {
	if !form.gotFile {
		return 0, "file", "no file part in the upload"
	}
	if form.purpose != PurposeBatch {
		return 0, "purpose", "files must be uploaded with purpose " + PurposeBatch
	}
	if len(form.filename) > maxFilename {
		return 0, "file", "the filename may be at most " + strconv.Itoa(maxFilename) + " bytes"
	}
	if form.anchor == "" && form.seconds == "" {
		return s.config.FileExpiration, "", ""
	}
	if form.anchor != "" && form.anchor != "created_at" {
		return 0, "expires_after", "expires_after.anchor must be created_at"
	}
	seconds, err := strconv.ParseInt(form.seconds, 10, 64)
	if err != nil || seconds < minExpiry || seconds > maxExpiry {
		return 0, "expires_after", "expires_after.seconds must be between 3600 and 2592000"
	}
	return time.Duration(seconds) * time.Second, "", ""
}

// uploadFailed answers a failed upload: too large is 413, a broken form 400, the volume 503.
func (s *Service) uploadFailed(reject func(int, string, string, string), err error, volume bool) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, ErrTooLarge) || errors.As(err, &tooBig):
		reject(http.StatusRequestEntityTooLarge, errInvalidRequest, "file", s.tooLarge())
	case errors.Is(err, errFieldTooLong):
		reject(http.StatusBadRequest, errInvalidRequest, "", err.Error())
	case volume && !errors.Is(err, io.ErrUnexpectedEOF):
		klog.Errorf("batch: cannot store the uploaded file: %v", err)
		reject(http.StatusServiceUnavailable, errAPI, "", "cannot store the uploaded file")
	default:
		reject(http.StatusBadRequest, errInvalidRequest, "file", "malformed multipart upload")
	}
}

func (s *Service) tooLarge() string {
	return "file is larger than " + strconv.FormatInt(s.config.MaxFileBytes, 10) + " bytes"
}

// readFormValue reads a short field; a longer one is refused, as a cut value could pass a check the whole one fails.
func readFormValue(part io.Reader) (string, error) {
	value, err := io.ReadAll(io.LimitReader(part, maxFormValue+1))
	if err == nil && len(value) > maxFormValue {
		err = errFieldTooLong
	}
	return strings.TrimSpace(string(value)), err
}

// lookup returns the caller's file; another tenant's file answers exactly like a missing one.
func (s *Service) lookup(c *gin.Context, id string) (*File, bool) {
	file, err := s.store.GetFile(c.Request.Context(), id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		unavailable(c, "cannot read the file record", err)
		return nil, false
	}
	if err != nil || file.Deleted || file.Tenant != tenantOf(c) {
		fail(c, http.StatusNotFound, errInvalidRequest, "id", "no such file: "+id)
		return nil, false
	}
	return file, true
}

func (s *Service) getFile(c *gin.Context, id string) {
	if file, ok := s.lookup(c, id); ok {
		c.JSON(http.StatusOK, fileObject(file))
	}
}

func (s *Service) fileContent(c *gin.Context, id string) {
	file, ok := s.lookup(c, id)
	if !ok {
		return
	}
	body, err := s.files.Open(c.Request.Context(), file.Tenant, file.ID)
	if errors.Is(err, ErrNotFound) {
		fail(c, http.StatusNotFound, errInvalidRequest, "id", "no such file: "+id)
		return
	}
	if err != nil {
		unavailable(c, "cannot read the file", err)
		return
	}
	defer body.Close()
	c.Header("Content-Type", "application/jsonl")
	http.ServeContent(&contentWriter{ResponseWriter: c.Writer, c: c}, c.Request, "", time.Unix(file.CreatedAt, 0), body)
}

// contentWriter answers the errors of http.ServeContent in the OpenAI shape instead of plain text.
type contentWriter struct {
	http.ResponseWriter
	c      *gin.Context
	failed bool
}

func (w *contentWriter) WriteHeader(status int) {
	if status < http.StatusBadRequest {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	// http.Error has set a plain-text type, which gin would keep for the JSON body.
	w.Header().Del("Content-Type")
	w.failed = true
	switch {
	case status < http.StatusInternalServerError:
		fail(w.c, status, errInvalidRequest, "", strings.ToLower(http.StatusText(status)))
	default:
		unavailable(w.c, "cannot read the file", errors.New("serving the content failed with status "+strconv.Itoa(status)))
	}
}

func (w *contentWriter) Write(p []byte) (int, error) {
	if w.failed {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}

func (s *Service) deleteFile(c *gin.Context, id string) {
	file, ok := s.lookup(c, id)
	if !ok {
		return
	}
	removable, err := s.store.DeleteFile(c.Request.Context(), id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		unavailable(c, "cannot delete the file", err)
		return
	}
	if removable {
		// The record is already deleted, so the bytes go even if the client left; leftovers are only logged.
		err := s.files.Remove(context.WithoutCancel(c.Request.Context()), file.Tenant, file.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			klog.Errorf("batch: bytes of deleted file %s were not removed: %v", file.ID, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"id": file.ID, "object": "file", "deleted": true})
}

func (s *Service) listFiles(c *gin.Context) {
	query := FileQuery{Tenant: tenantOf(c), Purpose: c.Query("purpose"), After: c.Query("after"), Limit: defaultPageSize}
	if raw := c.Query("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			fail(c, http.StatusBadRequest, errInvalidRequest, "limit", "limit must be a positive number")
			return
		}
		query.Limit = min(limit, maxPageSize)
	}
	switch c.Query("order") {
	case "", "desc":
	case "asc":
		query.Ascending = true
	default:
		fail(c, http.StatusBadRequest, errInvalidRequest, "order", "order must be asc or desc")
		return
	}
	files, more, err := s.store.ListFiles(c.Request.Context(), query)
	if errors.Is(err, ErrNotFound) {
		fail(c, http.StatusBadRequest, errInvalidRequest, "after", "unknown cursor")
		return
	}
	if err != nil {
		unavailable(c, "cannot list files right now", err)
		return
	}
	data := make([]gin.H, 0, len(files))
	for _, file := range files {
		data = append(data, fileObject(file))
	}
	out := gin.H{"object": "list", "data": data, "has_more": more, "first_id": nil, "last_id": nil}
	if len(files) > 0 {
		out["first_id"] = files[0].ID
		out["last_id"] = files[len(files)-1].ID
	}
	c.JSON(http.StatusOK, out)
}
