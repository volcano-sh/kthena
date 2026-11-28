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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeModel answers every line it is sent and remembers them.
type fakeModel struct {
	mu       sync.Mutex
	requests []*Request
	answer   func(ctx context.Context, request *Request) *Response
}

func (m *fakeModel) Dispatch(ctx context.Context, request *Request) *Response {
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if m.answer != nil {
		return m.answer(ctx, request)
	}
	return &Response{StatusCode: http.StatusOK, Body: []byte(`{"echo":` + string(request.Body) + `}`)}
}

func (m *fakeModel) sent() []*Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*Request(nil), m.requests...)
}

type runFixture struct {
	mini  *miniredis.Miniredis
	store *RedisStore
	files *FSStore
	root  string
	exec  *executor
	model *fakeModel
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	mini, store := newTestStore(t)
	files, root := newTestFileStore(t)
	model := &fakeModel{}
	exec := newExecutor(store, files, model)
	exec.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	return &runFixture{mini: mini, store: store, files: files, root: root, exec: exec, model: model}
}

// claimBatch uploads input as alice's file, creates a batch on it and claims it.
func (f *runFixture) claimBatch(t *testing.T, input string) (*Job, *Lease) {
	t.Helper()
	ctx := context.Background()
	tenant := tenantID("", "alice")
	written, err := f.files.Create(ctx, tenant, "file-input", strings.NewReader(input), 0)
	require.NoError(t, err)
	require.NoError(t, f.store.CreateFile(ctx, &File{ID: "file-input", Tenant: tenant, Purpose: PurposeBatch, Status: FileProcessed, Bytes: written, CreatedAt: 1000}))
	job := newJob("batch_1", tenant, "file-input")
	job.Origin = Origin{Host: "api.example.com", Gateway: "default/gw", Listener: "http", Port: 80}
	require.NoError(t, f.store.CreateJob(ctx, job))
	lease, err := f.store.Claim(ctx, "pod-a", newFence())
	require.NoError(t, err)
	require.NotNil(t, lease)
	return job, lease
}

func (f *runFixture) job(t *testing.T) *Job {
	t.Helper()
	job, err := f.store.GetJob(context.Background(), "batch_1")
	require.NoError(t, err)
	return job
}

// resultLines reads a published result file as JSON objects.
func (f *runFixture) resultLines(t *testing.T, id string) []map[string]any {
	t.Helper()
	if id == "" {
		return nil
	}
	file, err := f.files.Open(context.Background(), tenantID("", "alice"), id)
	require.NoError(t, err)
	defer file.Close()
	var lines []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := map[string]any{}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), scanner.Text())
		lines = append(lines, line)
	}
	require.NoError(t, scanner.Err())
	return lines
}

func chat(customID string) string {
	return `{"custom_id":"` + customID + `","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"` + customID + `"}]}}` + "\n"
}

func TestRunAnswersEveryLineExactlyOnce(t *testing.T) {
	f := newRunFixture(t)
	f.model.answer = func(_ context.Context, request *Request) *Response {
		if strings.Contains(string(request.Body), `"bad"`) {
			return &Response{StatusCode: http.StatusBadRequest, Body: []byte(`{"error":{"message":"no"}}`)}
		}
		return &Response{StatusCode: http.StatusOK, Body: []byte(`{"id":"chatcmpl-1"}`)}
	}
	job, lease := f.claimBatch(t, chat("a")+chat("bad")+chat("c"))
	require.NoError(t, f.exec.run(context.Background(), lease))

	done := f.job(t)
	assert.Equal(t, StatusCompleted, done.Status)
	assert.Equal(t, []int64{3, 2, 1}, []int64{done.Total, done.Completed, done.Failed})
	assert.NotZero(t, done.FinalizingAt)
	assert.NotZero(t, done.CompletedAt)
	output := f.resultLines(t, done.OutputFileID)
	errorLines := f.resultLines(t, done.ErrorFileID)
	require.Len(t, output, 2)
	require.Len(t, errorLines, 1)
	seen := map[any]int{}
	for _, line := range append(output, errorLines...) {
		seen[line["custom_id"]]++
	}
	assert.Equal(t, map[any]int{"a": 1, "bad": 1, "c": 1}, seen, "every custom_id appears once across both files")

	answered := output[0]
	assert.Nil(t, answered["error"])
	response := answered["response"].(map[string]any)
	assert.Equal(t, float64(200), response["status_code"])
	assert.Equal(t, answered["id"], response["request_id"])
	assert.True(t, strings.HasPrefix(answered["id"].(string), "batch_req_"))
	assert.Equal(t, map[string]any{"id": "chatcmpl-1"}, response["body"])
	refused := errorLines[0]
	assert.Nil(t, refused["error"], "a line the model answered keeps that answer, even in the error file")
	assert.Equal(t, float64(400), refused["response"].(map[string]any)["status_code"])

	for _, id := range []string{done.OutputFileID, done.ErrorFileID} {
		record, err := f.store.GetFile(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, PurposeBatchOutput, record.Purpose)
		assert.Equal(t, job.Tenant, record.Tenant)
		assert.Equal(t, FileProcessed, record.Status)
		assert.Equal(t, int64(1_800_000_000+30*24*3600), record.ExpiresAt, "result files expire after 30 days")
		assert.True(t, strings.HasPrefix(id, "file-"), id)
	}
	assert.False(t, f.mini.Exists(f.store.fileRefsKey("file-input")), "a finished batch lets go of its input")
}

