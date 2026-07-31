/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

    10|Unless required by applicable law or agreed to in writing, software
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
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// LineDispatcher executes one batch input line against a model endpoint.
// Implementations typically reuse the router's load-balancing path.
type LineDispatcher func(ctx context.Context, endpoint string, body json.RawMessage) (statusCode int, responseBody []byte, requestID string, err error)

// InteractiveLoadFunc returns the current interactive active-request count.
// Used so batch concurrency can back off under interactive pressure.
type InteractiveLoadFunc func() int64

// Worker processes batch jobs asynchronously inside the router process.
type Worker struct {
	files         FileStore
	batches       BatchStore
	dispatch      LineDispatcher
	interactive   InteractiveLoadFunc
	concurrency   int
	busyThreshold int64

	queue chan string
	wg    sync.WaitGroup

	mu       sync.Mutex
	inflight map[string]struct{}
}

// WorkerConfig configures the batch worker.
type WorkerConfig struct {
	Concurrency              int
	InteractiveBusyThreshold int64
	QueueSize                int
}

// NewWorker constructs a batch worker. dispatch must be non-nil for processing.
func NewWorker(files FileStore, batches BatchStore, dispatch LineDispatcher, interactive InteractiveLoadFunc, cfg WorkerConfig) *Worker {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultMaxConcurrency
	}
	busy := cfg.InteractiveBusyThreshold
	if busy < 0 {
		busy = DefaultInteractiveBusyThreshold
	}
	qsize := cfg.QueueSize
	if qsize <= 0 {
		qsize = 256
	}
	return &Worker{
		files:         files,
		batches:       batches,
		dispatch:      dispatch,
		interactive:   interactive,
		concurrency:   concurrency,
		busyThreshold: busy,
		queue:         make(chan string, qsize),
		inflight:      make(map[string]struct{}),
	}
}

// Enqueue schedules a batch ID for processing (non-blocking; drops if full after warn).
func (w *Worker) Enqueue(batchID string) {
	if w == nil || batchID == "" {
		return
	}
	select {
	case w.queue <- batchID:
	default:
		klog.Warningf("batch worker queue full; dropping enqueue for %s", batchID)
	}
}

// Start runs the worker loop until ctx is cancelled, then waits for in-flight jobs.
func (w *Worker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-w.queue:
				w.processOne(ctx, id)
			}
		}
	}()
}

// Stop waits for the worker loop to exit (caller must cancel Start's context first).
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.wg.Wait()
}

func (w *Worker) processOne(ctx context.Context, batchID string) {
	w.mu.Lock()
	if _, ok := w.inflight[batchID]; ok {
		w.mu.Unlock()
		return
	}
	w.inflight[batchID] = struct{}{}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.inflight, batchID)
		w.mu.Unlock()
	}()

	batch, err := w.batches.Get(ctx, batchID)
	if err != nil {
		klog.Errorf("batch worker: get %s: %v", batchID, err)
		return
	}

	switch batch.Status {
	case StatusValidating:
		w.runValidating(ctx, batch)
	case StatusInProgress, StatusCancelling:
		// Resume / continue — re-read input and process remaining is complex;
		// for MVP re-run full processing only from validating → in_progress.
		// If already in_progress after restart, mark failed with clear error.
		if batch.Status == StatusInProgress && batch.RequestCounts.Completed+batch.RequestCounts.Failed == 0 {
			w.runInProgress(ctx, batch, nil)
			return
		}
		if batch.Status == StatusCancelling {
			w.finishCancelled(ctx, batch)
		}
	default:
		// Terminal or finalizing — nothing to do.
	}
}

func (w *Worker) runValidating(ctx context.Context, batch *BatchObject) {
	lines, verrs, err := w.readAndValidate(ctx, batch)
	if err != nil {
		w.failBatch(ctx, batch, "invalid_request_error", err.Error(), nil)
		return
	}
	if len(verrs) > 0 {
		w.failBatch(ctx, batch, verrs[0].Code, verrs[0].Message, verrs)
		return
	}

	now := time.Now().Unix()
	batch.Status = StatusInProgress
	batch.InProgressAt = &now
	batch.RequestCounts.Total = len(lines)
	if _, err := w.batches.Update(ctx, batch); err != nil {
		klog.Errorf("batch worker: update in_progress %s: %v", batch.ID, err)
		return
	}
	w.runInProgress(ctx, batch, lines)
}

