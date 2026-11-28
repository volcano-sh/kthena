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
	// defaultRetryEvery is the pause between background checks while the storage is not ready.
	defaultRetryEvery = 5 * time.Second
	// defaultRecheckEvery is the pause between background checks while the storage is ready.
	defaultRecheckEvery = time.Minute
	// volumeIDFile names the volume, so records are never served from a volume they do not belong to.
	volumeIDFile = "volume-id"
)

var volumeIDPattern = regexp.MustCompile(`^volume-[0-9a-f]{24}$`)

// readiness is open while the last storage check passed; while closed each request starts a check or joins the one running.
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
			r.lastErr = ""
			if !r.ready.Swap(true) {
				klog.Info("batch: storage is ready")
			}
			return
		}
		r.ready.Store(false)
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

// keepChecking checks until ctx ends, so the storage comes back by itself and a later outage or mismatch closes the gate.
func (r *readiness) keepChecking(ctx context.Context, retry, recheck time.Duration) {
	for {
		<-r.start(ctx)
		pause := retry
		if r.ready.Load() {
			pause = recheck
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}

// Start checks the storage and runs batches in the background for as long as ctx lives; the channel closes once both stopped.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	var parts sync.WaitGroup
	parts.Add(1)
	go func() {
		defer parts.Done()
		s.gate.keepChecking(ctx, s.retryEvery, s.recheckEvery)
	}()
	if s.worker != nil {
		parts.Add(1)
		go func() {
			defer parts.Done()
			s.worker.run(ctx)
		}()
	}
	done := make(chan struct{})
	go func() {
		parts.Wait()
		close(done)
	}()
	return done
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
	// The ID is written and synced under a temporary name first, so a crash never leaves a cut marker.
	temp, err := os.CreateTemp(dir, "."+volumeIDFile+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temp.Name())
	id = newID("volume-")
	if err = temp.Chmod(fileMode); err == nil {
		_, err = temp.WriteString(id)
	}
	if err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("batch: cannot write the volume ID: %w", err)
	}
	// A hard link publishes the whole ID at once, and fails when another replica published one first.
	if err := os.Link(temp.Name(), path); errors.Is(err, fs.ErrExist) {
		return readVolumeID(path)
	} else if err != nil {
		return "", fmt.Errorf("batch: cannot publish the volume ID, the volume must support hard links: %w", err)
	}
	// Once published another replica may already use the ID, so it stays even if syncing the directory fails.
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("batch: cannot make the volume ID durable: %w", err)
	}
	return id, nil
}

// readVolumeID reads the ID; it is only ever published whole, so one that does not match is damaged.
func readVolumeID(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if !volumeIDPattern.MatchString(id) {
		return "", fmt.Errorf("batch: the volume ID in %s is damaged", path)
	}
	return id, nil
}