func TestRunSendsEachLineAsTheBatchOwner(t *testing.T) {
	f := newRunFixture(t)
	job, lease := f.claimBatch(t, chat("a"))
	require.NoError(t, f.exec.run(context.Background(), lease))

	sent := f.model.sent()
	require.Len(t, sent, 1)
	request := sent[0]
	assert.Equal(t, "batch_1", request.JobID)
	assert.Equal(t, chatCompletions, request.Endpoint)
	assert.Equal(t, job.Tenant, request.Tenant)
	assert.Equal(t, "user-"+job.Tenant, request.Subject)
	assert.Equal(t, job.Origin, request.Origin)
	assert.JSONEq(t, `{"model":"m","messages":[{"role":"user","content":"a"}]}`, string(request.Body), "the body is sent as the line wrote it")
	assert.True(t, strings.HasPrefix(request.RequestID, "batch_req_"))
	assert.Equal(t, request.RequestID, f.resultLines(t, f.job(t).OutputFileID)[0]["id"], "the result line names the request that was sent")
}

func TestRunTurnsBadLinesIntoErrorLines(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		customID any
		code     string
		message  string
	}{
		{"not JSON", "not json", nil, "invalid_request", "not a JSON object"},
		{"an array", `[1,2]`, nil, "invalid_request", "not a JSON object"},
		{"no custom_id", `{"method":"POST","url":"/v1/chat/completions","body":{}}`, nil, "invalid_request", "custom_id is required"},
		{"another method", `{"custom_id":"x","method":"GET","url":"/v1/chat/completions","body":{}}`, "x", "invalid_request", "method must be POST"},
		{"another url", `{"custom_id":"x","method":"POST","url":"/v1/embeddings","body":{}}`, "x", "invalid_request", "url must be /v1/chat/completions"},
		{"a body that is not an object", `{"custom_id":"x","method":"POST","url":"/v1/chat/completions","body":[1]}`, "x", "invalid_request", "body must be a JSON object"},
		{"no body", `{"custom_id":"x","method":"POST","url":"/v1/chat/completions"}`, "x", "invalid_request", "body must be a JSON object"},
		{"streaming", `{"custom_id":"x","method":"POST","url":"/v1/chat/completions","body":{"stream":true}}`, "x", "invalid_request", "cannot stream"},
		{"too long", `{"custom_id":"x","method":"POST","url":"/v1/chat/completions","body":{"p":"` + strings.Repeat("a", 200) + `"}}`, nil, "line_too_long", "longer than 150 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.exec.maxLine = 150
			_, lease := f.claimBatch(t, tc.line+"\n"+chat("ok"))
			require.NoError(t, f.exec.run(context.Background(), lease))

			done := f.job(t)
			assert.Equal(t, []int64{2, 1, 1}, []int64{done.Total, done.Completed, done.Failed})
			errorLines := f.resultLines(t, done.ErrorFileID)
			require.Len(t, errorLines, 1)
			assert.Equal(t, tc.customID, errorLines[0]["custom_id"])
			assert.Nil(t, errorLines[0]["response"], "a bad line is never sent")
			failure := errorLines[0]["error"].(map[string]any)
			assert.Equal(t, tc.code, failure["code"])
			assert.Contains(t, failure["message"], tc.message)
			assert.Len(t, f.model.sent(), 1, "only the good line reached the model")
		})
	}
}

