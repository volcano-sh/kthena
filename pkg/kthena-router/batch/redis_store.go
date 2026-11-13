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
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultKeyPrefix keeps every batch key in one Redis slot, which the scripts need.
const DefaultKeyPrefix = "kthena:batch:{b}:"

//go:embed scripts/*.lua
var scriptFS embed.FS

// listScan is how many index entries a listing reads at once after filtered files left its page short.
const listScan = 100

var (
	createFileScript = mustLoadScript("create_file.lua")
	deleteFileScript = mustLoadScript("delete_file.lua")
)

func mustLoadScript(name string) *redis.Script {
	src, err := scriptFS.ReadFile("scripts/" + name)
	if err != nil {
		panic(fmt.Sprintf("batch: cannot read %s: %v", name, err))
	}
	return redis.NewScript(string(src))
}

// RedisStore is the Redis implementation of Store.
type RedisStore struct {
	client *redis.Client
	prefix string
	now    func() time.Time
}

var _ Store = (*RedisStore)(nil)

// NewRedisStore builds a store on an existing client; an empty prefix uses DefaultKeyPrefix.
func NewRedisStore(client *redis.Client, prefix string) *RedisStore {
	if prefix == "" {
		prefix = DefaultKeyPrefix
	}
	return &RedisStore{client: client, prefix: prefix, now: time.Now}
}

func (s *RedisStore) fileKey(id string) string          { return s.prefix + "file:" + id }
func (s *RedisStore) fileRefsKey(id string) string      { return s.prefix + "fileref:" + id }
func (s *RedisStore) fileIndexKey(tenant string) string { return s.prefix + "idx:files:" + tenant }

// indexTime is the write time in microseconds, kept inside the second of created_at.
func (s *RedisStore) indexTime(createdAt int64) int64 {
	first := createdAt * 1e6
	return min(max(s.now().UnixMicro(), first), first+1e6-1)
}

// indexMember sorts by write time and then ID, so a page can start after any record, even a deleted one.
func indexMember(file *File) string {
	return fmt.Sprintf("%020d:%s", file.IndexedAt, file.ID)
}

// CreateFile stores a file record and indexes it for its tenant in one step.
func (s *RedisStore) CreateFile(ctx context.Context, file *File) error {
	if file == nil || file.ID == "" {
		return errors.New("batch: file id is required")
	}
	if file.Status == "" {
		file.Status = FileUploaded
	}
	if file.CreatedAt == 0 {
		file.CreatedAt = s.now().Unix()
	}
	file.IndexedAt = s.indexTime(file.CreatedAt)
	keys := []string{s.fileKey(file.ID), s.fileIndexKey(file.Tenant)}
	args := append([]any{indexMember(file)}, file.toHash()...)
	return createFileScript.Run(ctx, s.client, keys, args...).Err()
}

// GetFile returns one file record.
func (s *RedisStore) GetFile(ctx context.Context, id string) (*File, error) {
	raw, err := s.client.HGetAll(ctx, s.fileKey(id)).Result()
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, ErrNotFound
	}
	return fileFromHash(raw), nil
}

// DeleteFile marks the record deleted and says whether the bytes can be removed now.
func (s *RedisStore) DeleteFile(ctx context.Context, id string) (bool, error) {
	file, err := s.GetFile(ctx, id)
	if err != nil {
		return false, err
	}
	keys := []string{s.fileKey(id), s.fileRefsKey(id), s.fileIndexKey(file.Tenant)}
	res, err := deleteFileScript.Run(ctx, s.client, keys, indexMember(file)).Text()
	if err != nil {
		return false, err
	}
	if res == "NOT_FOUND" {
		return false, ErrNotFound
	}
	return res == "REMOVABLE", nil
}

// ListFiles returns one full page, reading on past files the purpose filter drops.
func (s *RedisStore) ListFiles(ctx context.Context, query FileQuery) ([]*File, bool, error) {
	index := s.fileIndexKey(query.Tenant)
	after := ""
	if query.After != "" {
		cursor, err := s.GetFile(ctx, query.After)
		if err != nil {
			return nil, false, err
		}
		if cursor.Tenant != query.Tenant {
			return nil, false, ErrNotFound
		}
		after = indexMember(cursor)
	}
	limit := max(query.Limit, 1)
	files := make([]*File, 0, min(limit, listScan))
	// One more than the page tells whether more files follow.
	want := limit + 1
	for {
		members, err := s.slice(ctx, index, after, want, query.Ascending)
		if err != nil {
			return nil, false, err
		}
		found, err := s.getFiles(ctx, members)
		if err != nil {
			return nil, false, err
		}
		for _, file := range found {
			if file.Deleted || (query.Purpose != "" && file.Purpose != query.Purpose) {
				continue
			}
			if len(files) == limit {
				return files, true, nil
			}
			files = append(files, file)
		}
		if len(members) < want {
			return files, false, nil
		}
		after = members[len(members)-1]
		want = max(want, listScan)
	}
}

// slice reads up to count index members that follow after in listing order.
func (s *RedisStore) slice(ctx context.Context, index, after string, count int, ascending bool) ([]string, error) {
	by := &redis.ZRangeBy{Min: "-", Max: "+", Count: int64(count)}
	if ascending {
		if after != "" {
			by.Min = "(" + after
		}
		return s.client.ZRangeByLex(ctx, index, by).Result()
	}
	if after != "" {
		by.Max = "(" + after
	}
	return s.client.ZRevRangeByLex(ctx, index, by).Result()
}

// getFiles reads the records of index members in one round trip, skipping any that are gone.
func (s *RedisStore) getFiles(ctx context.Context, members []string) ([]*File, error) {
	if len(members) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, 0, len(members))
	for _, member := range members {
		_, id, _ := strings.Cut(member, ":")
		cmds = append(cmds, pipe.HGetAll(ctx, s.fileKey(id)))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	files := make([]*File, 0, len(cmds))
	for _, cmd := range cmds {
		if raw := cmd.Val(); len(raw) > 0 {
			files = append(files, fileFromHash(raw))
		}
	}
	return files, nil
}

func (f *File) toHash() []any {
	deleted := "0"
	if f.Deleted {
		deleted = "1"
	}
	return []any{
		"v", recordVersion,
		"id", f.ID,
		"tenant", f.Tenant,
		"filename", f.Filename,
		"purpose", f.Purpose,
		"status", f.Status,
		"bytes", f.Bytes,
		"created_at", f.CreatedAt,
		"expires_at", f.ExpiresAt,
		"indexed_at", f.IndexedAt,
		"deleted", deleted,
	}
}

func fileFromHash(raw map[string]string) *File {
	return &File{
		ID:        raw["id"],
		Tenant:    raw["tenant"],
		Filename:  raw["filename"],
		Purpose:   raw["purpose"],
		Status:    raw["status"],
		Bytes:     parseInt(raw["bytes"]),
		CreatedAt: parseInt(raw["created_at"]),
		ExpiresAt: parseInt(raw["expires_at"]),
		IndexedAt: parseInt(raw["indexed_at"]),
		Deleted:   raw["deleted"] == "1",
	}
}

func parseInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
