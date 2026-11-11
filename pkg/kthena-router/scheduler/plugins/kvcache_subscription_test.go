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

package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type fakeSubscriberStore struct {
	mu           sync.Mutex
	pods         map[types.NamespacedName]*datastore.PodInfo
	modelServers map[types.NamespacedName]*aiv1alpha1.ModelServer
	msPods       map[types.NamespacedName][]types.NamespacedName
}

func newFakeSubscriberStore() *fakeSubscriberStore {
	return &fakeSubscriberStore{
		pods:         make(map[types.NamespacedName]*datastore.PodInfo),
		modelServers: make(map[types.NamespacedName]*aiv1alpha1.ModelServer),
		msPods:       make(map[types.NamespacedName][]types.NamespacedName),
	}
}

func (f *fakeSubscriberStore) GetAllPods() map[types.NamespacedName]*datastore.PodInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	pods := make(map[types.NamespacedName]*datastore.PodInfo, len(f.pods))
	for name, info := range f.pods {
		pods[name] = info
	}
	return pods
}

func (f *fakeSubscriberStore) add(name, uid, ip string, phase corev1.PodPhase) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID(uid)}}
	pod.Status.PodIP = ip
	pod.Status.Phase = phase
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[types.NamespacedName{Namespace: "default", Name: name}] = datastore.NewPodInfo(pod, "vLLM")
}

func (f *fakeSubscriberStore) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, types.NamespacedName{Namespace: "default", Name: name})
}

// addModelServer registers a ModelServer in the default namespace; an empty
// profile leaves its schedulerProfile unset.
func (f *fakeSubscriberStore) addModelServer(name, profile string, podNames ...string) {
	ms := &aiv1alpha1.ModelServer{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	if profile != "" {
		ms.Spec.TrafficPolicy = &aiv1alpha1.TrafficPolicy{
			LoadBalancer: &aiv1alpha1.LoadBalancer{SchedulerProfile: &profile},
		}
	}
	key := types.NamespacedName{Namespace: "default", Name: name}
	podKeys := make([]types.NamespacedName, 0, len(podNames))
	for _, podName := range podNames {
		podKeys = append(podKeys, types.NamespacedName{Namespace: "default", Name: podName})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelServers[key] = ms
	f.msPods[key] = podKeys
}

func (f *fakeSubscriberStore) GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	servers := make(map[types.NamespacedName]*aiv1alpha1.ModelServer, len(f.modelServers))
	for name, ms := range f.modelServers {
		servers[name] = ms
	}
	return servers
}

func (f *fakeSubscriberStore) GetPodsByModelServer(name types.NamespacedName) ([]*datastore.PodInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	podKeys, ok := f.msPods[name]
	if !ok {
		return nil, fmt.Errorf("model server not found: %v", name)
	}
	pods := make([]*datastore.PodInfo, 0, len(podKeys))
	for _, podKey := range podKeys {
		if pod, ok := f.pods[podKey]; ok {
			pods = append(pods, pod)
		}
	}
	return pods, nil
}

// fakeStream is one KV events stream served by a fakeSidecar.
type fakeStream struct {
	host     string
	routerID string
	lines    chan string
	end      chan struct{}
	closed   chan struct{}
}

func (s *fakeStream) send(t *testing.T, payload KVEventPayload) {
	t.Helper()
	line, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case s.lines <- string(line):
	case <-s.closed:
		t.Fatalf("stream of %s closed", s.host)
	}
}

// fakeSidecar serves KV events streams on every local address; the address a
// router dials tells the pod apart.
type fakeSidecar struct {
	server  *httptest.Server
	port    int
	streams chan *fakeStream
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := &fakeSidecar{streams: make(chan *fakeStream, 16)}
	sidecar.server = httptest.NewUnstartedServer(http.HandlerFunc(sidecar.serve))
	sidecar.server.Listener = listener
	sidecar.server.Start()
	t.Cleanup(sidecar.server.Close)
	sidecar.port = listener.Addr().(*net.TCPAddr).Port
	return sidecar
}

