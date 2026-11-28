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
	"net/http"
	"strconv"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// byteOrderMark may start a UTF-8 file; it is not part of the first line.
const byteOrderMark = "\xEF\xBB\xBF"

// boms is the room a byte order mark takes on the first line.
func boms(first bool) int {
	if first {
		return len(byteOrderMark)
	}
	return 0
}

const (
	defaultConcurrency  = 8
	defaultMaxLineBytes = int64(1 << 20)
	progressEvery       = 200
	progressAfter       = 5 * time.Second
	// publishTimeout bounds publishing, which runs on after shutdown starts and must end inside the router's wait.
	publishTimeout = 10 * time.Second
	// resultExpiration is how long result files live, the OpenAI default for batch files.
	resultExpiration = 30 * 24 * time.Hour
)

// ErrResponseTooLarge means a model answered with more bytes than a batch line may keep.
var ErrResponseTooLarge = errors.New("batch: the response is too large")

// Request is one batch line, ready to be sent to a model.
type Request struct {
	JobID     string
	RequestID string
	Endpoint  string
	Tenant    string
	Subject   string
	Origin    Origin
	Body      []byte
}

// Response is what came back for one line, or why nothing did.
type Response struct {
	StatusCode int
	Body       []byte
	Err        error
}

// Dispatcher sends one batch line through the router's own request path.
type Dispatcher interface {
	Dispatch(ctx context.Context, request *Request) *Response
}

// executor runs one claimed batch from its first line to its result files.
type executor struct {
	store         Store
	files         FileStore
	dispatcher    Dispatcher
	now           func() time.Time
	newID         func(prefix string) string
	concurrency   int
	maxLine       int64
	progressEvery int
}

func newExecutor(store Store, files FileStore, dispatcher Dispatcher) *executor {
	return &executor{store: store, files: files, dispatcher: dispatcher, now: time.Now, newID: newID, concurrency: defaultConcurrency, maxLine: defaultMaxLineBytes, progressEvery: progressEvery}
}

// run answers every line of the batch and publishes the result files; a run that stops early publishes nothing.
func (e *executor) run(ctx context.Context, lease *Lease) error {
	job, err := e.store.GetJob(ctx, lease.JobID)
	if err != nil {
		return err
	}
	total, err := e.count(ctx, job)
	if err != nil {
		return err
	}
	if err := e.store.Progress(ctx, lease, total, 0, 0); err != nil {
		return err
	}
	out, err := e.newResults(ctx, job)
	if err != nil {
		return err
	}
	defer out.abort()
	if err := e.answer(ctx, lease, job, total, out); err != nil {
		return err
	}
	return e.publish(ctx, lease, job, total, out)
}

