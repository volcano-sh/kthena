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
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) (*miniredis.Miniredis, *RedisStore) {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mini, NewRedisStore(client, "")
}

// addFiles stores n files for one tenant, one second apart, and returns their IDs oldest first.
func addFiles(t *testing.T, store *RedisStore, tenant, purpose string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("file-%s-%s-%03d", tenant, purpose, i)
		require.NoError(t, store.CreateFile(context.Background(), &File{
			ID: id, Tenant: tenant, Purpose: purpose, CreatedAt: int64(1000 + i),
		}))
		ids = append(ids, id)
	}
	return ids
}

func idsOf(files []*File) []string {
	ids := make([]string, 0, len(files))
	for _, file := range files {
		ids = append(ids, file.ID)
	}
	return ids
}

func TestFileRecordRoundTrip(t *testing.T) {
	mini, store := newTestStore(t)
	ctx := context.Background()
	in := &File{ID: "file-1", Tenant: "alice", Filename: "in.jsonl", Purpose: PurposeBatch,
		Status: FileProcessed, Bytes: 42, CreatedAt: 1700000000, ExpiresAt: 1700003600}
	require.NoError(t, store.CreateFile(ctx, in))

	out, err := store.GetFile(ctx, "file-1")
	require.NoError(t, err)
	assert.Equal(t, in, out)
	assert.Equal(t, recordVersion, mini.HGet(DefaultKeyPrefix+"file:file-1", "v"))

	_, err = store.GetFile(ctx, "file-missing")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestCreateFileRejectsAMissingID(t *testing.T) {
	_, store := newTestStore(t)
	assert.Error(t, store.CreateFile(context.Background(), &File{Tenant: "alice"}))
	assert.Error(t, store.CreateFile(context.Background(), nil))
}

func TestDeleteFile(t *testing.T) {
	tests := []struct {
		name      string
		held      bool
		removable bool
	}{
		{name: "no batch needs it", removable: true},
		{name: "a batch still needs it", held: true, removable: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mini, store := newTestStore(t)
			ctx := context.Background()
			addFiles(t, store, "alice", PurposeBatch, 1)
			id := "file-alice-batch-000"
			if tt.held {
				_, err := mini.SAdd(DefaultKeyPrefix+"fileref:"+id, "batch_1")
				require.NoError(t, err)
			}

			removable, err := store.DeleteFile(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, tt.removable, removable)

			file, err := store.GetFile(ctx, id)
			require.NoError(t, err)
			assert.True(t, file.Deleted, "the record stays, marked deleted")
			files, more, err := store.ListFiles(ctx, FileQuery{Tenant: "alice", Limit: 10})
			require.NoError(t, err)
			assert.Empty(t, files, "a deleted file leaves the listing")
			assert.False(t, more)
			members, err := mini.ZMembers(DefaultKeyPrefix + "idx:files:alice")
			if err == nil {
				assert.NotContains(t, members, id, "a deleted file leaves the index too, so it cannot pile up")
			}
		})
	}
}

