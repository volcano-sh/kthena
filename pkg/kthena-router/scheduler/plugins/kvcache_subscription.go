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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

// errKVStreamIdle ends a stream that delivered nothing for too long.
var errKVStreamIdle = errors.New("no message received in time")

const (
	// runtimeRegisterPath is the runtime sidecar endpoint a router registers
	// with. The response is the sidecar's stream of KV events, kept open for
	// as long as the router stays registered.
	runtimeRegisterPath = "/kvcache/routers/register"

	defaultRuntimePort = 9000

	// kvStreamResyncInterval is how often the streams are reconciled with the
	// datastore, in addition to every datastore Pod event.
	kvStreamResyncInterval = 10 * time.Second
	// kvStreamMinBackoff and kvStreamMaxBackoff bound the delay before a
	// stream that could not be opened, or that ended, is opened again.
	kvStreamMinBackoff = time.Second
	kvStreamMaxBackoff = 30 * time.Second
	// kvStreamIdleTimeout closes a stream that delivered nothing, not even a
	// heartbeat, for this long. Sidecars send a heartbeat every 10s, see
	// STREAM_HEARTBEAT_SECONDS in python/kthena/runtime/memory_kv_manager.py.
	kvStreamIdleTimeout = 30 * time.Second
	// kvStreamMaxLineBytes caps one message of a stream so a misbehaving
	// sidecar cannot exhaust router memory. Sidecars split their messages
	// well below it.
	kvStreamMaxLineBytes = 4 << 20
	kvStreamDialTimeout  = 3 * time.Second
	// kvStreamHeaderTimeout bounds the wait for the response headers, which
	// a sidecar sends before its block set.
	kvStreamHeaderTimeout = 10 * time.Second
)

// routerRegistrationRequest is the body posted to the runtime sidecar's
// /kvcache/routers/register endpoint. Kept in sync by hand with
// python/kthena/runtime/app.py.
type routerRegistrationRequest struct {
	RouterID string `json:"router_id"`
}

// subscriberStore is the subset of datastore.Store used to resolve the pods a
// chain streams KV events from.
type subscriberStore interface {
	GetAllPods() map[types.NamespacedName]*datastore.PodInfo
	GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer
	GetPodsByModelServer(name types.NamespacedName) ([]*datastore.PodInfo, error)
}

// kvStreamTarget identifies one incarnation of a pod and the address of its
// runtime sidecar.
type kvStreamTarget struct {
	uid types.UID
	ip  string
}

// kvStream is the stream of KV events of one pod.
type kvStream struct {
	target kvStreamTarget
	// stopped is set once the stream was cancelled because its target left
	// the datastore.
	stopped bool
	cancel  context.CancelFunc
	// done is closed once the stream has ended and dropped the pod's blocks.
	done chan struct{}
}

// kvEventSubscriber keeps one long-lived connection to the runtime sidecar of
// every pod in its scope and applies the KV events the sidecar streams back
// to the index. The router opens the connection to the pod IP it knows, so
// every event on it belongs to that pod; nothing in the stream says which pod
// it comes from, and the router exposes no endpoint for KV events.
//
// A stream starts with the pod's full block set, so the blocks of a pod are
// only kept while its stream is open: they are dropped when it ends,
// including when the pod leaves the datastore because it is deleted or not
// Ready, and are sent again as soon as a stream is reopened.
type kvEventSubscriber struct {
	store subscriberStore
	index *KVBlockMemoryIndex
	// profile scopes the streams: a profile chain streams from the pods of
	// the ModelServers referencing it; the global chain streams from all.
	profile     types.NamespacedName
	routerID    string
	runtimePort int
	client      *http.Client
	// idleTimeout and minBackoff default to kvStreamIdleTimeout and
	// kvStreamMinBackoff; tests shorten them.
	idleTimeout time.Duration
	minBackoff  time.Duration
	// trigger requests a reconciliation with the datastore, see onPodEvent.
	trigger chan struct{}

	mu sync.Mutex
	// streams is keyed by cache owner identifier.
	streams map[string]*kvStream
}

