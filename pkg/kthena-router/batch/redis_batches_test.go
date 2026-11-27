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
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addInput stores a batch input file for tenant and returns its ID.
func addInput(t *testing.T, store *RedisStore, tenant string) string {
	t.Helper()
	id := "file-input-" + tenant
	require.NoError(t, store.CreateFile(context.Background(), &File{ID: id, Tenant: tenant, Purpose: PurposeBatch, CreatedAt: 1000}))
	return id
}

func newJob(id, tenant, input string) *Job {
	return &Job{
		ID:               id,
		Tenant:           tenant,
		Subject:          "user-" + tenant,
		Status:           StatusValidating,
		Endpoint:         "/v1/chat/completions",
		InputFileID:      input,
		CompletionWindow: "24h",
		CreatedAt:        2000,
		ExpiresAt:        2000 + 24*3600,
	}
}

func TestJobRecordRoundTrip(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	input := addInput(t, store, "alice")
	job := newJob("batch_1", "alice", input)
	job.Metadata = map[string]string{"team": "search", "名前": "値"}
	job.Origin = Origin{Host: "api.example.com", Gateway: "default/gw", Listener: "http", Port: 8080}
	require.NoError(t, store.CreateJob(ctx, job))

	got, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	assert.Equal(t, job, got)
	assert.Equal(t, recordVersion, mini.HGet(store.jobKey("batch_1"), "v"))
	members, err := mini.ZMembers(store.queueKey())
	require.NoError(t, err)
	assert.Equal(t, []string{"batch_1"}, members)
	score, err := mini.ZScore(store.queueKey(), "batch_1")
	require.NoError(t, err)
	assert.Equal(t, float64(2000), score, "the queue is ordered by created_at")
	holders, err := mini.Members(store.fileRefsKey(input))
	require.NoError(t, err)
	assert.Equal(t, []string{"batch_1"}, holders, "the batch holds its input file")
}

func TestGetJobOfAMissingBatch(t *testing.T) {
	_, store := newTestStore(t)
	_, err := store.GetJob(context.Background(), "batch_none")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCreateJobRefusesAnUnusableInput(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, mini *miniredis.Miniredis, store *RedisStore) string
		want  error
	}{
		{"missing", func(*testing.T, *miniredis.Miniredis, *RedisStore) string { return "file-none" }, ErrNoInputFile},
		{"another tenant's", func(t *testing.T, _ *miniredis.Miniredis, store *RedisStore) string {
			return addInput(t, store, "bob")
		}, ErrNoInputFile},
		{"deleted", func(t *testing.T, _ *miniredis.Miniredis, store *RedisStore) string {
			id := addInput(t, store, "alice")
			_, err := store.DeleteFile(context.Background(), id)
			require.NoError(t, err)
			return id
		}, ErrNoInputFile},
		{"expired", func(t *testing.T, mini *miniredis.Miniredis, store *RedisStore) string {
			require.NoError(t, store.CreateFile(context.Background(), &File{ID: "file-old", Tenant: "alice", Purpose: PurposeBatch, CreatedAt: 1000, ExpiresAt: 5000}))
			mini.SetTime(time.Unix(5000, 0))
			return "file-old"
		}, ErrNoInputFile},
		{"a result file", func(t *testing.T, _ *miniredis.Miniredis, store *RedisStore) string {
			require.NoError(t, store.CreateFile(context.Background(), &File{ID: "file-out", Tenant: "alice", Purpose: PurposeBatchOutput, CreatedAt: 1000}))
			return "file-out"
		}, ErrNotBatchFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mini, store := newTestStore(t)
			input := tc.setup(t, mini, store)
			err := store.CreateJob(context.Background(), newJob("batch_1", "alice", input))
			require.ErrorIs(t, err, tc.want)
			assert.False(t, mini.Exists(store.jobKey("batch_1")), "a refused batch leaves no record")
			assert.False(t, mini.Exists(store.queueKey()), "a refused batch is not queued")
			assert.False(t, mini.Exists(store.fileRefsKey(input)), "a refused batch holds nothing")
		})
	}
}

func TestCreateJobStillTakesAFileJustBeforeItExpires(t *testing.T) {
	mini, store := newTestStore(t)
	require.NoError(t, store.CreateFile(context.Background(), &File{ID: "file-soon", Tenant: "alice", Purpose: PurposeBatch, CreatedAt: 1000, ExpiresAt: 5000}))
	mini.SetTime(time.Unix(4999, 0))
	assert.NoError(t, store.CreateJob(context.Background(), newJob("batch_1", "alice", "file-soon")))
}

