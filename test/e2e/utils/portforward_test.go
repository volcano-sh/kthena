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

package utils

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/portforward"
)

// --- fake session ---

// mockSession is a fake forwardSession for unit tests. ForwardPorts binds to
// no real socket; it only signals readyCh and blocks until breakCh or stopCh
// is closed. skipDialCheck must be true on the owning forwarder (or the
// dial check will fail).
type mockSession struct {
	readyCh chan struct{} // closed by ForwardPorts; broadcast to all waiters
	breakCh chan struct{} // test closes to simulate pod termination
	stopCh  <-chan struct{}

	fpErr        error                       // returned by ForwardPorts on breakCh
	portsErr     error                       // if non-nil, GetPorts returns this error
	getPortsHook func()                      // if non-nil, called at the start of GetPorts
	ports        []portforward.ForwardedPort // returned by GetPorts
}

func newMockSession(stopCh <-chan struct{}) *mockSession {
	return &mockSession{
		readyCh: make(chan struct{}), // closed by ForwardPorts; broadcast to all waiters
		breakCh: make(chan struct{}),
		stopCh:  stopCh,
		ports:   []portforward.ForwardedPort{{Local: 12345, Remote: 8100}},
	}
}

func (s *mockSession) ForwardPorts() error {
	// Close readyCh to broadcast "tunnel ready" to all concurrent receivers
	// (the buildFn forwarding goroutine and the test both read it).
	close(s.readyCh)
	select {
	case <-s.breakCh:
		return s.fpErr
	case <-s.stopCh:
		return nil
	}
}

func (s *mockSession) GetPorts() ([]portforward.ForwardedPort, error) {
	if s.getPortsHook != nil {
		s.getPortsHook()
	}
	if s.portsErr != nil {
		return nil, s.portsErr
	}
	return s.ports, nil
}

// restart simulates a pod termination; unblocks ForwardPorts.
func (s *mockSession) restart() { close(s.breakCh) }

// --- helpers ---

// makeTestForwarder builds a forwarder configured for unit tests.
// sessions is the ordered list returned by successive buildFn calls.
// resolve defaults to ("pod-0", 8100, nil) when nil.
func makeTestForwarder(sessions []*mockSession, resolve func() (string, int, error)) *forwarder {
	if resolve == nil {
		resolve = func() (string, int, error) { return "pod-0", 8100, nil }
	}
	f := newForwarder("test-ns", "pod-0", 12345, 8100, resolve)
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 5 * time.Second
	f.skipDialCheck = true

	// Set sessions' stopCh now that f.stopCh is allocated.
	for _, s := range sessions {
		s.stopCh = f.stopCh
	}

	var mu sync.Mutex
	idx := 0
	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		mu.Lock()
		i := idx
		idx++
		mu.Unlock()
		if i >= len(sessions) {
			return nil, fmt.Errorf("no more mock sessions (call %d)", i)
		}
		s := sessions[i]
		if readyCh != nil {
			// Forward the session's ready signal to the forwarder's readyCh.
			go func() {
				select {
				case <-s.readyCh:
					select {
					case readyCh <- struct{}{}:
					default:
					}
				case <-f.stopCh:
				}
			}()
		}
		return s, nil
	}
	return f
}

// awaitDone fails the test if the forwarder goroutine has not exited within 5s.
// It verifies goroutine termination rather than relying on Close() alone.
func awaitDone(t *testing.T, f *forwarder) {
	t.Helper()
	select {
	case <-f.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("port-forward goroutine did not exit within 5s — goroutine leak detected")
	}
}

// --- tests ---

// 1. Initial successful startup
func TestForwarder_InitialSuccessfulStartup(t *testing.T) {
	s0 := newMockSession(nil) // stopCh wired by makeTestForwarder
	f := makeTestForwarder([]*mockSession{s0}, nil)

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}

	f.Close()
	awaitDone(t, f)
}

