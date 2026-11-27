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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const eventually = 10 * time.Second

func newWorkerService(store Store, files FileStore, model Dispatcher, holder string) *Service {
	service := NewService(store, files, Config{Dispatcher: model, Holder: holder})
	service.retryEvery = 5 * time.Millisecond
	if service.worker != nil {
		service.worker.poll = 5 * time.Millisecond
	}
	return service
}

// startService runs the service for the rest of the test and checks that it stops when told.
func startService(t *testing.T, service *Service) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := service.Start(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(eventually):
			t.Error("the batch service did not stop")
		}
	})
	return cancel, done
}

// queueBatch stores input as alice's file and creates batch_<name> on it.
func queueBatch(t *testing.T, store *RedisStore, files *FSStore, name, input string) string {
	t.Helper()
	ctx := context.Background()
	tenant := tenantID("", "alice")
	fileID := "file-in-" + name
	written, err := files.Create(ctx, tenant, fileID, strings.NewReader(input), 0)
	require.NoError(t, err)
	require.NoError(t, store.CreateFile(ctx, &File{ID: fileID, Tenant: tenant, Purpose: PurposeBatch, Status: FileProcessed, Bytes: written, CreatedAt: 1000}))
	job := newJob("batch_"+name, tenant, fileID)
	require.NoError(t, store.CreateJob(ctx, job))
	return job.ID
}

func statusOf(t *testing.T, store *RedisStore, id string) Status {
	t.Helper()
	job, err := store.GetJob(context.Background(), id)
	require.NoError(t, err)
	return job.Status
}

func TestWorkerRunsAQueuedBatch(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	model := &fakeModel{}
	id := queueBatch(t, store, files, "1", chat("a")+chat("b"))
	startService(t, newWorkerService(store, files, model, "pod-a"))

	require.Eventually(t, func() bool { return statusOf(t, store, id) == StatusCompleted }, eventually, 5*time.Millisecond)
	job, err := store.GetJob(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), job.Completed)
	assert.Equal(t, "pod-a", job.Holder, "the record names the replica that ran it")
	assert.Len(t, model.sent(), 2)
}

// countedClaims counts how often the worker looks at the queue.
type countedClaims struct {
	Store
	claims atomic.Int32
}

func (s *countedClaims) Claim(ctx context.Context, holder string) (*Lease, error) {
	s.claims.Add(1)
	return s.Store.Claim(ctx, holder)
}

func TestWorkerWaitsUntilTheStorageIsReady(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	id := queueBatch(t, store, files, "1", chat("a"))
	counted := &countedClaims{Store: store}
	service := newWorkerService(counted, files, &fakeModel{}, "pod-a")
	var checks atomic.Int32
	var mounted atomic.Bool
	service.gate.check = func(ctx context.Context) error {
		checks.Add(1)
		if !mounted.Load() {
			return errors.New("the volume is not mounted yet")
		}
		return service.checkStorage(ctx)
	}
	startService(t, service)

	require.Eventually(t, func() bool { return checks.Load() >= 3 }, eventually, time.Millisecond)
	assert.Zero(t, counted.claims.Load(), "no batch is taken while its files cannot be read or written")
	mounted.Store(true)
	require.Eventually(t, func() bool { return statusOf(t, store, id) == StatusCompleted }, eventually, 5*time.Millisecond)
}

func TestShutdownHandsTheRunningBatchBack(t *testing.T) {
	mini, store := newTestStore(t)
	files, root := newTestFileStore(t)
	reached := make(chan struct{})
	var once sync.Once
	model := &fakeModel{answer: func(ctx context.Context, _ *Request) *Response {
		once.Do(func() { close(reached) })
		<-ctx.Done()
		return &Response{Err: ctx.Err()}
	}}
	id := queueBatch(t, store, files, "1", chat("a")+chat("b"))
	stop, done := startService(t, newWorkerService(store, files, model, "pod-a"))
	<-reached
	stop()
	select {
	case <-done:
	case <-time.After(eventually):
		t.Fatal("the worker did not stop")
	}

	job, err := store.GetJob(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, job.Status)
	assert.Empty(t, job.Holder, "nobody owns the batch any more")
	score, err := mini.ZScore(store.queueKey(), id)
	require.NoError(t, err, "the batch is back in the queue")
	assert.Equal(t, float64(job.CreatedAt), score, "at its original place")
	entries, err := os.ReadDir(filepath.Join(root, tenantID("", "alice")))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the input: a stopped run leaves no result and no temporary file")
}

