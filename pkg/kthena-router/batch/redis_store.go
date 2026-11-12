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
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultKeyPrefix keeps every batch key in one Redis slot, which the scripts need.
const DefaultKeyPrefix = "kthena:batch:{b}:"

// listScan is how many index entries one listing round reads.
const listScan = 100

//go:embed scripts/*.lua
var scriptFS embed.FS

var deleteFileScript = mustLoadScript("delete_file.lua")

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

// indexScore orders a listing by write time in microseconds, kept inside the second of created_at.
func (s *RedisStore) indexScore(createdAt int64) float64 {
	first := createdAt * 1e6
	return float64(min(max(s.now().UnixMicro(), first), first+1e6-1))
}

// CreateFile stores a file record and indexes it for its tenant.
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
	pipe := s.client.TxPipeline()
	pipe.HSet(ctx, s.fileKey(file.ID), file.toHash()...)
	pipe.ZAddNX(ctx, s.fileIndexKey(file.Tenant), redis.Z{Score: s.indexScore(file.CreatedAt), Member: file.ID})
	_, err := pipe.Exec(ctx)
	return err
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
	res, err := deleteFileScript.Run(ctx, s.client, keys, id).Text()
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
	start := int64(0)
	if query.After != "" {
		rank, err := s.rank(ctx, index, query.After, query.Ascending)
		if err != nil {
			return nil, false, err
		}
		start = rank + 1
	}
	files := make([]*File, 0, query.Limit)
	for {
		ids, err := s.slice(ctx, index, start, start+listScan-1, query.Ascending)
		if err != nil {
			return nil, false, err
		}
		for _, id := range ids {
			file, err := s.GetFile(ctx, id)
			if errors.Is(err, ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, false, err
			}
			if file.Deleted || (query.Purpose != "" && file.Purpose != query.Purpose) {
				continue
			}
			if len(files) == query.Limit {
				return files, true, nil
			}
			files = append(files, file)
		}
		if len(ids) < listScan {
			return files, false, nil
		}
		start += listScan
	}
}

func (s *RedisStore) rank(ctx context.Context, index, id string, ascending bool) (int64, error) {
	cmd := s.client.ZRevRank(ctx, index, id)
	if ascending {
		cmd = s.client.ZRank(ctx, index, id)
	}
	rank, err := cmd.Result()
	if errors.Is(err, redis.Nil) {
		return 0, ErrNotFound
	}
	return rank, err
}

func (s *RedisStore) slice(ctx context.Context, index string, start, stop int64, ascending bool) ([]string, error) {
	if ascending {
		return s.client.ZRange(ctx, index, start, stop).Result()
	}
	return s.client.ZRevRange(ctx, index, start, stop).Result()
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