func (f *fakeSidecar) serve(w http.ResponseWriter, r *http.Request) {
	var body routerRegistrationRequest
	if r.Method != http.MethodPost || r.URL.Path != runtimeRegisterPath || json.NewDecoder(r.Body).Decode(&body) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	host, _, _ := net.SplitHostPort(r.Host)
	stream := &fakeStream{
		host:     host,
		routerID: body.RouterID,
		lines:    make(chan string),
		end:      make(chan struct{}),
		closed:   make(chan struct{}),
	}
	defer close(stream.closed)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	f.streams <- stream
	for {
		select {
		case line := <-stream.lines:
			fmt.Fprintln(w, line)
			w.(http.Flusher).Flush()
		case <-stream.end:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (f *fakeSidecar) nextStream(t *testing.T) *fakeStream {
	t.Helper()
	select {
	case stream := <-f.streams:
		return stream
	case <-time.After(5 * time.Second):
		t.Fatal("no KV events stream was opened")
		return nil
	}
}

func (f *fakeSidecar) expectNoStream(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case stream := <-f.streams:
		t.Fatalf("unexpected KV events stream from %s", stream.host)
	case <-time.After(wait):
	}
}

func storedEvent(model string, hashes ...uint64) KVEventPayload {
	return KVEventPayload{ModelName: model, Events: []KVEvent{{Type: kvEventStored, BlockHashes: hashes}}}
}

func waitForOwners(t *testing.T, idx *KVBlockMemoryIndex, model string, hash uint64, want ...string) {
	t.Helper()
	slices.Sort(want)
	var got []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = got[:0]
		for owner := range idx.GetBlockOwners(model, []uint64{hash})[hash] {
			got = append(got, owner)
		}
		slices.Sort(got)
		if slices.Equal(got, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owners of block %d of %s = %v, want %v", hash, model, got, want)
}

func startTestSubscriber(t *testing.T, pods subscriberStore, idx *KVBlockMemoryIndex, port int) (*kvEventSubscriber, context.CancelFunc) {
	t.Helper()
	subscriber := newKVEventSubscriber(pods, idx, types.NamespacedName{}, "kthena-system/router-0", port)
	subscriber.minBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go subscriber.run(ctx)
	return subscriber, cancel
}

func TestKVEventSubscriber_AttributesEventsToDialedPod(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	pods.add("pod-b", "uid-b", "127.0.0.2", corev1.PodRunning)
	pods.add("pending", "uid-p", "127.0.0.3", corev1.PodPending)
	pods.add("no-ip", "uid-n", "", corev1.PodRunning)
	idx := NewKVBlockMemoryIndex(0)
	startTestSubscriber(t, pods, idx, sidecar.port)

	streams := map[string]*fakeStream{}
	for range 2 {
		stream := sidecar.nextStream(t)
		if stream.routerID != "kthena-system/router-0" {
			t.Errorf("router_id = %q, want %q", stream.routerID, "kthena-system/router-0")
		}
		streams[stream.host] = stream
	}
	sidecar.expectNoStream(t, 100*time.Millisecond)

	streams["127.0.0.1"].send(t, storedEvent("qwen", 1))
	streams["127.0.0.2"].send(t, storedEvent("qwen", 1, 2))
	waitForOwners(t, idx, "qwen", 1, "pod-a.default", "pod-b.default")
	waitForOwners(t, idx, "qwen", 2, "pod-b.default")
}

func TestKVEventSubscriber_SkipsPodsSharingAnIP(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("host-1", "uid-1", "127.0.0.1", corev1.PodRunning)
	pods.add("host-2", "uid-2", "127.0.0.1", corev1.PodRunning)
	subscriber, _ := startTestSubscriber(t, pods, NewKVBlockMemoryIndex(0), sidecar.port)
	sidecar.expectNoStream(t, 200*time.Millisecond)

	pods.remove("host-2")
	subscriber.onPodEvent(datastore.EventData{})
	if stream := sidecar.nextStream(t); stream.host != "127.0.0.1" {
		t.Fatalf("stream opened to %s, want 127.0.0.1", stream.host)
	}
}

// A pod that is temporarily not Ready leaves the datastore. Its blocks are
// dropped with its stream, and the stream opened once it is back resends
// them, so its warm cache is known again.
func TestKVEventSubscriber_ResyncsPodBackFromNotReady(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	idx := NewKVBlockMemoryIndex(0)
	subscriber, _ := startTestSubscriber(t, pods, idx, sidecar.port)

	stream := sidecar.nextStream(t)
	stream.send(t, storedEvent("qwen", 1))
	waitForOwners(t, idx, "qwen", 1, "pod-a.default")

	pods.remove("pod-a")
	subscriber.onPodEvent(datastore.EventData{})
	select {
	case <-stream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream of a pod that left the datastore was not closed")
	}
	waitForOwners(t, idx, "qwen", 1)

	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	subscriber.onPodEvent(datastore.EventData{})
	stream = sidecar.nextStream(t)
	stream.send(t, storedEvent("qwen", 1))
	waitForOwners(t, idx, "qwen", 1, "pod-a.default")
}

func TestKVEventSubscriber_ReopensStreamOfReplacedPod(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-1", "127.0.0.1", corev1.PodRunning)
	idx := NewKVBlockMemoryIndex(0)
	subscriber, _ := startTestSubscriber(t, pods, idx, sidecar.port)

	old := sidecar.nextStream(t)
	old.send(t, storedEvent("qwen", 1))
	waitForOwners(t, idx, "qwen", 1, "pod-a.default")

	// Same name, new incarnation on another IP.
	pods.add("pod-a", "uid-2", "127.0.0.2", corev1.PodRunning)
	subscriber.onPodEvent(datastore.EventData{})
	stream := sidecar.nextStream(t)
	if stream.host != "127.0.0.2" {
		t.Fatalf("stream opened to %s, want 127.0.0.2", stream.host)
	}
	stream.send(t, storedEvent("qwen", 2))
	waitForOwners(t, idx, "qwen", 2, "pod-a.default")
	waitForOwners(t, idx, "qwen", 1)
	select {
	case <-old.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream of the replaced pod was not closed")
	}
	// Ending the old stream did not drop the blocks of the new one.
	waitForOwners(t, idx, "qwen", 2, "pod-a.default")
}

// A stream replaced while it still waits for its own predecessor must keep
// waiting for it: otherwise the next stream, which only waits for the
// replaced one, could receive blocks the predecessor drops when it ends.
func TestKVEventSubscriber_CancelledStreamWaitsForItsPredecessor(t *testing.T) {
	subscriber := newKVEventSubscriber(newFakeSubscriberStore(), NewKVBlockMemoryIndex(0), types.NamespacedName{}, "router-0", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prev := make(chan struct{})
	done := make(chan struct{})
	go subscriber.follow(ctx, "pod-a.default", "127.0.0.1", prev, done)

	select {
	case <-done:
		t.Fatal("cancelled stream ended before its predecessor")
	case <-time.After(100 * time.Millisecond):
	}
	close(prev)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled stream did not end after its predecessor")
	}
}

func TestKVEventSubscriber_DoesNotFollowRedirects(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("registration was redirected to %s", r.URL)
	}))
	t.Cleanup(elsewhere.Close)
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+runtimeRegisterPath, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(sidecar.Close)
	port := sidecar.Listener.Addr().(*net.TCPAddr).Port

	subscriber := newKVEventSubscriber(newFakeSubscriberStore(), NewKVBlockMemoryIndex(0), types.NamespacedName{}, "router-0", port)
	opened, err := subscriber.stream(context.Background(), "pod-a.default", "127.0.0.1")
	if opened || err == nil {
		t.Fatalf("stream() = %v, %v; want a failure to open the stream", opened, err)
	}
}