// count reads the input once, so the batch shows its total before the first line is sent.
func (e *executor) count(ctx context.Context, job *Job) (int64, error) {
	input, err := e.files.Open(ctx, job.Tenant, job.InputFileID)
	if err != nil {
		return 0, err
	}
	defer input.Close()
	lines := newLineReader(input, e.maxLine)
	var total int64
	for {
		if _, _, err := lines.next(); errors.Is(err, io.EOF) {
			return total, nil
		} else if err != nil {
			return 0, err
		}
		total++
		if total%1000 == 0 && ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
}

// answer sends the lines with at most concurrency in flight, and writes each answer as it arrives.
func (e *executor) answer(ctx context.Context, lease *Lease, job *Job, total int64, out *results) error {
	input, err := e.files.Open(ctx, job.Tenant, job.InputFileID)
	if err != nil {
		return err
	}
	defer input.Close()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	answered := make(chan lineResult, e.concurrency)
	readErr := make(chan error, 1)
	go e.send(runCtx, job, newLineReader(input, e.maxLine), answered, readErr)

	var failure error
	since, last := 0, e.now()
	for result := range answered {
		// After a stop the remaining answers are dropped: nothing of this run is published.
		if failure != nil || runCtx.Err() != nil {
			continue
		}
		if failure = out.write(result); failure != nil {
			stop()
			continue
		}
		since++
		if since >= e.progressEvery || e.now().Sub(last) >= progressAfter {
			if err := e.store.Progress(ctx, lease, total, out.completed, out.failed); errors.Is(err, ErrNotOwner) {
				failure = err
				stop()
				continue
			} else if err != nil {
				// The counts are only for the client to watch; publishing writes the final ones.
				klog.Errorf("batch: cannot update the counts of %s: %v", job.ID, err)
			}
			since, last = 0, e.now()
		}
	}
	if failure != nil {
		return failure
	}
	if err := <-readErr; err != nil {
		return err
	}
	return ctx.Err()
}

// send reads the lines and answers each in its own goroutine, closing answered once every one has reported.
func (e *executor) send(ctx context.Context, job *Job, lines *lineReader, answered chan<- lineResult, readErr chan<- error) {
	var sending sync.WaitGroup
	defer func() {
		sending.Wait()
		close(answered)
	}()
	slots := make(chan struct{}, e.concurrency)
	for {
		line, tooLong, err := lines.next()
		if errors.Is(err, io.EOF) {
			readErr <- nil
			return
		}
		if err != nil {
			readErr <- err
			return
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			readErr <- ctx.Err()
			return
		}
		sending.Add(1)
		go func() {
			defer sending.Done()
			defer func() { <-slots }()
			answered <- e.line(ctx, job, line, tooLong)
		}()
	}
}

// lineResult is one line of the output or the error file.
type lineResult struct {
	record []byte
	failed bool
}

// resultLine is the OpenAI shape of one result line.
type resultLine struct {
	ID       string        `json:"id"`
	CustomID any           `json:"custom_id"`
	Response *lineResponse `json:"response"`
	Error    *lineError    `json:"error"`
}

type lineResponse struct {
	StatusCode int             `json:"status_code"`
	RequestID  string          `json:"request_id"`
	Body       json.RawMessage `json:"body"`
}

type lineError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// line sends one line, or explains why it was not sent, and shapes the result line.
func (e *executor) line(ctx context.Context, job *Job, raw []byte, tooLong bool) lineResult {
	result := resultLine{ID: e.newID("batch_req_")}
	if tooLong {
		result.Error = &lineError{Code: "line_too_long", Message: "the line is longer than " + strconv.FormatInt(e.maxLine, 10) + " bytes"}
		return encodeResult(result, true)
	}
	body, customID, problem := parseLine(raw, job.Endpoint)
	result.CustomID = orNull(customID)
	if problem != "" {
		result.Error = &lineError{Code: "invalid_request", Message: problem}
		return encodeResult(result, true)
	}
	response := e.dispatcher.Dispatch(ctx, &Request{JobID: job.ID, RequestID: result.ID, Endpoint: job.Endpoint, Tenant: job.Tenant, Subject: job.Subject, Origin: job.Origin, Body: body})
	switch {
	case response == nil:
		result.Error = &lineError{Code: "request_failed", Message: "the request got no answer"}
	case errors.Is(response.Err, ErrResponseTooLarge):
		result.Error = &lineError{Code: "response_too_large", Message: "the response is larger than a batch line may keep"}
	case response.Err != nil:
		result.Error = &lineError{Code: "request_failed", Message: response.Err.Error()}
	case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && !json.Valid(response.Body):
		result.Error = &lineError{Code: "invalid_response", Message: "the answer is cut off or is not JSON"}
	default:
		result.Response = &lineResponse{StatusCode: response.StatusCode, RequestID: result.ID, Body: jsonOrString(response.Body)}
		return encodeResult(result, response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices)
	}
	return encodeResult(result, true)
}

func encodeResult(result resultLine, failed bool) lineResult {
	record, err := json.Marshal(result)
	if err != nil {
		record, _ = json.Marshal(resultLine{ID: result.ID, CustomID: result.CustomID, Error: &lineError{Code: "request_failed", Message: "the result cannot be written as JSON"}})
		failed = true
	}
	return lineResult{record: append(record, '\n'), failed: failed}
}

// jsonOrString keeps a JSON body as it is and turns anything else into a JSON string.
func jsonOrString(body []byte) json.RawMessage {
	if json.Valid(body) {
		return body
	}
	quoted, _ := json.Marshal(string(body))
	return quoted
}

type inputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// parseLine returns the body to send and the custom_id, or what keeps the line from being sent.
func parseLine(raw []byte, endpoint string) ([]byte, string, string) {
	var line inputLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil, "", "the line is not a JSON object with custom_id, method, url and body"
	}
	body := bytes.TrimSpace(line.Body)
	var options struct {
		Stream bool `json:"stream"`
	}
	switch {
	case line.CustomID == "":
		return nil, "", "custom_id is required"
	case line.Method != http.MethodPost:
		return nil, line.CustomID, "method must be POST"
	case line.URL != endpoint:
		return nil, line.CustomID, "url must be " + endpoint + ", the endpoint of the batch"
	case len(body) == 0 || body[0] != '{':
		return nil, line.CustomID, "body must be a JSON object"
	case json.Unmarshal(body, &options) != nil || options.Stream:
		return nil, line.CustomID, "a batch request cannot stream; leave stream out or set it to false"
	}
	return body, line.CustomID, ""
}

// lineReader yields the non-blank lines of a file, never holding more than max bytes of one.
type lineReader struct {
	r     *bufio.Reader
	max   int64
	first bool
}

func newLineReader(r io.Reader, max int64) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64<<10), max: max, first: true}
}

