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
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

const (
	// defaultReadyWait bounds how long a request waits for a storage check before it answers 503.
	defaultReadyWait = 2 * time.Second
	// defaultRetryEvery is the pause between background checks until one passes.
	defaultRetryEvery = 5 * time.Second
	// volumeIDFile names the volume, so records are never served from a volume they do not belong to.
	volumeIDFile = "volume-id"
)

var volumeIDPattern = regexp.MustCompile(`^volume-[0-9a-f]{24}$`)

// readiness opens once a storage check passes; until then each request starts a check or joins the one running.
type readiness struct {
	check   func(context.Context) error
	ready   atomic.Bool
	mu      sync.Mutex
	running chan struct{}
	lastErr string
}

// start runs a check unless one is running already, and returns a channel that is closed when it ends.
func (r *readiness) start(ctx context.Context) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != nil {
		return r.running
	}
	done := make(chan struct{})
	r.running = done
	go func() {
		defer close(done)
		err := r.check(ctx)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.running = nil
		if err == nil {
			r.ready.Store(true)
			klog.Info("batch: storage is ready")
			return
		}
		// One line per new cause keeps a long outage from flooding the log.
		if msg := err.Error(); msg != r.lastErr {
			r.lastErr = msg
			klog.Errorf("batch: storage is not ready: %v", err)
		}
	}()
	return done
}

// await reports whether the storage is ready, waiting at most wait for a check.
func (r *readiness) await(ctx context.Context, wait time.Duration) bool {
	if r.ready.Load() {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-r.start(context.WithoutCancel(ctx)):
	case <-timer.C:
	case <-ctx.Done():
	}
	return r.ready.Load()
}

// keepTrying runs checks until one passes, so the storage comes back by itself even without requests.
func (r *readiness) keepTrying(ctx context.Context, every time.Duration) {
	for {
		<-r.start(ctx)
		if r.ready.Load() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Start checks the storage in the background until a check passes, so a slow volume or Redis never holds up the caller.
func (s *Service) Start(ctx context.Context) {
	go s.gate.keepTrying(ctx, s.retryEvery)
}

// checkStorage passes when this pod can use the volume and the records in Redis belong to that volume.
func (s *Service) checkStorage(ctx context.Context) error {
	if err := s.files.Probe(ctx); err != nil {
		return err
	}
	id, err := s.files.VolumeID(ctx)
	if err != nil {
		return err
	}
	recorded, err := s.store.ClaimVolume(ctx, id)
	if err != nil {
		return fmt.Errorf("batch: cannot reach the record store: %w", err)
	}
	if recorded != id {
		return fmt.Errorf("batch: this pod mounts volume %s, but the records in Redis belong to volume %s", id, recorded)
	}
	return nil
}

// VolumeID returns the ID written on the volume, writing a new one the first time.
func (s *FSStore) VolumeID(_ context.Context) (string, error) {
	dir := filepath.Join(s.root, reservedDir)
	path := filepath.Join(dir, volumeIDFile)
	id, err := readVolumeID(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if err := ensureDir(dir); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if errors.Is(err, fs.ErrExist) {
		// Another replica created it first.
		return readVolumeID(path)
	}
	if err != nil {
		return "", err
	}
	id = newID("volume-")
	// A failed write leaves a cut ID that no replica accepts, so only then can the file go and be written again.
	if _, err := f.WriteString(id); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("batch: cannot write the volume ID: %w", err)
	}
	// Once the whole ID is in the file another replica may already use it, so the file stays even if syncing fails.
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncDir(dir)
	}
	if err != nil {
		return "", fmt.Errorf("batch: cannot make the volume ID durable: %w", err)
	}
	return id, nil
}

// readVolumeID reads the ID; one that is empty or cut short is still being written by another replica.
func readVolumeID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if !volumeIDPattern.MatchString(id) {
		return "", fmt.Errorf("batch: the volume ID in %s is incomplete or damaged", path)
	}
	return id, nil
}