func TestKVEventSubscriber_DropsBlocksWhenStreamEnds(t *testing.T) {
	tests := []struct {
		name string
		end  func(t *testing.T, stream *fakeStream)
	}{
		{
			name: "closed by the sidecar",
			end:  func(t *testing.T, stream *fakeStream) { close(stream.end) },
		},
		{
			name: "invalid message",
			end: func(t *testing.T, stream *fakeStream) {
				stream.lines <- `{"model_name": "qwen", "events": [{"type": "bogus"}]}`
			},
		},
		{
			name: "malformed message",
			end:  func(t *testing.T, stream *fakeStream) { stream.lines <- `{` },
		},
		{
			name: "message too long",
			end: func(t *testing.T, stream *fakeStream) {
				stream.lines <- strconv.Quote(string(make([]byte, kvStreamMaxLineBytes)))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sidecar := newFakeSidecar(t)
			pods := newFakeSubscriberStore()
			pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
			idx := NewKVBlockMemoryIndex(0)
			startTestSubscriber(t, pods, idx, sidecar.port)

			stream := sidecar.nextStream(t)
			stream.send(t, storedEvent("qwen", 1))
			waitForOwners(t, idx, "qwen", 1, "pod-a.default")

			tt.end(t, stream)
			waitForOwners(t, idx, "qwen", 1)
			// The pod is still in the datastore, so the stream is reopened.
			stream = sidecar.nextStream(t)
			stream.send(t, storedEvent("qwen", 1))
			waitForOwners(t, idx, "qwen", 1, "pod-a.default")
		})
	}
}