// next returns the next line without its line break, or reports that it was too long; io.EOF ends the file.
func (l *lineReader) next() ([]byte, bool, error) {
	for {
		var line []byte
		tooLong := false
		var err error
		for {
			var chunk []byte
			chunk, err = l.r.ReadSlice('\n')
			if !tooLong {
				// Room for the line break, and on the first line for a byte order mark, which are trimmed below.
				if int64(len(line)+len(chunk)) > l.max+2+int64(boms(l.first)) {
					tooLong, line = true, nil
				} else {
					line = append(line, chunk...)
				}
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		if l.first {
			l.first = false
			line = bytes.TrimPrefix(line, []byte(byteOrderMark))
		}
		line = bytes.TrimRight(line, "\r\n")
		if !tooLong && int64(len(line)) > l.max {
			tooLong, line = true, nil
		}
		if tooLong || len(bytes.TrimSpace(line)) > 0 {
			return line, tooLong, nil
		}
		if err != nil {
			return nil, false, io.EOF
		}
	}
}

// results are the output and error files of one run, under IDs no other run uses.
type results struct {
	output, errors    FileWriter
	outputID, errorID string
	completed, failed int64
	published         bool
}

func (e *executor) newResults(ctx context.Context, job *Job) (*results, error) {
	out := &results{outputID: e.newID("file-"), errorID: e.newID("file-")}
	var err error
	if out.output, err = e.files.NewWriter(ctx, job.Tenant, out.outputID); err != nil {
		return nil, err
	}
	if out.errors, err = e.files.NewWriter(ctx, job.Tenant, out.errorID); err != nil {
		out.output.Abort()
		return nil, err
	}
	return out, nil
}

func (r *results) write(result lineResult) error {
	target := r.output
	if result.failed {
		target = r.errors
	}
	if _, err := target.Write(result.record); err != nil {
		return err
	}
	if result.failed {
		r.failed++
	} else {
		r.completed++
	}
	return nil
}

// abort throws away both files unless they were published.
func (r *results) abort() {
	if !r.published {
		r.output.Abort()
		r.errors.Abort()
	}
}

// resultPart is one result file of a run on its way to being published.
type resultPart struct {
	lines  int64
	writer FileWriter
	id     string
	field  string
	suffix string
	size   int64
}

// publish makes the result files durable, records them, and moves the batch to completed in one fenced step.
func (e *executor) publish(ctx context.Context, lease *Lease, job *Job, total int64, out *results) error {
	// The answers are paid for, so a shutdown that starts now does not throw them away.
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	fields := map[string]string{"total": strconv.FormatInt(total, 10), "completed": strconv.FormatInt(out.completed, 10), "failed": strconv.FormatInt(out.failed, 10)}
	var parts []resultPart
	for _, part := range []resultPart{
		{lines: out.completed, writer: out.output, id: out.outputID, field: "output_file_id", suffix: "output"},
		{lines: out.failed, writer: out.errors, id: out.errorID, field: "error_file_id", suffix: "error"},
	} {
		if part.lines == 0 {
			part.writer.Abort()
			continue
		}
		parts = append(parts, part)
	}
	// Every file is durable before any is recorded, so a failure here leaves nothing a client can list.
	for i := range parts {
		size, err := parts[i].writer.Commit()
		if err != nil {
			e.forget(finish, job, parts[:i], nil)
			return err
		}
		parts[i].size = size
	}
	out.published = true
	for i, part := range parts {
		now := e.now()
		record := &File{ID: part.id, Tenant: job.Tenant, Filename: job.ID + "_" + part.suffix + ".jsonl", Purpose: PurposeBatchOutput, Status: FileProcessed, Bytes: part.size, CreatedAt: now.Unix(), ExpiresAt: now.Add(resultExpiration).Unix()}
		if err := e.store.CreateFile(finish, record); err != nil {
			e.forget(finish, job, parts[i+1:], parts[:i])
			e.settle(finish, job, part, err)
			return fmt.Errorf("batch: cannot record the %s file of %s: %w", part.suffix, job.ID, err)
		}
		fields[part.field] = part.id
	}
	err := e.store.Transition(finish, lease, StatusInProgress, StatusCompleted, fields)
	if err == nil {
		return nil
	}
	// The step may have landed with only its answer lost; a batch finished under this fence is published.
	if current, getErr := e.store.GetJob(finish, job.ID); getErr == nil && current.Status == StatusCompleted && current.Fence == lease.Fence {
		return nil
	}
	return err
}

// forget removes the result files of a run that is not published: no batch points at them yet.
func (e *executor) forget(ctx context.Context, job *Job, unrecorded, recorded []resultPart) {
	for _, part := range recorded {
		if _, err := e.store.DeleteFile(ctx, part.id); err != nil {
			klog.Errorf("batch: cannot take back the result file %s of %s: %v", part.id, job.ID, err)
			continue
		}
		unrecorded = append(unrecorded, part)
	}
	for _, part := range unrecorded {
		if err := e.files.Remove(ctx, job.Tenant, part.id); err != nil && !errors.Is(err, ErrNotFound) {
			klog.Errorf("batch: removing the result bytes of %s: %v", part.id, err)
		}
	}
}

// settle handles a result record that failed to write: a sure refusal frees the bytes, and a record that may still land keeps them.
func (e *executor) settle(ctx context.Context, job *Job, part resultPart, err error) {
	if errors.Is(err, ErrRefused) {
		e.forget(ctx, job, []resultPart{part}, nil)
		return
	}
	if _, lookupErr := e.store.GetFile(ctx, part.id); lookupErr == nil {
		e.forget(ctx, job, nil, []resultPart{part})
		return
	}
	klog.Errorf("batch: the record of %s may still be written, so its bytes are kept: %v", part.id, err)
}