func TestRunStreamFalseIsFine(t *testing.T) {
	f := newRunFixture(t)
	_, lease := f.claimBatch(t, `{"custom_id":"x","method":"POST","url":"/v1/chat/completions","body":{"stream":false}}`+"\n")
	require.NoError(t, f.exec.run(context.Background(), lease))
	assert.Equal(t, int64(1), f.job(t).Completed)
}

func TestRunSkipsBlankLinesAndAByteOrderMark(t *testing.T) {
	f := newRunFixture(t)
	input := "\xEF\xBB\xBF" + strings.TrimSuffix(chat("a"), "\n") + "\r\n\n   \r\n" + strings.TrimSuffix(chat("b"), "\n")
	_, lease := f.claimBatch(t, input)
	require.NoError(t, f.exec.run(context.Background(), lease))

	done := f.job(t)
	assert.Equal(t, []int64{2, 2, 0}, []int64{done.Total, done.Completed, done.Failed}, "a last line without a newline still counts")
	assert.Empty(t, done.ErrorFileID, "no error file when nothing failed")
	entries, err := os.ReadDir(filepath.Join(f.root, tenantID("", "alice")))
	require.NoError(t, err)
	assert.Len(t, entries, 2, "the input and one result file; the empty error file was thrown away")
}

func TestRunMarksAnswersThatCannotBeKept(t *testing.T) {
	f := newRunFixture(t)
	f.model.answer = func(_ context.Context, request *Request) *Response {
		switch {
		case strings.Contains(string(request.Body), `"big"`):
			return &Response{StatusCode: http.StatusOK, Err: ErrResponseTooLarge}
		case strings.Contains(string(request.Body), `"lost"`):
			return &Response{Err: errors.New("connection reset")}
		case strings.Contains(string(request.Body), `"nil"`):
			return nil
		}
		return &Response{StatusCode: http.StatusBadGateway, Body: []byte("upstream says no")}
	}
	_, lease := f.claimBatch(t, chat("big")+chat("lost")+chat("nil")+chat("text"))
	require.NoError(t, f.exec.run(context.Background(), lease))

	byID := map[any]map[string]any{}
	for _, line := range f.resultLines(t, f.job(t).ErrorFileID) {
		byID[line["custom_id"]] = line
	}
	require.Len(t, byID, 4)
	assert.Equal(t, "response_too_large", byID["big"]["error"].(map[string]any)["code"])
	assert.Equal(t, "request_failed", byID["lost"]["error"].(map[string]any)["code"])
	assert.Contains(t, byID["lost"]["error"].(map[string]any)["message"], "connection reset")
	assert.Equal(t, "request_failed", byID["nil"]["error"].(map[string]any)["code"])
	assert.Equal(t, "upstream says no", byID["text"]["response"].(map[string]any)["body"], "a body that is not JSON is kept as a string")
}