func TestTwoReplicasRunEachBatchOnce(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	var mu sync.Mutex
	seen := map[string]int{}
	model := &fakeModel{answer: func(_ context.Context, request *Request) *Response {
		mu.Lock()
		seen[request.JobID+" "+string(request.Body)]++
		mu.Unlock()
		return &Response{StatusCode: 200, Body: []byte(`{}`)}
	}}
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, queueBatch(t, store, files, fmt.Sprint(i), chat("a")+chat("b")+chat("c")))
	}
	startService(t, newWorkerService(store, files, model, "pod-a"))
	startService(t, newWorkerService(store, files, model, "pod-b"))

	require.Eventually(t, func() bool {
		for _, id := range ids {
			if statusOf(t, store, id) != StatusCompleted {
				return false
			}
		}
		return true
	}, eventually, 5*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, seen, 18)
	for line, n := range seen {
		assert.Equal(t, 1, n, "%s was sent %d times", line, n)
	}
}

// lostBatch says every run lost its batch to another replica, and counts the hand-backs.
type lostBatch struct {
	countedClaims
	releases atomic.Int32
}

func (s *lostBatch) Progress(context.Context, *Lease, int64, int64, int64) error {
	return ErrNotOwner
}

func (s *lostBatch) Release(ctx context.Context, lease *Lease) error {
	s.releases.Add(1)
	return s.Store.Release(ctx, lease)
}

func TestAWorkerThatLostTheBatchDoesNotHandItBack(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	queueBatch(t, store, files, "1", chat("a"))
	lost := &lostBatch{countedClaims: countedClaims{Store: store}}
	service := newWorkerService(lost, files, &fakeModel{}, "pod-a")
	service.worker.runners = 1
	startService(t, service)

	// A second claim means the first run is over, hand-back included.
	require.Eventually(t, func() bool { return lost.claims.Load() >= 2 }, eventually, time.Millisecond)
	assert.Zero(t, lost.releases.Load(), "a batch another replica owns is left to that replica")
}

// flakyVolume cannot start result files while broken is set.
type flakyVolume struct {
	FileStore
	broken atomic.Bool
	tries  atomic.Int32
}

func (v *flakyVolume) NewWriter(ctx context.Context, tenant, id string) (FileWriter, error) {
	if v.broken.Load() {
		v.tries.Add(1)
		return nil, errors.New("no space left on device")
	}
	return v.FileStore.NewWriter(ctx, tenant, id)
}

func TestAFailedRunGoesBackToTheQueue(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	id := queueBatch(t, store, files, "1", chat("a"))
	volume := &flakyVolume{FileStore: files}
	volume.broken.Store(true)
	startService(t, newWorkerService(store, volume, &fakeModel{}, "pod-a"))

	require.Eventually(t, func() bool { return volume.tries.Load() >= 2 }, eventually, time.Millisecond, "the batch is tried again after it was handed back")
	assert.Equal(t, StatusInProgress, statusOf(t, store, id))
	volume.broken.Store(false)
	require.Eventually(t, func() bool { return statusOf(t, store, id) == StatusCompleted }, eventually, 5*time.Millisecond)
}

func TestAServiceWithoutADispatcherRunsNoBatches(t *testing.T) {
	_, store := newTestStore(t)
	files, _ := newTestFileStore(t)
	id := queueBatch(t, store, files, "1", chat("a"))
	counted := &countedClaims{Store: store}
	service := NewService(counted, files, Config{})
	assert.Nil(t, service.worker)
	var checks atomic.Int32
	service.retryEvery = time.Millisecond
	service.recheckEvery = time.Millisecond
	service.gate.check = func(ctx context.Context) error {
		checks.Add(1)
		return service.checkStorage(ctx)
	}
	startService(t, service)
	require.Eventually(t, func() bool { return checks.Load() >= 3 }, eventually, time.Millisecond)
	assert.Zero(t, counted.claims.Load())
	assert.Equal(t, StatusValidating, statusOf(t, store, id))
}