func (w *Worker) readAndValidate(ctx context.Context, batch *BatchObject) ([]InputLine, []BatchError, error) {
	rc, meta, err := w.files.Open(ctx, batch.InputFileID)
	if err != nil {
		return nil, nil, fmt.Errorf("open input file: %w", err)
	}
	defer rc.Close()
	_ = meta

	scanner := bufio.NewScanner(rc)
	// Allow large JSONL lines (up to ~1 MiB per line by default buffer growth).
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var lines []InputLine
	var errs []BatchError
	seen := make(map[string]struct{})
	lineNo := 0

	for scanner.Scan() {
		lineNo++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		if len(lines) >= DefaultMaxRequestsPerBatch {
			ln := lineNo
			errs = append(errs, BatchError{
				Code:    "too_many_requests",
				Message: fmt.Sprintf("batch exceeds max %d requests", DefaultMaxRequestsPerBatch),
				Line:    &ln,
			})
			break
		}

		var in InputLine
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			ln := lineNo
			errs = append(errs, BatchError{Code: "invalid_json", Message: err.Error(), Line: &ln})
			continue
		}
		if in.CustomID == "" {
			ln := lineNo
			errs = append(errs, BatchError{Code: "missing_custom_id", Message: "custom_id is required", Line: &ln})
			continue
		}
		if _, dup := seen[in.CustomID]; dup {
			ln := lineNo
			errs = append(errs, BatchError{Code: "duplicate_custom_id", Message: in.CustomID, Line: &ln})
			continue
		}
		seen[in.CustomID] = struct{}{}
		if !strings.EqualFold(in.Method, "POST") {
			ln := lineNo
			errs = append(errs, BatchError{Code: "invalid_method", Message: in.Method, Line: &ln})
			continue
		}
		if in.URL != batch.Endpoint {
			ln := lineNo
			errs = append(errs, BatchError{
				Code:    "invalid_url",
				Message: fmt.Sprintf("url %q does not match batch endpoint %q", in.URL, batch.Endpoint),
				Line:    &ln,
			})
			continue
		}
		if len(in.Body) == 0 || string(in.Body) == "null" {
			ln := lineNo
			errs = append(errs, BatchError{Code: "missing_body", Message: "body is required", Line: &ln})
			continue
		}
		lines = append(lines, in)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read input file: %w", err)
	}
	if len(lines) == 0 && len(errs) == 0 {
		return nil, []BatchError{{Code: "empty_batch", Message: "input file has no requests"}}, nil
	}
	return lines, errs, nil
}