func TestKVEventSubscriber_IdleStreamIsClosed(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	idx := NewKVBlockMemoryIndex(0)
	subscriber := newKVEventSubscriber(pods, idx, types.NamespacedName{}, "router-0", sidecar.port)
	subscriber.minBackoff = 10 * time.Millisecond
	subscriber.idleTimeout = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go subscriber.run(ctx)

	stream := sidecar.nextStream(t)
	stream.send(t, storedEvent("qwen", 1))
	// Heartbeats keep the stream open.
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		stream.lines <- ""
	}
	waitForOwners(t, idx, "qwen", 1, "pod-a.default")

	select {
	case <-stream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("idle stream was not closed")
	}
	waitForOwners(t, idx, "qwen", 1)
	sidecar.nextStream(t)
}

func TestKVEventSubscriber_StopsWithContext(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	idx := NewKVBlockMemoryIndex(0)
	_, cancel := startTestSubscriber(t, pods, idx, sidecar.port)

	stream := sidecar.nextStream(t)
	stream.send(t, storedEvent("qwen", 1))
	waitForOwners(t, idx, "qwen", 1, "pod-a.default")
	cancel()
	select {
	case <-stream.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream was not closed with the context")
	}
	waitForOwners(t, idx, "qwen", 1)
}

func TestRouterInstanceID(t *testing.T) {
	tests := []struct {
		name      string
		podName   string
		namespace string
		want      string
	}{
		{name: "namespace and pod name", podName: "router-0", namespace: "kthena-system", want: "kthena-system/router-0"},
		{name: "pod name only", podName: "router-0", want: "router-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("POD_NAME", tt.podName)
			t.Setenv("POD_NAMESPACE", tt.namespace)
			if got := routerInstanceID(); got != tt.want {
				t.Errorf("routerInstanceID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A profile chain streams only from the pods of its profile's ModelServers.
func TestKVEventSubscriber_ScopesTargetsToProfileModelServers(t *testing.T) {
	sidecar := newFakeSidecar(t)
	pods := newFakeSubscriberStore()
	pods.add("pod-a", "uid-a", "127.0.0.1", corev1.PodRunning)
	pods.add("pod-b", "uid-b", "127.0.0.2", corev1.PodRunning)
	pods.addModelServer("ms-a", "cache-heavy", "pod-a")
	pods.addModelServer("ms-b", "", "pod-b")
	profile := types.NamespacedName{Namespace: "default", Name: "cache-heavy"}

	subscriber := newKVEventSubscriber(pods, NewKVBlockMemoryIndex(0), profile, "router-0", sidecar.port)
	subscriber.minBackoff = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go subscriber.run(ctx)

	streamA := sidecar.nextStream(t)
	if streamA.host != "127.0.0.1" {
		t.Fatalf("stream opened to %s, want 127.0.0.1", streamA.host)
	}
	sidecar.expectNoStream(t, 100*time.Millisecond)

	// A pod joining a referenced ModelServer opens its stream.
	pods.add("pod-c", "uid-c", "127.0.0.3", corev1.PodRunning)
	pods.addModelServer("ms-a", "cache-heavy", "pod-a", "pod-c")
	subscriber.onPodEvent(datastore.EventData{})
	streamC := sidecar.nextStream(t)
	if streamC.host != "127.0.0.3" {
		t.Fatalf("stream opened to %s, want 127.0.0.3", streamC.host)
	}

	// The ModelServer switching to another profile closes the streams of
	// its pods: they are no longer in this chain's scope.
	pods.addModelServer("ms-a", "other-profile", "pod-a", "pod-c")
	subscriber.onPodEvent(datastore.EventData{})
	for _, stream := range []*fakeStream{streamA, streamC} {
		select {
		case <-stream.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("stream of a pod that left the scope was not closed")
		}
	}
	sidecar.expectNoStream(t, 100*time.Millisecond)
}

// Successive registrations of the same profile chain must not share a router
// id: while a replaced chain drains, the two instances coexist.
func TestRouterChainInstanceID(t *testing.T) {
	t.Setenv("POD_NAME", "router-0")
	t.Setenv("POD_NAMESPACE", "kthena-system")
	profile := types.NamespacedName{Namespace: "default", Name: "cache-heavy"}

	first := routerChainInstanceID(profile)
	second := routerChainInstanceID(profile)
	if first == second {
		t.Errorf("routerChainInstanceID(%v) repeated %q; coexisting chain instances must not share an id", profile, first)
	}
	prefix := "kthena-system/router-0/default/cache-heavy/"
	if !strings.HasPrefix(first, prefix) {
		t.Errorf("routerChainInstanceID(%v) = %q, want the %q prefix", profile, first, prefix)
	}
}