// 2. Initial build failure — Start() must return an error and goroutine must exit.
func TestForwarder_InitialBuildFailure(t *testing.T) {
	f := newForwarder("test-ns", "pod-0", 12345, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 5 * time.Second
	f.skipDialCheck = true
	f.buildFn = func(_ chan struct{}) (forwardSession, error) {
		return nil, fmt.Errorf("simulated auth error")
	}

	err := f.Start()
	if err == nil {
		t.Fatal("Start() should return an error on build failure")
	}
	if !strings.Contains(err.Error(), "simulated auth error") {
		t.Errorf("unexpected error: %v", err)
	}

	awaitDone(t, f) // goroutine must have exited
}

// 3. Three consecutive reconnects
func TestForwarder_ThreeConsecutiveReconnects(t *testing.T) {
	const totalSessions = 4 // initial + 3 reconnects

	sessions := make([]*mockSession, totalSessions)
	// stopCh not set yet; makeTestForwarder sets it.
	for i := range sessions {
		sessions[i] = newMockSession(nil)
	}

	reconnects := 0
	resolve := func() (string, int, error) {
		reconnects++
		return fmt.Sprintf("pod-%d", reconnects), 8100, nil
	}

	f := makeTestForwarder(sessions, resolve)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	for i := 0; i < 3; i++ {
		// Confirm current session's ForwardPorts is running.
		select {
		case <-sessions[i].readyCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("session %d: ForwardPorts did not start in time", i)
		}
		// Trigger pod restart.
		sessions[i].restart()
		// Confirm the next session connected.
		select {
		case <-sessions[i+1].readyCh:
		case <-time.After(5 * time.Second):
			t.Fatalf("forwarder did not reconnect to session %d", i+1)
		}
	}

	if reconnects != 3 {
		t.Errorf("expected 3 resolveTarget calls, got %d", reconnects)
	}

	f.Close()
	awaitDone(t, f)
}

// 4. Service pod replacement — resolve returns a new pod name on each reconnect.
func TestForwarder_ServicePodReplacementReResolves(t *testing.T) {
	s0 := newMockSession(nil)
	s1 := newMockSession(nil)

	var resolvedNames []string
	var mu sync.Mutex
	resolve := func() (string, int, error) {
		mu.Lock()
		defer mu.Unlock()
		name := fmt.Sprintf("pod-%d", len(resolvedNames)+1)
		resolvedNames = append(resolvedNames, name)
		return name, 8100, nil
	}

	f := makeTestForwarder([]*mockSession{s0, s1}, resolve)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh // s0 running
	s0.restart()

	<-s1.readyCh // s1 running after re-resolution

	mu.Lock()
	names := append([]string(nil), resolvedNames...)
	mu.Unlock()

	if len(names) != 1 {
		t.Fatalf("expected 1 resolve call, got %d: %v", len(names), names)
	}
	if names[0] != "pod-1" {
		t.Errorf("expected resolved pod-1, got %q", names[0])
	}

	f.Close()
	awaitDone(t, f)
}

// 5. Temporary build failures followed by success — the forwarder eventually reconnects.
func TestForwarder_TemporaryBuildFailuresThenSuccess(t *testing.T) {
	s0 := newMockSession(nil) // initial session
	s1 := newMockSession(nil) // eventual reconnect session

	buildCalls := 0
	f := newForwarder("test-ns", "pod-0", 12345, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 5 * time.Second
	f.skipDialCheck = true

	for _, s := range []*mockSession{s0, s1} {
		s.stopCh = f.stopCh
	}

	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		buildCalls++
		switch {
		case buildCalls == 1: // initial — succeeds
			if readyCh != nil {
				go func() {
					select {
					case <-s0.readyCh:
						select {
						case readyCh <- struct{}{}:
						default:
						}
					case <-f.stopCh:
					}
				}()
			}
			return s0, nil
		case buildCalls <= 3: // reconnect attempts 1 and 2 — transient failure
			return nil, fmt.Errorf("pod is not running. Status=Pending")
		default: // reconnect attempt 3 — succeeds
			return s1, nil
		}
	}

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh
	s0.restart()

	select {
	case <-s1.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("forwarder did not reconnect after transient failures (buildCalls=%d)", buildCalls)
	}

	if buildCalls < 4 {
		t.Errorf("expected ≥4 build calls (1 initial + 2 fail + 1 success), got %d", buildCalls)
	}

	f.Close()
	awaitDone(t, f)
}

// 6. Permanent forwarding errors — goroutine stops retrying after maxPermanentStreak.
func TestForwarder_PermanentBuildErrors_GivesUp(t *testing.T) {
	s0 := newMockSession(nil)

	buildCalls := 0
	f := newForwarder("test-ns", "pod-0", 12345, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 5 * time.Second
	f.skipDialCheck = true
	s0.stopCh = f.stopCh

	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		buildCalls++
		if buildCalls == 1 { // initial — succeeds
			if readyCh != nil {
				go func() {
					select {
					case <-s0.readyCh:
						select {
						case readyCh <- struct{}{}:
						default:
						}
					case <-f.stopCh:
					}
				}()
			}
			return s0, nil
		}
		// All reconnect attempts fail with a permanent-looking error.
		return nil, fmt.Errorf("failed to create kubernetes client: unauthorized")
	}

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh
	s0.restart() // triggers reconnect; buildFn returns permanent errors

	// After maxPermanentStreak consecutive permanent errors, goroutine must exit.
	awaitDone(t, f)

	if buildCalls < 1+maxPermanentStreak {
		t.Errorf("expected ≥%d build calls (1 initial + %d permanent), got %d",
			1+maxPermanentStreak, maxPermanentStreak, buildCalls)
	}
}

// 7. Close during active forwarding
func TestForwarder_CloseDuringActiveForwarding(t *testing.T) {
	s0 := newMockSession(nil)
	f := makeTestForwarder([]*mockSession{s0}, nil)

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh // ForwardPorts is running

	done := make(chan struct{})
	go func() {
		f.Close() // closes stopCh; s0.ForwardPorts selects <-s.stopCh and returns
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return within 5s during active forwarding")
	}
	awaitDone(t, f)
}

// 8. Close during reconnect backoff
func TestForwarder_CloseDuringReconnectBackoff(t *testing.T) {
	s0 := newMockSession(nil)
	// After s0 finishes, make all reconnect builds fail so the goroutine
	// enters backoff. reconnectWait is 1ms so it's almost always in the
	// time.After select, which is interrupted by stopCh.
	f := newForwarder("test-ns", "pod-0", 12345, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 100 * time.Millisecond // slightly longer to increase chance of catching backoff
	f.startupTimeout = 5 * time.Second
	f.skipDialCheck = true
	s0.stopCh = f.stopCh

	buildCalls := 0
	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		buildCalls++
		if buildCalls == 1 {
			if readyCh != nil {
				go func() {
					select {
					case <-s0.readyCh:
						select {
						case readyCh <- struct{}{}:
						default:
						}
					case <-f.stopCh:
					}
				}()
			}
			return s0, nil
		}
		return nil, fmt.Errorf("pod is not running. Status=Pending")
	}

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh
	s0.restart() // goroutine enters backoff loop

	done := make(chan struct{})
	go func() {
		f.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not return within 5s during reconnect backoff")
	}
	awaitDone(t, f)
}

// 9. Repeated Close calls must not panic.
func TestForwarder_RepeatedClose(t *testing.T) {
	s0 := newMockSession(nil)
	f := makeTestForwarder([]*mockSession{s0}, nil)

	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	// Three sequential calls; none must panic.
	f.Close()
	f.Close()
	f.Close()

	awaitDone(t, f)
}

// 10. Start/Close race — no panic; goroutine must not leak regardless of outcome.
func TestForwarder_StartCloseRace(t *testing.T) {
	s0 := newMockSession(nil)
	f := makeTestForwarder([]*mockSession{s0}, nil)
	t.Cleanup(func() { f.Close() })

	var startErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		startErr = f.Start()
	}()
	go func() {
		defer wg.Done()
		f.Close()
	}()

	wg.Wait()
	_ = startErr // may be nil or an error depending on the race outcome

	// Critical assertion: goroutine must have exited (no leak).
	awaitDone(t, f)
}