func (w *Worker) runInProgress(ctx context.Context, batch *BatchObject, lines []InputLine) {
	var err error
	if lines == nil {
		lines, _, err = w.readAndValidate(ctx, batch)
		if err != nil || len(lines) == 0 {
			w.failBatch(ctx, batch, "invalid_request_error", "failed to resume batch", nil)
			return
		}
	}

	sem := make(chan struct{}, w.concurrency)
	var (
		mu        sync.Mutex
		outLines  []OutputLine
		errLines  []OutputLine
		completed int
		failed    int
		cancelled bool
		expired   bool
	)

	var wg sync.WaitGroup
	for _, line := range lines {
		// Refresh cancel / expiry between scheduling.
		cur, gerr := w.batches.Get(ctx, batch.ID)
		if gerr == nil {
			batch = cur
		}
		if batch.Status == StatusCancelling {
			cancelled = true
			break
		}
		if batch.ExpiresAt != nil && time.Now().Unix() > *batch.ExpiresAt {
			expired = true
			break
		}

		w.waitForCapacity(ctx)
		if ctx.Err() != nil {
			return
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(in InputLine) {
			defer wg.Done()
			defer func() { <-sem }()

			out := w.dispatchLine(ctx, batch.Endpoint, in)
			mu.Lock()
			defer mu.Unlock()
			if out.Error != nil {
				errLines = append(errLines, out)
				failed++
			} else {
				outLines = append(outLines, out)
				completed++
			}
			batch.RequestCounts.Completed = completed
			batch.RequestCounts.Failed = failed
			batch.RequestCounts.Total = len(lines)
			if _, uerr := w.batches.Update(ctx, batch); uerr != nil {
				klog.Warningf("batch worker: progress update %s: %v", batch.ID, uerr)
			}
		}(line)
	}
	wg.Wait()

	// Re-read for cancel that arrived during run.
	if cur, gerr := w.batches.Get(ctx, batch.ID); gerr == nil {
		batch = cur
		if batch.Status == StatusCancelling {
			cancelled = true
		}
	}

	batch.RequestCounts = RequestCounts{Total: len(lines), Completed: completed, Failed: failed}

	if cancelled {
		_ = w.writeResultFiles(ctx, batch, outLines, errLines)
		w.finishCancelled(ctx, batch)
		return
	}
	if expired {
		_ = w.writeResultFiles(ctx, batch, outLines, errLines)
		now := time.Now().Unix()
		batch.Status = StatusExpired
		batch.ExpiredAt = &now
		_, _ = w.batches.Update(ctx, batch)
		return
	}

	now := time.Now().Unix()
	batch.Status = StatusFinalizing
	batch.FinalizingAt = &now
	_, _ = w.batches.Update(ctx, batch)

	if err := w.writeResultFiles(ctx, batch, outLines, errLines); err != nil {
		w.failBatch(ctx, batch, "server_error", err.Error(), nil)
		return
	}

	now = time.Now().Unix()
	batch.Status = StatusCompleted
	batch.CompletedAt = &now
	if _, err := w.batches.Update(ctx, batch); err != nil {
		klog.Errorf("batch worker: complete %s: %v", batch.ID, err)
	}
}

func (w *Worker) dispatchLine(ctx context.Context, endpoint string, in InputLine) OutputLine {
	id := "batch_req_" + uuid.New().String()
	if w.dispatch == nil {
		return OutputLine{
			ID:       id,
			CustomID: in.CustomID,
			Error:    &LineError{Code: "server_error", Message: "batch dispatcher not configured"},
		}
	}
	status, body, reqID, err := w.dispatch(ctx, endpoint, in.Body)
	if err != nil {
		return OutputLine{
			ID:       id,
			CustomID: in.CustomID,
			Error:    &LineError{Code: "server_error", Message: err.Error()},
		}
	}
	if status >= 400 {
		msg := string(body)
		if msg == "" {
			msg = fmt.Sprintf("upstream status %d", status)
		}
		raw := body
		if !json.Valid(raw) {
			quoted, _ := json.Marshal(string(raw))
			raw = quoted
		}
		return OutputLine{
			ID:       id,
			CustomID: in.CustomID,
			Response: &LineResponse{StatusCode: status, RequestID: reqID, Body: raw},
			Error:    &LineError{Code: "upstream_error", Message: msg},
		}
	}
	raw := body
	if !json.Valid(raw) {
		quoted, _ := json.Marshal(string(raw))
		raw = quoted
	}
	return OutputLine{
		ID:       id,
		CustomID: in.CustomID,
		Response: &LineResponse{StatusCode: status, RequestID: reqID, Body: raw},
	}
}

func (w *Worker) writeResultFiles(ctx context.Context, batch *BatchObject, outLines, errLines []OutputLine) error {
	if len(outLines) > 0 {
		buf, err := encodeJSONL(outLines)
		if err != nil {
			return err
		}
		obj, err := w.files.Create(ctx, batch.ID+"-output.jsonl", PurposeBatchOutput, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		batch.OutputFileID = &obj.ID
	}
	if len(errLines) > 0 {
		buf, err := encodeJSONL(errLines)
		if err != nil {
			return err
		}
		obj, err := w.files.Create(ctx, batch.ID+"-errors.jsonl", PurposeBatchOutput, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("create error file: %w", err)
		}
		batch.ErrorFileID = &obj.ID
	}
	_, err := w.batches.Update(ctx, batch)
	return err
}

func encodeJSONL(lines []OutputLine) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func (w *Worker) failBatch(ctx context.Context, batch *BatchObject, code, message string, details []BatchError) {
	now := time.Now().Unix()
	batch.Status = StatusFailed
	batch.FailedAt = &now
	if len(details) == 0 {
		details = []BatchError{{Code: code, Message: message}}
	}
	batch.Errors = &BatchErrors{Object: "list", Data: details}
	if _, err := w.batches.Update(ctx, batch); err != nil {
		klog.Errorf("batch worker: fail %s: %v", batch.ID, err)
	}
}

func (w *Worker) finishCancelled(ctx context.Context, batch *BatchObject) {
	now := time.Now().Unix()
	batch.Status = StatusCancelled
	batch.CancelledAt = &now
	if _, err := w.batches.Update(ctx, batch); err != nil {
		klog.Errorf("batch worker: cancel %s: %v", batch.ID, err)
	}
}

func (w *Worker) waitForCapacity(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if w.interactive == nil || w.busyThreshold <= 0 {
			return
		}
		if w.interactive() < w.busyThreshold {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