func TestRunKeepsAtMostConcurrencyLinesInFlight(t *testing.T) {
	f := newRunFixture(t)
	f.exec.concurrency = 3
	var inFlight, most atomic.Int32
	f.model.answer = func(context.Context, *Request) *Response {
		now := inFlight.Add(1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		inFlight.Add(-1)
		return &Response{StatusCode: http.StatusOK, Body: []byte(`{}`)}
	}
	var input strings.Builder
	for i := 0; i < 40; i++ {
		input.WriteString(chat(fmt.Sprintf("q%d", i)))
	}
	_, lease := f.claimBatch(t, input.String())
	require.NoError(t, f.exec.run(context.Background(), lease))
	assert.Equal(t, int64(40), f.job(t).Completed)
	assert.LessOrEqual(t, most.Load(), int32(3))
	assert.Greater(t, most.Load(), int32(1), "lines are sent side by side")
}

func TestRunShowsProgressWhileItRuns(t *testing.T) {
	f := newRunFixture(t)
	f.exec.progressEvery = 2
	f.exec.concurrency = 1
	reached := make(chan struct{})
	release := make(chan struct{})
	f.model.answer = func(_ context.Context, request *Request) *Response {
		if strings.Contains(string(request.Body), `"q4"`) {
			close(reached)
			<-release
		}
		return &Response{StatusCode: http.StatusOK, Body: []byte(`{}`)}
	}
	_, lease := f.claimBatch(t, chat("q0")+chat("q1")+chat("q2")+chat("q3")+chat("q4")+chat("q5"))
	finished := make(chan error, 1)
	go func() { finished <- f.exec.run(context.Background(), lease) }()
	<-reached
	// The fifth line is held, so the counts stop at four lines done.
	require.Eventually(t, func() bool { return f.job(t).Completed == 4 }, 5*time.Second, 5*time.Millisecond, "the counts move while the batch runs")
	running := f.job(t)
	close(release)
	require.NoError(t, <-finished)
	assert.Equal(t, StatusInProgress, running.Status)
	assert.Equal(t, int64(6), running.Total, "the total is known before the first line runs")
}

func TestRunThatIsStoppedPublishesNothing(t *testing.T) {
	f := newRunFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.model.answer = func(ctx context.Context, request *Request) *Response {
		if strings.Contains(string(request.Body), `"q2"`) {
			cancel()
		}
		<-ctx.Done()
		return &Response{Err: ctx.Err()}
	}
	_, lease := f.claimBatch(t, chat("q1")+chat("q2")+chat("q3"))
	err := f.exec.run(ctx, lease)
	require.ErrorIs(t, err, context.Canceled)

	job := f.job(t)
	assert.Equal(t, StatusInProgress, job.Status, "the worker hands the batch back")
	assert.Empty(t, job.OutputFileID)
	assert.Empty(t, job.ErrorFileID)
	entries, err := os.ReadDir(filepath.Join(f.root, tenantID("", "alice")))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the input is left: no result and no temporary file")
}

// brokenVolume cannot start a new file.
type brokenVolume struct{ FileStore }

func (brokenVolume) NewWriter(context.Context, string, string) (FileWriter, error) {
	return nil, errors.New("read-only file system")
}

func TestRunFailsWhenTheVolumeCannotTakeResults(t *testing.T) {
	f := newRunFixture(t)
	_, lease := f.claimBatch(t, chat("a"))
	f.exec.files = brokenVolume{f.files}
	require.Error(t, f.exec.run(context.Background(), lease))
	assert.Equal(t, StatusInProgress, f.job(t).Status)
	assert.Empty(t, f.model.sent(), "nothing is sent when its answer cannot be kept")
}

func TestRunByARunThatLostTheBatchChangesNothing(t *testing.T) {
	f := newRunFixture(t)
	_, lease := f.claimBatch(t, chat("a"))
	stale := &Lease{JobID: lease.JobID, Holder: "pod-b", Fence: "0123"}
	require.ErrorIs(t, f.exec.run(context.Background(), stale), ErrNotOwner)
	assert.Empty(t, f.model.sent())
	assert.Equal(t, int64(0), f.job(t).Total)
}

func TestRunLineLimitIsExact(t *testing.T) {
	line := strings.TrimSuffix(chat("edge"), "\n")
	cases := []struct {
		name  string
		limit int64
		ends  string
		fits  bool
	}{
		{"at the limit with CRLF", int64(len(line)), "\r\n", true},
		{"at the limit without a line break", int64(len(line)), "", true},
		{"one byte over with LF", int64(len(line)) - 1, "\n", false},
		{"one byte over with CRLF", int64(len(line)) - 1, "\r\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.exec.maxLine = tc.limit
			_, lease := f.claimBatch(t, line+tc.ends)
			require.NoError(t, f.exec.run(context.Background(), lease))
			done := f.job(t)
			if tc.fits {
				assert.Equal(t, int64(1), done.Completed, "the line break does not count toward the limit")
				return
			}
			require.Equal(t, int64(1), done.Failed)
			assert.Equal(t, "line_too_long", f.resultLines(t, done.ErrorFileID)[0]["error"].(map[string]any)["code"])
		})
	}
}

// shutdownOnPublish starts the shutdown the moment the first result file is recorded.
type shutdownOnPublish struct {
	Store
	stop context.CancelFunc
}

func (s shutdownOnPublish) CreateFile(ctx context.Context, file *File) error {
	s.stop()
	return s.Store.CreateFile(ctx, file)
}

