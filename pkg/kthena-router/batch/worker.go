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
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"k8s.io/klog/v2"
)

// LineDispatcher executes one batch input line against a model endpoint.
type LineDispatcher func(ctx context.Context, endpoint string, body json.RawMessage) (statusCode int, responseBody []byte, requestID string, err error)

// InteractiveLoadFunc returns the current interactive active-request count.
type InteractiveLoadFunc func() int64

// Worker processes batch jobs asynchronously inside the router process.
type Worker struct {
	files         FileStore
	batches       BatchStore
	dispatch      LineDispatcher
	interactive   InteractiveLoadFunc
	concurrency   int
	busyThreshold int64

	queue       chan string
	loopWG      sync.WaitGroup
	jobWG       sync.WaitGroup
	mu          sync.Mutex
	inflight    map[string]struct{}
	activeLines atomic.Int64
}

// WorkerConfig configures the batch worker.
type WorkerConfig struct {
	Concurrency              int
	InteractiveBusyThreshold int64
	QueueSize                int
}

// NewWorker constructs a batch worker.
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
		qsize = DefaultWorkerQueueSize
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

// Enqueue schedules a batch ID for processing. Blocks until queued or ctx is done.
func (w *Worker) Enqueue(ctx context.Context, batchID string) error {
	if w == nil || batchID == "" {
		return nil
	}
	select {
	case w.queue <- batchID:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", ErrEnqueueTimeout, ctx.Err())
	}
}

// Start runs the worker loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	w.loopWG.Add(1)
	go func() {
		defer w.loopWG.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-w.queue:
				w.jobWG.Add(1)
				go func(batchID string) {
					defer w.jobWG.Done()
					w.processOne(ctx, batchID)
				}(id)
			}
		}
	}()
}

// Stop waits for the loop and in-flight jobs to finish.
// Caller must cancel the context passed to Start first.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.loopWG.Wait()
	w.jobWG.Wait()
}

func (w *Worker) tryBegin(batchID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.inflight[batchID]; ok {
		return false
	}
	w.inflight[batchID] = struct{}{}
	return true
}

func (w *Worker) end(batchID string) {
	w.mu.Lock()
	delete(w.inflight, batchID)
	w.mu.Unlock()
}

func (w *Worker) processOne(ctx context.Context, batchID string) {
	if !w.tryBegin(batchID) {
		return
	}
	defer w.end(batchID)

	b, err := w.batches.Get(ctx, batchID)
	if err != nil {
		klog.Errorf("batch worker: get %s: %v", batchID, err)
		return
	}

	switch b.Status {
	case StatusValidating:
		w.runValidating(ctx, b)
	case StatusInProgress:
		if b.RequestCounts.Completed+b.RequestCounts.Failed == 0 {
			w.runInProgress(ctx, b, nil)
		}
	case StatusCancelling:
		w.finishCancelled(ctx, b)
	}
}

func (w *Worker) runValidating(ctx context.Context, b *BatchObject) {
	lines, verrs, err := w.readAndValidate(ctx, b)
	if err != nil {
		w.failBatch(ctx, b, "invalid_request_error", err.Error(), nil)
		return
	}
	if len(verrs) > 0 {
		w.failBatch(ctx, b, verrs[0].Code, verrs[0].Message, verrs)
		return
	}

	now := time.Now().Unix()
	b.Status = StatusInProgress
	b.InProgressAt = &now
	b.RequestCounts.Total = len(lines)
	if _, err := w.batches.Update(ctx, b); err != nil {
		klog.Errorf("batch worker: update in_progress %s: %v", b.ID, err)
		return
	}
	w.runInProgress(ctx, b, lines)
}

func (w *Worker) readAndValidate(ctx context.Context, b *BatchObject) ([]InputLine, []BatchError, error) {
	rc, _, err := w.files.Open(ctx, b.InputFileID)
	if err != nil {
		return nil, nil, fmt.Errorf("open input file: %w", err)
	}
	defer rc.Close()

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, DefaultScannerBufSize), DefaultScannerMaxToken)

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
		if verr := validateInputLine(in, b.Endpoint, lineNo, seen); verr != nil {
			errs = append(errs, *verr)
			continue
		}
		seen[in.CustomID] = struct{}{}
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