func TestCreateJobTwiceChangesNothing(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	input := addInput(t, store, "alice")
	require.NoError(t, store.CreateJob(ctx, newJob("batch_1", "alice", input)))
	// A retry after a lost reply runs the script again with the same batch ID.
	again := newJob("batch_1", "alice", input)
	again.Endpoint = "/v1/changed"
	again.CreatedAt = 9000
	require.NoError(t, store.CreateJob(ctx, again))

	got, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	assert.Equal(t, "/v1/chat/completions", got.Endpoint, "the first write stays")
	score, err := mini.ZScore(store.queueKey(), "batch_1")
	require.NoError(t, err)
	assert.Equal(t, float64(2000), score, "the batch keeps its place in the queue")
	holders, err := mini.Members(store.fileRefsKey(input))
	require.NoError(t, err)
	assert.Len(t, holders, 1)
}

func TestDeleteRacingCreateKeepsTheInputConsistent(t *testing.T) {
	t.Run("delete first", func(t *testing.T) {
		mini, store := newTestStore(t)
		ctx := context.Background()
		input := addInput(t, store, "alice")
		removable, err := store.DeleteFile(ctx, input)
		require.NoError(t, err)
		assert.True(t, removable)
		assert.ErrorIs(t, store.CreateJob(ctx, newJob("batch_1", "alice", input)), ErrNoInputFile)
		assert.False(t, mini.Exists(store.jobKey("batch_1")))
	})
	t.Run("create first", func(t *testing.T) {
		_, store := newTestStore(t)
		ctx := context.Background()
		input := addInput(t, store, "alice")
		require.NoError(t, store.CreateJob(ctx, newJob("batch_1", "alice", input)))
		removable, err := store.DeleteFile(ctx, input)
		require.NoError(t, err)
		assert.False(t, removable, "the batch still needs the bytes")
		_, err = store.GetJob(ctx, "batch_1")
		assert.NoError(t, err)
	})
}

func TestCreateJobNeedsAnID(t *testing.T) {
	_, store := newTestStore(t)
	assert.Error(t, store.CreateJob(context.Background(), nil))
	assert.Error(t, store.CreateJob(context.Background(), &Job{Tenant: "alice", InputFileID: "file-1"}))
}

// queueJob creates a batch of tenant alice created at createdAt and returns its ID.
func queueJob(t *testing.T, store *RedisStore, id string, createdAt int64) string {
	t.Helper()
	job := newJob(id, "alice", addInput(t, store, "alice"))
	job.CreatedAt = createdAt
	require.NoError(t, store.CreateJob(context.Background(), job))
	return id
}

func TestClaimTakesTheOldestQueuedBatch(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_new", 2000)
	queueJob(t, store, "batch_old", 1000)
	mini.SetTime(time.Unix(3000, 0))

	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "batch_old", lease.JobID)
	assert.Equal(t, "pod-a", lease.Holder)
	assert.Regexp(t, `^[0-9a-f]{32}$`, lease.Fence, "the fence is random, so it is never handed out twice")

	job, err := store.GetJob(ctx, "batch_old")
	require.NoError(t, err)
	assert.Equal(t, StatusInProgress, job.Status)
	assert.Equal(t, int64(3000), job.InProgressAt, "the start time comes from Redis")
	assert.Equal(t, "pod-a", job.Holder)
	assert.Equal(t, lease.Fence, job.Fence)
	members, err := mini.ZMembers(store.queueKey())
	require.NoError(t, err)
	assert.Equal(t, []string{"batch_new"}, members)
}

func TestClaimWithNothingQueued(t *testing.T) {
	_, store := newTestStore(t)
	lease, err := store.Claim(context.Background(), "pod-a")
	require.NoError(t, err)
	assert.Nil(t, lease)
}

func TestTwoClaimsNeverTakeTheSameBatch(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	first, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := store.Claim(ctx, "pod-b")
	require.NoError(t, err)
	assert.Nil(t, second, "one batch has one owner")
}

func TestClaimSkipsBatchesThatCannotRun(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_done", 1000)
	mini.HSet(store.jobKey("batch_done"), "status", string(StatusCompleted))
	_, err := mini.ZAdd(store.queueKey(), 1500, "batch_gone")
	require.NoError(t, err)
	queueJob(t, store, "batch_next", 2000)

	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "batch_next", lease.JobID)
	assert.False(t, mini.Exists(store.queueKey()), "the entries that cannot run are taken out of the queue")
}