func TestAShutdownWhilePublishingStillFinishesTheBatch(t *testing.T) {
	f := newRunFixture(t)
	_, lease := f.claimBatch(t, chat("a"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.exec.store = shutdownOnPublish{Store: f.store, stop: cancel}
	require.NoError(t, f.exec.run(ctx, lease))
	done := f.job(t)
	assert.Equal(t, StatusCompleted, done.Status, "answers that are already paid for are published, not thrown away")
	assert.Len(t, f.resultLines(t, done.OutputFileID), 1)
}

// stepStore counts transitions and can fail the final one, after or instead of running it.
type stepStore struct {
	Store
	steps        atomic.Int32
	failFinal    bool
	landThenFail bool
}

func (s *stepStore) Transition(ctx context.Context, lease *Lease, from, to Status, fields map[string]string) error {
	s.steps.Add(1)
	if to == StatusCompleted && s.landThenFail {
		_ = s.Store.Transition(ctx, lease, from, to, fields)
		return errors.New("i/o timeout")
	}
	if to == StatusCompleted && s.failFinal {
		return errors.New("i/o timeout")
	}
	return s.Store.Transition(ctx, lease, from, to, fields)
}

func TestPublishFinishesTheBatchInOneStep(t *testing.T) {
	f := newRunFixture(t)
	steps := &stepStore{Store: f.store}
	f.exec.store = steps
	_, lease := f.claimBatch(t, chat("a"))
	require.NoError(t, f.exec.run(context.Background(), lease))
	done := f.job(t)
	assert.Equal(t, StatusCompleted, done.Status)
	assert.Equal(t, int32(1), steps.steps.Load(), "no status in between that nothing could finish")
	assert.Equal(t, done.CompletedAt, done.FinalizingAt)
}

func TestAPublishWhoseLastStepFailsCanBeHandedBack(t *testing.T) {
	f := newRunFixture(t)
	f.exec.store = &stepStore{Store: f.store, failFinal: true}
	_, lease := f.claimBatch(t, chat("a"))
	require.Error(t, f.exec.run(context.Background(), lease))
	assert.Equal(t, StatusInProgress, f.job(t).Status)
	assert.NoError(t, f.store.Release(context.Background(), lease), "the worker can hand it back")
}

func TestAFinalStepWhoseAnswerWasLostStillCounts(t *testing.T) {
	f := newRunFixture(t)
	f.exec.store = &stepStore{Store: f.store, landThenFail: true}
	_, lease := f.claimBatch(t, chat("a"))
	require.NoError(t, f.exec.run(context.Background(), lease), "a batch finished under this fence is published")
	assert.Equal(t, StatusCompleted, f.job(t).Status)
}

func TestRunFailsALineWhoseAnswerIsNotJSON(t *testing.T) {
	f := newRunFixture(t)
	f.model.answer = func(context.Context, *Request) *Response {
		return &Response{StatusCode: http.StatusOK, Body: []byte(`{"id":"chatcmpl-1","choi`)}
	}
	_, lease := f.claimBatch(t, chat("a"))
	require.NoError(t, f.exec.run(context.Background(), lease))
	done := f.job(t)
	assert.Equal(t, []int64{1, 0, 1}, []int64{done.Total, done.Completed, done.Failed}, "a cut-off answer is not a success")
	failed := f.resultLines(t, done.ErrorFileID)
	require.Len(t, failed, 1)
	assert.Equal(t, "invalid_response", failed[0]["error"].(map[string]any)["code"])
}

// flakyCounts fails every counts update after the first, which records the total.
type flakyCounts struct {
	Store
	calls atomic.Int32
}

func (s *flakyCounts) Progress(ctx context.Context, lease *Lease, total, completed, failed int64) error {
	if s.calls.Add(1) > 1 {
		return errors.New("i/o timeout")
	}
	return s.Store.Progress(ctx, lease, total, completed, failed)
}

func TestRunKeepsGoingWhenTheCountsCannotBeWritten(t *testing.T) {
	f := newRunFixture(t)
	counts := &flakyCounts{Store: f.store}
	f.exec.store = counts
	f.exec.progressEvery = 1
	_, lease := f.claimBatch(t, chat("a")+chat("b")+chat("c"))
	require.NoError(t, f.exec.run(context.Background(), lease))
	assert.Greater(t, counts.calls.Load(), int32(1), "the counts were tried while running")
	done := f.job(t)
	assert.Equal(t, StatusCompleted, done.Status, "the answers are kept and published")
	assert.Equal(t, int64(3), done.Completed, "publishing writes the final counts")
}

// refusedRecords lets the first result record through and answers the second with err, writing nothing.
type refusedRecords struct {
	Store
	calls atomic.Int32
	err   error
}

func (s *refusedRecords) CreateFile(ctx context.Context, file *File) error {
	if file.Purpose == PurposeBatchOutput && s.calls.Add(1) == 2 {
		return s.err
	}
	return s.Store.CreateFile(ctx, file)
}

func (f *runFixture) listedResults(t *testing.T) []string {
	t.Helper()
	files, _, err := f.store.ListFiles(context.Background(), FileQuery{Tenant: tenantID("", "alice"), Purpose: PurposeBatchOutput, Limit: 10})
	require.NoError(t, err)
	return idsOf(files)
}

func (f *runFixture) storedBytes(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.root, tenantID("", "alice")))
	require.NoError(t, err)
	return len(entries)
}