func validateInputLine(in InputLine, endpoint string, lineNo int, seen map[string]struct{}) *BatchError {
	ln := lineNo
	if in.CustomID == "" {
		return &BatchError{Code: "missing_custom_id", Message: "custom_id is required", Line: &ln}
	}
	if _, dup := seen[in.CustomID]; dup {
		return &BatchError{Code: "duplicate_custom_id", Message: in.CustomID, Line: &ln}
	}
	if !strings.EqualFold(in.Method, httpMethodPOST) {
		return &BatchError{Code: "invalid_method", Message: in.Method, Line: &ln}
	}
	if in.URL != endpoint {
		return &BatchError{
			Code:    "invalid_url",
			Message: fmt.Sprintf("url %q does not match batch endpoint %q", in.URL, endpoint),
			Line:    &ln,
		}
	}
	if len(in.Body) == 0 || string(in.Body) == "null" {
		return &BatchError{Code: "missing_body", Message: "body is required", Line: &ln}
	}
	return nil
}

const httpMethodPOST = "POST"

func (w *Worker) runInProgress(ctx context.Context, b *BatchObject, lines []InputLine) {
	if lines == nil {
		var err error
		lines, _, err = w.readAndValidate(ctx, b)
		if err != nil || len(lines) == 0 {
			w.failBatch(ctx, b, "invalid_request_error", "failed to resume batch", nil)
			return
		}
	}

	endpoint := b.Endpoint
	batchID := b.ID
	total := len(lines)

	sem := make(chan struct{}, w.concurrency)
	var (
		mu          sync.Mutex
		outLines    []OutputLine
		errLines    []OutputLine
		completed   int
		failed      int
		cancelled   bool
		expired     bool
		interrupted bool
		processed   = make(map[string]struct{}, total)
	)

	var wg sync.WaitGroup
	for _, line := range lines {
		cur, gerr := w.batches.Get(ctx, batchID)
		if gerr == nil {
			if cur.Status == StatusCancelling {
				cancelled = true
				break
			}
			if cur.ExpiresAt != nil && time.Now().Unix() > *cur.ExpiresAt {
				expired = true
				break
			}
		}

		w.waitForCapacity(ctx)
		if ctx.Err() != nil {
			interrupted = true
			break
		}

		sem <- struct{}{}
		wg.Add(1)
		go func(in InputLine) {
			defer wg.Done()
			defer func() { <-sem }()

			out := w.dispatchLine(ctx, endpoint, in)

			mu.Lock()
			processed[in.CustomID] = struct{}{}
			if out.Error != nil {
				errLines = append(errLines, out)
				failed++
			} else {
				outLines = append(outLines, out)
				completed++
			}
			counts := RequestCounts{Total: total, Completed: completed, Failed: failed}
			mu.Unlock()

			if err := w.batches.UpdateRequestCounts(ctx, batchID, counts); err != nil {
				klog.Warningf("batch worker: progress update %s: %v", batchID, err)
			}
		}(line)
	}
	wg.Wait()

	cur, err := w.batches.Get(ctx, batchID)
	if err != nil {
		klog.Errorf("batch worker: reload %s: %v", batchID, err)
		return
	}
	if cur.Status == StatusCancelling {
		cancelled = true
	}

	// Lines never started because of cancel/expiry/interrupt.
	for _, in := range lines {
		if _, ok := processed[in.CustomID]; ok {
			continue
		}
		code := "batch_cancelled"
		msg := "This request was not executed because the batch was cancelled."
		if expired {
			code = "batch_expired"
			msg = "This request could not be executed before the completion window expired."
		} else if interrupted {
			code = "batch_interrupted"
			msg = "This request was not executed because the batch worker was interrupted."
		} else if !cancelled {
			continue
		}
		errLines = append(errLines, OutputLine{
			ID:       BatchRequestIDPrefix + uuid.New().String(),
			CustomID: in.CustomID,
			Error:    &LineError{Code: code, Message: msg},
		})
		failed++
	}

	cur.RequestCounts = RequestCounts{Total: total, Completed: completed, Failed: failed}

	switch {
	case cancelled:
		_ = w.writeResultFiles(ctx, cur, outLines, errLines)
		w.finishCancelled(ctx, cur)
	case expired:
		_ = w.writeResultFiles(ctx, cur, outLines, errLines)
		now := time.Now().Unix()
		cur.Status = StatusExpired
		cur.ExpiredAt = &now
		if _, err := w.batches.Update(ctx, cur); err != nil {
			klog.Errorf("batch worker: expire %s: %v", batchID, err)
		}
	case interrupted:
		_ = w.writeResultFiles(ctx, cur, outLines, errLines)
		w.failBatch(ctx, cur, "server_error", "batch worker interrupted", nil)
	default:
		now := time.Now().Unix()
		cur.Status = StatusFinalizing
		cur.FinalizingAt = &now
		if _, err := w.batches.Update(ctx, cur); err != nil {
			klog.Errorf("batch worker: finalizing %s: %v", batchID, err)
			return
		}
		if err := w.writeResultFiles(ctx, cur, outLines, errLines); err != nil {
			w.failBatch(ctx, cur, "server_error", err.Error(), nil)
			return
		}
		now = time.Now().Unix()
		cur.Status = StatusCompleted
		cur.CompletedAt = &now
		if _, err := w.batches.Update(ctx, cur); err != nil {
			klog.Errorf("batch worker: complete %s: %v", batchID, err)
		}
	}
}

