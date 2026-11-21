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

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// Health records the controllers that stopped while the controller manager is still running.
type Health struct {
	mu      sync.Mutex
	stopped []string
}

// run calls fn and records name as stopped if fn returns before manager is done.
func (h *Health) run(manager context.Context, name string, fn func()) {
	fn()
	if h == nil || manager.Err() != nil {
		return
	}
	klog.Errorf("%s controller stopped while the controller manager is still running", name)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = append(h.stopped, name)
}

// Check returns an error naming every controller that stopped unexpectedly.
func (h *Health) Check() error {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.stopped) == 0 {
		return nil
	}
	names := slices.Clone(h.stopped)
	slices.Sort(names)
	return fmt.Errorf("controllers stopped: %s", strings.Join(names, ", "))
}