func TestAFailedPublishLeavesNothingListed(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		bytesLeft  int
		becauseMsg string
	}{
		{"a refused record", fmt.Errorf("%w: OOM command not allowed", ErrRefused), 1, "only the input is left"},
		{"a record that may still land", errors.New("i/o timeout"), 2, "the bytes a late record could point at are kept"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.exec.store = &refusedRecords{Store: f.store, err: tc.err}
			f.model.answer = func(_ context.Context, request *Request) *Response {
				if strings.Contains(string(request.Body), `"bad"`) {
					return &Response{StatusCode: http.StatusBadRequest, Body: []byte(`{}`)}
				}
				return &Response{StatusCode: http.StatusOK, Body: []byte(`{}`)}
			}
			_, lease := f.claimBatch(t, chat("ok")+chat("bad"))
			require.Error(t, f.exec.run(context.Background(), lease))
			assert.Empty(t, f.listedResults(t), "no result file a client can list")
			assert.Equal(t, tc.bytesLeft, f.storedBytes(t), tc.becauseMsg)
			assert.Equal(t, StatusInProgress, f.job(t).Status)
		})
	}
}

// secondCommitFails makes the second result file of a run fail to commit.
type secondCommitFails struct {
	FileStore
	writers atomic.Int32
}

type failingCommit struct{ FileWriter }

func (w failingCommit) Commit() (int64, error) {
	w.FileWriter.Abort()
	return 0, errors.New("no space left on device")
}

func (v *secondCommitFails) NewWriter(ctx context.Context, tenant, id string) (FileWriter, error) {
	writer, err := v.FileStore.NewWriter(ctx, tenant, id)
	if err == nil && v.writers.Add(1) == 2 {
		return failingCommit{writer}, nil
	}
	return writer, err
}

func TestAFailedCommitRemovesTheOtherResultFile(t *testing.T) {
	f := newRunFixture(t)
	f.exec.files = &secondCommitFails{FileStore: f.files}
	f.model.answer = func(_ context.Context, request *Request) *Response {
		if strings.Contains(string(request.Body), `"bad"`) {
			return &Response{StatusCode: http.StatusBadRequest, Body: []byte(`{}`)}
		}
		return &Response{StatusCode: http.StatusOK, Body: []byte(`{}`)}
	}
	_, lease := f.claimBatch(t, chat("ok")+chat("bad"))
	require.Error(t, f.exec.run(context.Background(), lease))
	assert.Empty(t, f.listedResults(t))
	assert.Equal(t, 1, f.storedBytes(t), "the output file already committed is removed too")
}

func TestRunAcceptsAByteOrderMarkBeforeAFirstLineAtTheLimit(t *testing.T) {
	f := newRunFixture(t)
	line := strings.TrimSuffix(chat("edge"), "\n")
	f.exec.maxLine = int64(len(line))
	_, lease := f.claimBatch(t, "\xEF\xBB\xBF"+line+"\n"+line+"\n")
	require.NoError(t, f.exec.run(context.Background(), lease))
	assert.Equal(t, int64(2), f.job(t).Completed, "the mark does not count toward the first line")
}
