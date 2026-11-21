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

func TestTheGateOpensByItselfWithoutRequests(t *testing.T) {
	api := newTestAPI(t)
	volume := filepath.Join(t.TempDir(), "volume")
	require.NoError(t, os.WriteFile(volume, nil, 0o600), "a file sits where the volume should be")
	api.service.files = NewFSStore(volume)
	api.service.retryEvery = 20 * time.Millisecond
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)

	api.service.Start(ctx)
	<-api.service.gate.start(ctx)
	require.False(t, api.service.gate.ready.Load(), "the first check must fail on the broken volume")
	require.NoError(t, os.Remove(volume))
	assert.Eventually(t, api.service.gate.ready.Load, 5*time.Second, 20*time.Millisecond, "batch comes back by itself, with no request to start a check")
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
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A replica that reads the file while another one writes it tries again, as the gate does.
			for try := 0; try < 100 && ids[i] == ""; try++ {
				if id, err := NewFSStore(root).VolumeID(context.Background()); err == nil {
					ids[i] = id
				} else {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	require.NotEmpty(t, ids[0])
	for _, id := range ids {
		assert.Equal(t, ids[0], id)
	}

	again, err := NewFSStore(root).VolumeID(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ids[0], again, "a restarted pod reads the ID that is already there")
	raw, err := os.ReadFile(filepath.Join(root, ".kthena", "volume-id"))
	require.NoError(t, err)
	assert.Equal(t, ids[0], strings.TrimSpace(string(raw)))
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
	api.service.Start(context.Background())
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