func newKVEventSubscriber(store subscriberStore, index *KVBlockMemoryIndex, profile types.NamespacedName, routerID string, runtimePort int) *kvEventSubscriber {
	return &kvEventSubscriber{
		store:       store,
		index:       index,
		profile:     profile,
		routerID:    routerID,
		runtimePort: runtimePort,
		client: &http.Client{
			// No overall timeout: a stream stays open indefinitely and is
			// bounded by kvStreamIdleTimeout instead.
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: kvStreamDialTimeout, KeepAlive: 30 * time.Second}).DialContext,
				ResponseHeaderTimeout: kvStreamHeaderTimeout,
				// Every stream holds its own connection until it ends.
				DisableKeepAlives: true,
			},
			// The events of a stream are attributed to the pod the router
			// dialed, so a stream must not be received from anywhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		idleTimeout: kvStreamIdleTimeout,
		minBackoff:  kvStreamMinBackoff,
		trigger:     make(chan struct{}, 1),
		streams:     make(map[string]*kvStream),
	}
}

// routerInstanceID identifies this router replica to sidecars. Pod names are
// only unique within a namespace, so the namespace is included to keep
// routers from different namespaces from replacing each other's stream.
func routerInstanceID() string {
	name := os.Getenv("POD_NAME")
	if name == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return "kthena-router"
		}
		name = hostname
	}
	if namespace := os.Getenv("POD_NAMESPACE"); namespace != "" {
		return namespace + "/" + name
	}
	return name
}

// chainRegistrationSequence numbers chain registrations, so a profile's
// rebuilt chain never shares a router id with the chain it replaces.
var chainRegistrationSequence atomic.Int64

// routerChainInstanceID returns the router id of a profile chain's
// subscriber: replica id + profile + sequence. Global keeps the plain id.
func routerChainInstanceID(profile types.NamespacedName) string {
	return fmt.Sprintf("%s/%s/%s/%d",
		routerInstanceID(), profile.Namespace, profile.Name,
		chainRegistrationSequence.Add(1))
}