// 11. Startup timeout — if the tunnel is never established, Start() must return
//
//	a timeout error and the goroutine must exit cleanly.
func TestForwarder_StartupTimeoutCleansUp(t *testing.T) {
	f := newForwarder("test-ns", "pod-0", 12345, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 50 * time.Millisecond // fast timeout for the test
	f.skipDialCheck = true

	// buildFn returns a session whose ForwardPorts closes s.readyCh but nobody
	// forwards that signal to the forwarder's readyCh, so Start() times out.
	s := &mockSession{
		readyCh: make(chan struct{}),
		breakCh: make(chan struct{}),
		stopCh:  f.stopCh,
		ports:   []portforward.ForwardedPort{{Local: 12345, Remote: 8100}},
	}

	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		// Intentionally do NOT start the forwarding goroutine to wire s.readyCh
		// to readyCh, so the forwarder's readyCh is never signalled.
		return s, nil
	}

	err := f.Start()
	if err == nil {
		f.Close()
		t.Fatal("Start() should return a timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got: %v", err)
	}

	// Start() already called f.closeOnce.Do(close(f.stopCh)) and waited for doneCh.
	awaitDone(t, f) // doneCh already closed; returns immediately
}

// 13. GetPorts() error in Start() phase 3 — Start() must return an error and
//
//	the forwarding goroutine must exit without leaking.
func TestForwarder_GetPortsErrorAborts(t *testing.T) {
	s0 := newMockSession(nil)
	s0.portsErr = fmt.Errorf("get ports failed: internal error")

	f := makeTestForwarder([]*mockSession{s0}, nil)
	t.Cleanup(func() { f.Close() })

	err := f.Start()
	if err == nil {
		t.Fatal("Start() should return an error when GetPorts() fails")
	}
	if !strings.Contains(err.Error(), "get ports failed") {
		t.Errorf("unexpected error: %v", err)
	}

	// Start() calls closeOnce.Do and waits for doneCh before returning.
	awaitDone(t, f)
}

