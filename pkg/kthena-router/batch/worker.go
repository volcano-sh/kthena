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
	"math/rand/v2"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	// defaultRunners is how many batches one router replica runs at once.
	defaultRunners = 2
	// defaultPoll is how long an idle runner waits before it looks at the queue again.
	defaultPoll = 3 * time.Second
	// releaseTimeout bounds handing a batch back, which runs on after shutdown starts.
	releaseTimeout = 5 * time.Second
)

// worker claims queued batches for this replica and runs them while the storage is ready.
type worker struct {
	store   Store
	exec    *executor
	gate    *readiness
	holder  string
	runners int
	poll    time.Duration
}

// run keeps the runners going until ctx ends and returns once every one has stopped.
func (w *worker) run(ctx context.Context) {
	var runners sync.WaitGroup
	for i := 0; i < w.runners; i++ {
		runners.Add(1)
		go func() {
			defer runners.Done()
			w.loop(ctx)
		}()
	}
	runners.Wait()
}

func (w *worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		if !w.gate.ready.Load() {
			w.wait(ctx)
			continue
		}
		lease, err := w.store.Claim(ctx, w.holder)
		if err != nil && ctx.Err() == nil {
			klog.Errorf("batch: cannot claim a batch: %v", err)
		}
		if err != nil || lease == nil {
			w.wait(ctx)
			continue
		}
		if err := w.runOne(ctx, lease); err != nil {
			// A run that failed waits once, so a batch that always fails is not claimed in a tight loop.
			w.wait(ctx)
		}
	}
}

// runOne runs one batch and hands it back to the queue when it did not finish, also on shutdown.
func (w *worker) runOne(ctx context.Context, lease *Lease) error {
	err := w.exec.run(ctx, lease)
	if err == nil {
		return nil
	}
	switch {
	case ctx.Err() != nil:
		klog.V(2).Infof("batch: %s stops for shutdown and goes back to the queue", lease.JobID)
	case errors.Is(err, ErrNotOwner):
		klog.V(2).Infof("batch: %s is no longer run here: %v", lease.JobID, err)
		return err
	default:
		klog.Errorf("batch: running %s failed, it goes back to the queue: %v", lease.JobID, err)
	}
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if releaseErr := w.store.Release(release, lease); releaseErr != nil {
		klog.Errorf("batch: cannot hand %s back to the queue: %v", lease.JobID, releaseErr)
	}
	return err
}

// wait sleeps one poll interval with jitter, so replicas do not all look at the queue at once.
func (w *worker) wait(ctx context.Context) {
	timer := time.NewTimer(w.poll + rand.N(w.poll/2+1))
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