func (w *Worker) dispatchLine(ctx context.Context, endpoint string, in InputLine) OutputLine {
	id := BatchRequestIDPrefix + uuid.New().String()
	w.activeLines.Add(1)
	defer w.activeLines.Add(-1)

	if w.dispatch == nil {
		return OutputLine{
			ID: id, CustomID: in.CustomID,
			Error: &LineError{Code: "server_error", Message: "batch dispatcher not configured"},
		}
	}

	status, body, reqID, err := w.dispatch(ctx, endpoint, in.Body)
	if err != nil {
		return OutputLine{
			ID: id, CustomID: in.CustomID,
			Error: &LineError{Code: "server_error", Message: err.Error()},
		}
	}

	raw := asJSONBody(body)
	line := OutputLine{
		ID:       id,
		CustomID: in.CustomID,
		Response: &LineResponse{StatusCode: status, RequestID: reqID, Body: raw},
	}
	if status >= 400 {
		msg := string(body)
		if msg == "" {
			msg = fmt.Sprintf("upstream status %d", status)
		}
		line.Error = &LineError{Code: "upstream_error", Message: msg}
	}
	return line
}

func asJSONBody(body []byte) json.RawMessage {
	if json.Valid(body) {
		return body
	}
	quoted, err := json.Marshal(string(body))
	if err != nil {
		return json.RawMessage(`""`)
	}
	return quoted
}

func (w *Worker) writeResultFiles(ctx context.Context, b *BatchObject, outLines, errLines []OutputLine) error {
	if len(outLines) > 0 {
		buf, err := encodeJSONL(outLines)
		if err != nil {
			return err
		}
		obj, err := w.files.Create(ctx, b.ID+"-output.jsonl", PurposeBatchOutput, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		b.OutputFileID = &obj.ID
	}
	if len(errLines) > 0 {
		buf, err := encodeJSONL(errLines)
		if err != nil {
			return err
		}
		obj, err := w.files.Create(ctx, b.ID+"-errors.jsonl", PurposeBatchOutput, bytes.NewReader(buf))
		if err != nil {
			return fmt.Errorf("create error file: %w", err)
		}
		b.ErrorFileID = &obj.ID
	}
	_, err := w.batches.Update(ctx, b)
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

func (w *Worker) failBatch(ctx context.Context, b *BatchObject, code, message string, details []BatchError) {
	now := time.Now().Unix()
	b.Status = StatusFailed
	b.FailedAt = &now
	if len(details) == 0 {
		details = []BatchError{{Code: code, Message: message}}
	}
	b.Errors = &BatchErrors{Object: ObjectList, Data: details}
	if _, err := w.batches.Update(ctx, b); err != nil {
		klog.Errorf("batch worker: fail %s: %v", b.ID, err)
	}
}

func (w *Worker) finishCancelled(ctx context.Context, b *BatchObject) {
	now := time.Now().Unix()
	b.Status = StatusCancelled
	b.CancelledAt = &now
	if _, err := w.batches.Update(ctx, b); err != nil {
		klog.Errorf("batch worker: cancel %s: %v", b.ID, err)
	}
}

func (w *Worker) waitForCapacity(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		if w.busyThreshold <= 0 {
			return
		}
		var interactive int64
		if w.interactive != nil {
			interactive = w.interactive()
		}
		load := interactive + w.activeLines.Load()
		if load < w.busyThreshold {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(DefaultBusyPollInterval):
		}
	}
}