func TestDeleteFileThatDoesNotExist(t *testing.T) {
	_, store := newTestStore(t)
	_, err := store.DeleteFile(context.Background(), "file-missing")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestListFiles(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	ids := addFiles(t, store, "alice", PurposeBatch, 5)
	addFiles(t, store, "bob", PurposeBatch, 2)

	tests := []struct {
		name     string
		query    FileQuery
		want     []string
		wantMore bool
	}{
		{name: "newest first by default", query: FileQuery{Tenant: "alice", Limit: 10},
			want: []string{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{name: "oldest first", query: FileQuery{Tenant: "alice", Limit: 10, Ascending: true}, want: ids},
		{name: "a short page says more follow", query: FileQuery{Tenant: "alice", Limit: 2},
			want: []string{ids[4], ids[3]}, wantMore: true},
		{name: "the cursor continues after an id", query: FileQuery{Tenant: "alice", Limit: 2, After: ids[3]},
			want: []string{ids[2], ids[1]}, wantMore: true},
		{name: "the last page says no more", query: FileQuery{Tenant: "alice", Limit: 2, After: ids[1]},
			want: []string{ids[0]}},
		{name: "an exact page says no more", query: FileQuery{Tenant: "alice", Limit: 5},
			want: []string{ids[4], ids[3], ids[2], ids[1], ids[0]}},
		{name: "the cursor works oldest first", query: FileQuery{Tenant: "alice", Limit: 2, After: ids[1], Ascending: true},
			want: []string{ids[2], ids[3]}, wantMore: true},
		{name: "another tenant sees only its own", query: FileQuery{Tenant: "carol", Limit: 10}, want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, more, err := store.ListFiles(ctx, tt.query)
			require.NoError(t, err)
			assert.Equal(t, tt.want, idsOf(files))
			assert.Equal(t, tt.wantMore, more)
		})
	}

	_, _, err := store.ListFiles(ctx, FileQuery{Tenant: "alice", Limit: 2, After: "file-unknown"})
	assert.ErrorIs(t, err, ErrNotFound, "an unknown cursor is an error, not an empty page")
}

func TestListFilesFillsThePagePastFilteredFiles(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	// 150 newer output files sit in front of 3 older inputs, more than one scan round.
	inputs := addFiles(t, store, "alice", PurposeBatch, 3)
	for i := 0; i < 150; i++ {
		require.NoError(t, store.CreateFile(ctx, &File{
			ID: fmt.Sprintf("file-out-%03d", i), Tenant: "alice", Purpose: PurposeBatchOutput, CreatedAt: int64(2000 + i),
		}))
	}

	files, more, err := store.ListFiles(ctx, FileQuery{Tenant: "alice", Purpose: PurposeBatch, Limit: 2})
	require.NoError(t, err)
	assert.Equal(t, []string{inputs[2], inputs[1]}, idsOf(files), "the page is full although 150 files were skipped")
	assert.True(t, more)

	files, more, err = store.ListFiles(ctx, FileQuery{Tenant: "alice", Purpose: PurposeBatch, Limit: 2, After: inputs[1]})
	require.NoError(t, err)
	assert.Equal(t, []string{inputs[0]}, idsOf(files))
	assert.False(t, more)
}

func TestListingKeepsCreationOrderWithinOneSecond(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	clock := time.Unix(1700000000, 0)
	store.now = func() time.Time { clock = clock.Add(time.Millisecond); return clock }
	for i := 0; i < 5; i++ {
		require.NoError(t, store.CreateFile(ctx, &File{ID: fmt.Sprintf("file-%d", i), Tenant: "alice", CreatedAt: 1700000000}))
	}
	files, _, err := store.ListFiles(ctx, FileQuery{Tenant: "alice", Limit: 10, Ascending: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"file-0", "file-1", "file-2", "file-3", "file-4"}, idsOf(files))
}

func TestStoreErrorsAreNotNotFound(t *testing.T) {
	mini, store := newTestStore(t)
	mini.Close()
	_, err := store.GetFile(context.Background(), "file-1")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound, "Redis being down must not look like a missing file")
}

func TestListingKeepsUploadOrderAcrossASecondBoundary(t *testing.T) {
	_, store := newTestStore(t)
	ctx := context.Background()
	store.now = func() time.Time { return time.Unix(100, 900_000_000) }
	require.NoError(t, store.CreateFile(ctx, &File{ID: "file-first", Tenant: "alice", CreatedAt: 100}))
	store.now = func() time.Time { return time.Unix(101, 50_000_000) }
	require.NoError(t, store.CreateFile(ctx, &File{ID: "file-second", Tenant: "alice", CreatedAt: 100}))
	store.now = func() time.Time { return time.Unix(101, 60_000_000) }
	require.NoError(t, store.CreateFile(ctx, &File{ID: "file-third", Tenant: "alice", CreatedAt: 101}))

	files, _, err := store.ListFiles(ctx, FileQuery{Tenant: "alice", Limit: 10, Ascending: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"file-first", "file-second", "file-third"}, idsOf(files),
		"a record written after the second ticked over still sorts inside its own second")
}