// onPodEvent is the datastore Pod callback: a pod that is added, updated or
// removed is picked up right away instead of on the next resync.
func (s *kvEventSubscriber) onPodEvent(datastore.EventData) {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// run reconciles the streams with the datastore until ctx is done.
func (s *kvEventSubscriber) run(ctx context.Context) {
	ticker := time.NewTicker(kvStreamResyncInterval)
	defer ticker.Stop()
	for {
		s.sync(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.trigger:
		}
	}
}

// scopedOwners returns the cache owners a profile chain streams from: the
// pods of the ModelServers referencing its profile.
func (s *kvEventSubscriber) scopedOwners() map[string]struct{} {
	owners := make(map[string]struct{})
	for name, ms := range s.store.GetAllModelServers() {
		if name.Namespace != s.profile.Namespace || !servesProfile(ms, s.profile.Name) {
			continue
		}
		pods, err := s.store.GetPodsByModelServer(name)
		if err != nil {
			klog.V(4).Infof("KVCacheAware: skipping ModelServer %s: %v", name.String(), err)
			continue
		}
		for _, pod := range pods {
			owners[cacheOwnerIdentifier(pod.GetPodNamespacedName())] = struct{}{}
		}
	}
	return owners
}

// servesProfile reports whether ms references the named SchedulerProfile.
func servesProfile(ms *aiv1alpha1.ModelServer, profile string) bool {
	return ms.Spec.TrafficPolicy != nil &&
		ms.Spec.TrafficPolicy.LoadBalancer != nil &&
		ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile != nil &&
		*ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == profile
}

// targets returns the pods to stream KV events from. A profile chain streams
// only from its profile's ModelServers; the global chain streams from all.
func (s *kvEventSubscriber) targets() map[string]kvStreamTarget {
	var scoped map[string]struct{}
	if s.profile != (types.NamespacedName{}) {
		scoped = s.scopedOwners()
	}
	targets := make(map[string]kvStreamTarget)
	pods := make(map[string][]string)
	for name, info := range s.store.GetAllPods() {
		owner := cacheOwnerIdentifier(name)
		if scoped != nil {
			if _, ok := scoped[owner]; !ok {
				continue
			}
		}
		pod := info.GetPod()
		if pod == nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		ip := net.ParseIP(pod.Status.PodIP)
		if ip == nil {
			continue
		}
		targets[owner] = kvStreamTarget{uid: pod.UID, ip: ip.String()}
		pods[ip.String()] = append(pods[ip.String()], owner)
	}
	// Pods sharing an IP (e.g. hostNetwork pods on the same node) cannot be
	// told apart: a stream from that IP may come from either pod's sidecar.
	for ip, owners := range pods {
		if len(owners) > 1 {
			klog.V(4).Infof("KVCacheAware: not streaming KV events from pods %v, they share IP %s", owners, ip)
			for _, owner := range owners {
				delete(targets, owner)
			}
		}
	}
	return targets
}

// sync opens a stream for every new pod, reopens the stream of a pod whose
// incarnation or IP changed, and stops the streams of pods that are gone.
func (s *kvEventSubscriber) sync(ctx context.Context) {
	targets := s.targets()

	s.mu.Lock()
	defer s.mu.Unlock()
	for owner, stream := range s.streams {
		target, ok := targets[owner]
		if !ok && !stream.stopped {
			stream.cancel()
			stream.stopped = true
		}
		if stream.stopped && isClosed(stream.done) {
			delete(s.streams, owner)
			continue
		}
		if ok && (stream.stopped || stream.target != target) {
			stream.cancel()
			s.startLocked(ctx, owner, target, stream.done)
		}
	}
	for owner, target := range targets {
		if _, ok := s.streams[owner]; !ok {
			s.startLocked(ctx, owner, target, nil)
		}
	}
}

// startLocked starts the stream of owner from target. prev is the done channel
// of the previous stream of the same owner, which has been cancelled: the new
// stream waits for it, so the previous stream cannot drop blocks the new one
// already received.
func (s *kvEventSubscriber) startLocked(ctx context.Context, owner string, target kvStreamTarget, prev <-chan struct{}) {
	streamCtx, cancel := context.WithCancel(ctx)
	stream := &kvStream{target: target, cancel: cancel, done: make(chan struct{})}
	s.streams[owner] = stream
	go s.follow(streamCtx, owner, target.ip, prev, stream.done)
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// follow keeps the stream of owner open, reopening it with backoff, until ctx
// is done.
func (s *kvEventSubscriber) follow(ctx context.Context, owner, ip string, prev <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	if prev != nil {
		// Wait even if ctx is done: done must not be closed before prev, or
		// a stream started after this one would only wait for this one, and
		// could receive blocks that the still running previous stream drops.
		// The previous stream was cancelled, so it ends soon.
		<-prev
		if ctx.Err() != nil {
			return
		}
	}
	backoff := s.minBackoff
	for {
		start := time.Now()
		opened, err := s.stream(ctx, owner, ip)
		if opened {
			// The blocks are only known while the stream is open; the next
			// stream starts with the full block set again.
			s.index.RemoveOwner(owner)
		}
		if ctx.Err() != nil {
			return
		}
		// Pods without a runtime sidecar in memory mode are expected to
		// fail; keep this quiet.
		klog.V(4).Infof("KVCacheAware: KV events stream of pod %s (%s) ended: %v", owner, ip, err)
		if opened && time.Since(start) >= kvStreamMaxBackoff {
			backoff = s.minBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, kvStreamMaxBackoff)
	}
}

// stream registers with the sidecar at ip and applies the KV events it
// streams back to the blocks of owner until the stream ends. It reports
// whether the stream was opened, i.e. whether blocks may have been applied.
func (s *kvEventSubscriber) stream(ctx context.Context, owner, ip string) (bool, error) {
	body, err := json.Marshal(routerRegistrationRequest{RouterID: s.routerID})
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	url := "http://" + net.JoinHostPort(ip, strconv.Itoa(s.runtimePort)) + runtimeRegisterPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	klog.V(4).Infof("KVCacheAware: streaming KV events of pod %s from %s", owner, url)

	idle := time.AfterFunc(s.idleTimeout, func() { cancel(errKVStreamIdle) })
	defer idle.Stop()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), kvStreamMaxLineBytes)
	for scanner.Scan() {
		idle.Reset(s.idleTimeout)
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			// Heartbeat.
			continue
		}
		var payload KVEventPayload
		if err := json.Unmarshal(line, &payload); err != nil {
			return true, fmt.Errorf("invalid KV events message: %w", err)
		}
		if err := s.index.Apply(owner, &payload); err != nil {
			return true, fmt.Errorf("invalid KV events message: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errKVStreamIdle) {
			return true, cause
		}
		return true, err
	}
	return true, errors.New("closed by the sidecar")
}