// 14. run() exits while Start() is blocked in the phase-4 TCP dial check.
//
//	Start() must return promptly with a "goroutine exited before" error.
//	This covers the case where the forwarding pod crashes AND reconnects
//	permanently fail before the local port becomes reachable.
func TestForwarder_GoroutineExitDuringDialCheck(t *testing.T) {
	// AllocateLocalPortE grabs a free port then releases it, so there is
	// no listener — dial attempts in phase 4 will fail immediately.
	portStr, allocErr := AllocateLocalPortE()
	if allocErr != nil {
		t.Fatalf("failed to allocate local port: %v", allocErr)
	}
	localPort, _ := strconv.Atoi(portStr)

	s0 := newMockSession(nil)
	s0.ports = []portforward.ForwardedPort{{Local: uint16(localPort), Remote: 8100}}
	// getPortsHook fires at the phase 2→3 boundary, after Start() has
	// already received from readyCh and before it enters the dial loop.
	// Calling restart() here closes s0.breakCh, which unblocks
	// s0.ForwardPorts() and starts the reconnect countdown.
	s0.getPortsHook = func() { s0.restart() }

	f := newForwarder("test-ns", "pod-0", localPort, 8100,
		func() (string, int, error) { return "pod-0", 8100, nil })
	f.reconnectWait = 1 * time.Millisecond
	f.startupTimeout = 5 * time.Second
	// skipDialCheck left false so Start() runs the phase-4 dial loop.
	s0.stopCh = f.stopCh

	buildCalls := 0
	f.buildFn = func(readyCh chan struct{}) (forwardSession, error) {
		buildCalls++
		if buildCalls == 1 {
			if readyCh != nil {
				go func() {
					select {
					case <-s0.readyCh:
						select {
						case readyCh <- struct{}{}:
						default:
						}
					case <-f.stopCh:
					}
				}()
			}
			return s0, nil
		}
		// All reconnect attempts fail permanently so run() exits quickly,
		// closing doneCh while Start() is still in the phase-4 dial loop.
		return nil, fmt.Errorf("failed to create kubernetes client: unauthorized")
	}
	t.Cleanup(func() { f.Close() })

	err := f.Start()
	if err == nil {
		t.Fatal("Start() should return an error when the goroutine exits during dial check")
	}
	if !strings.Contains(err.Error(), "goroutine exited before") {
		t.Errorf("unexpected error message: %v", err)
	}

	// doneCh was already closed when Start() received it; returns immediately.
	awaitDone(t, f)
}

// 12. ForwardPorts() error triggers reconnect.
//
//	When ForwardPorts() returns a non-nil error (e.g. a transient pod crash),
//	run() must log the error and reconnect rather than giving up.
func TestForwarder_ForwardPortsErrorTriggersReconnect(t *testing.T) {
	s0 := newMockSession(nil)
	s1 := newMockSession(nil)
	s0.fpErr = fmt.Errorf("connection reset by peer")

	f := makeTestForwarder([]*mockSession{s0, s1}, nil)
	if err := f.Start(); err != nil {
		t.Fatalf("Start() failed: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	<-s0.readyCh // s0 ForwardPorts is running
	s0.restart() // ForwardPorts() returns s0.fpErr; run() must reconnect

	select {
	case <-s1.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("forwarder did not reconnect after ForwardPorts() returned an error")
	}

	f.Close()
	awaitDone(t, f)
}
