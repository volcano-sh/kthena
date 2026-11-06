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

package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandleTerminationSignals(t *testing.T) {
	// Unbuffered, so a send completes only once the handler receives it.
	signalCh := make(chan os.Signal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exitCode := make(chan int, 1)
	go handleTerminationSignals(signalCh, cancel, func(code int) { exitCode <- code })

	select {
	case signalCh <- syscall.SIGTERM:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler must wait for a signal")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the first signal must start the shutdown")
	}

	// A handler that exits on the first signal never receives the second one.
	select {
	case signalCh <- syscall.SIGINT:
	case code := <-exitCode:
		t.Fatalf("the first signal must not exit, got exit code %d", code)
	case <-time.After(5 * time.Second):
		t.Fatal("the handler must wait for a second signal")
	}
	select {
	case code := <-exitCode:
		require.Equal(t, 1, code)
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal must exit without waiting for the drain")
	}
}