func TestARetriedClaimReturnsTheSameBatch(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	queueJob(t, store, "batch_2", 2000)
	claim := func() any {
		res, err := claimJobScript.Run(ctx, store.client, []string{store.queueKey(), store.claimedKey("f00d")}, store.prefix, "pod-a", "f00d", claimScan).Result()
		require.NoError(t, err)
		return res
	}
	// A reply lost on the way back makes go-redis run the same script again with the same fence.
	assert.Equal(t, "batch_1", claim())
	assert.Equal(t, "batch_1", claim())
	members, err := mini.ZMembers(store.queueKey())
	require.NoError(t, err)
	assert.Equal(t, []string{"batch_2"}, members, "the retry did not take a second batch")
}

func TestProgressIsWrittenOnlyByTheOwner(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)

	require.NoError(t, store.Progress(ctx, lease, 10, 4, 1))
	require.NoError(t, store.Progress(ctx, lease, 10, 4, 1), "a retry writes the same counts")
	stale := &Lease{JobID: "batch_1", Holder: "pod-b", Fence: "0123"}
	assert.ErrorIs(t, store.Progress(ctx, stale, 10, 9, 0), ErrNotOwner)

	job, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	assert.Equal(t, []int64{10, 4, 1}, []int64{job.Total, job.Completed, job.Failed})
}

func TestTransitionMovesOnlyTheOwnersBatchOnward(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)
	job, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	mini.SetTime(time.Unix(5000, 0))

	stale := &Lease{JobID: "batch_1", Holder: "pod-b", Fence: "0123"}
	assert.ErrorIs(t, store.Transition(ctx, stale, StatusInProgress, StatusFinalizing, nil), ErrNotOwner)
	assert.ErrorIs(t, store.Transition(ctx, lease, StatusValidating, StatusFinalizing, nil), ErrStatusChanged)
	require.NoError(t, store.Transition(ctx, lease, StatusInProgress, StatusFinalizing, nil))
	require.NoError(t, store.Transition(ctx, lease, StatusInProgress, StatusFinalizing, nil), "a retry finds the step done")

	holders, err := mini.Members(store.fileRefsKey(job.InputFileID))
	require.NoError(t, err)
	assert.Equal(t, []string{"batch_1"}, holders, "the input is held until the batch ends")
	require.NoError(t, store.Transition(ctx, lease, StatusFinalizing, StatusCompleted, map[string]string{"output_file_id": "file-out", "completed": "3"}))

	got, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	assert.Equal(t, StatusCompleted, got.Status)
	assert.Equal(t, int64(5000), got.FinalizingAt)
	assert.Equal(t, int64(5000), got.CompletedAt)
	assert.Equal(t, "file-out", got.OutputFileID)
	assert.Equal(t, int64(3), got.Completed)
	assert.False(t, mini.Exists(store.fileRefsKey(job.InputFileID)), "a finished batch lets go of its input")
}

func TestReleaseHandsTheBatchBackAtItsPlace(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)

	require.NoError(t, store.Release(ctx, lease))
	require.NoError(t, store.Release(ctx, lease), "a retry finds the batch queued")
	score, err := mini.ZScore(store.queueKey(), "batch_1")
	require.NoError(t, err)
	assert.Equal(t, float64(1000), score, "a released batch keeps its place in the queue")
	job, err := store.GetJob(ctx, "batch_1")
	require.NoError(t, err)
	assert.Empty(t, job.Holder)
	assert.Empty(t, job.Fence)
	assert.ErrorIs(t, store.Progress(ctx, lease, 1, 1, 0), ErrNotOwner, "the old run can no longer write")

	next, err := store.Claim(ctx, "pod-b")
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, "batch_1", next.JobID)
	assert.ErrorIs(t, store.Release(ctx, lease), ErrNotOwner, "the old run cannot hand back a batch it lost")
}

func TestReleaseLeavesAFinishingBatchAlone(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	queueJob(t, store, "batch_1", 1000)
	lease, err := store.Claim(ctx, "pod-a")
	require.NoError(t, err)
	require.NoError(t, store.Transition(ctx, lease, StatusInProgress, StatusFinalizing, nil))

	assert.ErrorIs(t, store.Release(ctx, lease), ErrStatusChanged)
	assert.False(t, mini.Exists(store.queueKey()), "a batch past its lines is not run again")
}
