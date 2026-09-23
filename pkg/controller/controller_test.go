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
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOnStoppedLeading verifies that the OnStoppedLeading callback only exits the
// process when leadership is lost unexpectedly while the manager is still supposed
// to be running, and not when the manager's own context was already canceled as
// part of a graceful shutdown.
func TestOnStoppedLeading(t *testing.T) {
	t.Run("exits when leadership is lost unexpectedly", func(t *testing.T) {
		ctx := context.Background()
		exited := false
		onStoppedLeading(ctx, func() { exited = true })()
		assert.True(t, exited, "expected the process to exit after an unexpected leadership loss")
	})

	t.Run("does not exit during graceful shutdown", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		exited := false
		onStoppedLeading(ctx, func() { exited = true })()
		assert.False(t, exited, "graceful shutdown should not force a process exit")
	})
}
