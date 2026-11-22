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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, filesPath, nil)
}

func TestEveryFilesRouteAnswers503UntilTheVolumeIsUsable(t *testing.T) {
	api := newTestAPI(t)
	volume := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.WriteFile(volume, nil, 0o600), "a file sits where the volume should be")
	api.service.files = NewFSStore(volume)
	routes := map[string]*http.Request{
		"list":    listRequest(),
		"get":     httptest.NewRequest(http.MethodGet, filesPath+"/file-1", nil),
		"content": httptest.NewRequest(http.MethodGet, filesPath+"/file-1/content", nil),
		"delete":  httptest.NewRequest(http.MethodDelete, filesPath+"/file-1", nil),
		"upload":  uploadRequest(t, "line\n", true, map[string]string{"purpose": PurposeBatch}),
	}
	for name, req := range routes {
		rec := api.call(t, "alice", req)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Equal(t, "api_error", errorOf(t, rec)["type"], name)
		assert.Equal(t, req.Method == http.MethodPost, rec.Header().Get("Connection") == "close", "%s: a refused POST closes the connection", name)
	}

	require.NoError(t, os.Remove(volume))
	rec := api.call(t, "alice", listRequest())
	assert.Equal(t, http.StatusOK, rec.Code, "the API opens once the volume is usable, with no restart: %s", rec.Body.String())
}

// stopped makes the test wait, at cleanup, until the background storage checks have ended.
func stopped(t *testing.T, done <-chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the background storage checks did not stop")
		}
	})
}

// A check still running after its owner is gone would write into a volume that is being removed.
func TestTheStorageChecksStopWithTheirContext(t *testing.T) {
	api := newTestAPI(t)
	api.service.retryEvery = 10 * time.Millisecond
	api.service.recheckEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := api.service.Start(ctx)
	require.Eventually(t, api.service.gate.ready.Load, 5*time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the background storage checks did not stop when their context ended")
	}
}

func TestTheGateOpensByItselfWithoutRequests(t *testing.T) {
	api := newTestAPI(t)
	volume := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.WriteFile(volume, nil, 0o600), "a file sits where the volume should be")
	api.service.files = NewFSStore(volume)
	api.service.retryEvery = 20 * time.Millisecond

	stopped(t, api.service.Start(t.Context()))
	<-api.service.gate.start(t.Context())
	require.False(t, api.service.gate.ready.Load(), "the first check must fail on the broken volume")
	require.NoError(t, os.Remove(volume))
	assert.Eventually(t, api.service.gate.ready.Load, 5*time.Second, 20*time.Millisecond, "batch comes back by itself, with no request to start a check")
}

// countedVolume counts the storage checks, which each start with a probe of the volume.
type countedVolume struct {
	FileStore
	probes *atomic.Int32
}

func (v countedVolume) Probe(ctx context.Context) error {
	v.probes.Add(1)
	return v.FileStore.Probe(ctx)
}

func TestTheGateClosesWhenTheRecordsStopMatchingTheVolume(t *testing.T) {
	api := newTestAPI(t)
	probes := &atomic.Int32{}
	api.service.files = countedVolume{FileStore: api.files, probes: probes}
	require.Equal(t, http.StatusOK, api.call(t, "alice", listRequest()).Code)
	api.service.retryEvery = 20 * time.Millisecond
	api.service.recheckEvery = 20 * time.Millisecond
	stopped(t, api.service.Start(t.Context()))
	// One check came from the request; two more mean the loop kept checking while the gate was open.
	require.Eventually(t, func() bool { return probes.Load() >= 3 }, 5*time.Second, 10*time.Millisecond, "the storage is checked again while ready")
	own, err := api.mini.Get(api.store.volumeKey())
	require.NoError(t, err)

	// Redis came back empty and another volume claimed the records first.
	require.NoError(t, api.mini.Set(api.store.volumeKey(), "volume-0123456789abcdef01234567"))
	assert.Eventually(t, func() bool {
		return api.call(t, "alice", listRequest()).Code == http.StatusServiceUnavailable
	}, 5*time.Second, 20*time.Millisecond, "a ready pod notices that the records belong to another volume")

	require.NoError(t, api.mini.Set(api.store.volumeKey(), own))
	assert.Eventually(t, func() bool {
		return api.call(t, "alice", listRequest()).Code == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "the gate opens again once the records match")
}

func TestAVolumeTheRecordsDoNotBelongToIsRefused(t *testing.T) {
	first := newTestAPI(t)
	mustUpload(t, first, "alice", "line\n")
	otherFiles, otherRoot := newTestFileStore(t)
	second := &testAPI{service: NewService(first.store, otherFiles, Config{MaxFileBytes: 1024}), files: otherFiles, root: otherRoot, store: first.store, mini: first.mini}

	rec := second.call(t, "alice", listRequest())
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "a pod on another volume must not serve records whose bytes it lacks: %s", rec.Body.String())
	assert.Equal(t, "api_error", errorOf(t, rec)["type"])
	assert.Equal(t, http.StatusOK, first.call(t, "alice", listRequest()).Code, "the volume the records belong to keeps working")
}

func TestReplicasOnOneVolumeShareTheRecords(t *testing.T) {
	first := newTestAPI(t)
	id := mustUpload(t, first, "alice", "line\n")
	second := &testAPI{service: NewService(first.store, NewFSStore(first.root), Config{MaxFileBytes: 1024}), root: first.root, store: first.store, mini: first.mini}

	rec := second.call(t, "alice", httptest.NewRequest(http.MethodGet, filesPath+"/"+id+"/content", nil))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "line\n", rec.Body.String())
}

func TestVolumeIDIsWrittenOnceAndKept(t *testing.T) {
	_, root := newTestFileStore(t)
	// Replicas that start together on a new volume must still agree on one ID.
	ids := make([]string, 8)
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The ID is published whole, so no replica ever needs a second try.
			ids[i], errs[i] = NewFSStore(root).VolumeID(context.Background())
		}()
	}
	wg.Wait()
	for i := range ids {
		require.NoError(t, errs[i], "replica %d", i)
		assert.Equal(t, ids[0], ids[i])
	}

	again, err := NewFSStore(root).VolumeID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ids[0], again, "a restarted pod reads the ID that is already there")
	raw, err := os.ReadFile(filepath.Join(root, ".kthena", "volume-id"))
	require.NoError(t, err)
	assert.Equal(t, ids[0], strings.TrimSpace(string(raw)))
	entries, err := os.ReadDir(filepath.Join(root, ".kthena"))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{"volume-id"}, names, "no temporary marker is left behind")
}

// stuckVolume blocks Probe until it is released, like a network volume that stopped answering.
type stuckVolume struct {
	FileStore
	release chan struct{}
}

func (v stuckVolume) Probe(ctx context.Context) error {
	<-v.release
	return v.FileStore.Probe(ctx)
}

func TestAStuckVolumeHoldsUpNeitherStartNorRequests(t *testing.T) {
	api := newTestAPI(t)
	release := make(chan struct{})
	api.service.files = stuckVolume{FileStore: api.files, release: release}
	api.service.readyWait = 50 * time.Millisecond

	started := time.Now()
	stopped(t, api.service.Start(t.Context()))
	assert.Less(t, time.Since(started), time.Second, "Start must not wait for the volume")
	asked := time.Now()
	rec := api.call(t, "alice", listRequest())
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Less(t, time.Since(asked), 2*time.Second, "a request waits a bounded time for the check")

	close(release)
	assert.Eventually(t, func() bool {
		return api.call(t, "alice", listRequest()).Code == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "the API opens once the volume answers")
}
