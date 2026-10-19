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

package discovery

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	api "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

// EndpointDiscovery owns background discovery and Store synchronization. Resource
// sources feed ModelServer lifecycle events to Controller; providers only supply
// endpoint snapshots. No discovery I/O occurs on the request path.
type EndpointDiscovery interface {
	Run(context.Context) error
	Close()
}

type target struct {
	mu         sync.Mutex
	parameters map[string]interface{}
	current    *subscription
}

type subscription struct {
	ms      *api.ModelServer
	version uint64
	last    []api.Endpoint // Raw snapshot; omitted ports follow the current ModelServer.
	ctx     context.Context
	cancel  context.CancelFunc
	wake    chan struct{}
	synced  bool
}

// Controller is shared by Kubernetes-backed and file-backed configuration. The
// target map is immutable. Store writes are serialized per ModelServer, without
// synchronously scraping runtime metrics when the Store uses deferred inspection.
type Controller struct {
	store                  datastore.Store
	factory                Factory
	targets                map[types.NamespacedName]*target
	interval, timeout      time.Duration
	minRefreshInterval     time.Duration
	requireInitialSnapshot bool
	ctx                    context.Context
	cancel                 context.CancelFunc
	slots                  chan struct{}
	mu                     sync.RWMutex
	closed                 bool
	workers                sync.WaitGroup
}

var _ EndpointDiscovery = (*Controller)(nil)

func NewController(config Config, store datastore.Store, registry Registry) (*Controller, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if !config.External() {
		return nil, fmt.Errorf("discovery controller requires an external provider")
	}
	factory := registry[config.Provider]
	if factory == nil {
		return nil, fmt.Errorf("endpoint discovery provider %q is not registered", config.Provider)
	}
	interval, timeout, err := config.durations()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	concurrency := config.MaxConcurrentSnapshots
	if concurrency == 0 {
		concurrency = 4
	}
	c := &Controller{store: store, factory: factory, interval: interval, timeout: timeout,
		minRefreshInterval:     min(time.Second, interval),
		requireInitialSnapshot: config.RequireInitialSnapshot == nil || *config.RequireInitialSnapshot,
		ctx:                    ctx, cancel: cancel, slots: make(chan struct{}, concurrency), targets: make(map[types.NamespacedName]*target)}
	for _, mapping := range config.Mappings {
		parts := strings.Split(mapping.ModelServer, "/")
		c.targets[types.NamespacedName{Namespace: parts[0], Name: parts[1]}] = &target{parameters: mapping.Parameters}
	}
	return c, nil
}

// Run binds the controller lifetime to ctx. UpdateModelServer may enqueue work
// before Run starts. Close is also safe before Run, and is idempotent.
func (c *Controller) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
	case <-c.ctx.Done():
	}
	c.Close()
	return nil
}

func (c *Controller) Close() {
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.workers.Wait()
}

// HasSynced reports initialization of existing mapped ModelServers, including
// successful empty snapshots. The server latches this after resource initialization.
func (c *Controller) HasSynced() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return false
	}
	if !c.requireInitialSnapshot {
		return true
	}
	for _, t := range c.targets {
		t.mu.Lock()
		pending := t.current != nil && !t.current.synced
		t.mu.Unlock()
		if pending {
			return false
		}
	}
	return true
}

// WarnMissingModelServers is called once after the resource source initializes.
// Missing resources do not gate readiness, but a misspelled mapping needs a diagnostic.
func (c *Controller) WarnMissingModelServers() {
	for name, t := range c.targets {
		t.mu.Lock()
		missing := t.current == nil
		t.mu.Unlock()
		if missing {
			klog.Warningf("endpoint discovery mapping %s has no ModelServer resource; waiting for creation", name)
		}
	}
}

// UpdateModelServer accepts resource metadata without trusting its selector or
// static endpoints as membership for mapped targets. Unmapped static endpoints
// still work; unmapped selectors have an empty pool, never a Pod-discovery fallback.
func (c *Controller) UpdateModelServer(ms *api.ModelServer) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return fmt.Errorf("endpoint discovery controller is closed")
	}
	ms = ms.DeepCopy()
	name := utils.GetNamespaceName(ms)
	t := c.targets[name]
	if t == nil {
		if len(ms.Spec.Endpoints) == 0 {
			klog.Warningf("ModelServer %s has no endpoint discovery mapping or static endpoints; its pool remains empty", name)
		}
		return SyncEndpoints(c.store, ms, ms.Spec.Endpoints)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	sub := t.current
	if len(ms.Spec.Endpoints) > 0 && (sub == nil || !reflect.DeepEqual(sub.ms.Spec.Endpoints, ms.Spec.Endpoints)) {
		klog.Warningf("ModelServer %s is managed by endpoint discovery; spec.endpoints is ignored", name)
	}
	if sub != nil && sub.ms.UID != ms.UID {
		sub.cancel()
		t.current = nil
		if err := c.store.DeleteModelServer(name); err != nil {
			return err
		}
		sub = nil
	}
	if sub == nil {
		if err := c.store.AddOrUpdateModelServer(ms, sets.New[types.NamespacedName]()); err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(c.ctx)
		sub = &subscription{ms: ms, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
		t.current = sub
		metrics.DefaultMetrics.SetDiscoverySnapshot(name.String(), 0, 0)
		c.workers.Add(1)
		go c.watch(name, t, sub)
		return nil
	}
	if reflect.DeepEqual(sub.ms, ms) {
		return nil
	}
	if reflect.DeepEqual(sub.ms.Spec, ms.Spec) {
		if err := c.store.AddOrUpdateModelServer(ms, nil); err != nil {
			return err
		}
		sub.ms = ms
		return nil
	}
	// Reapply the raw snapshot immediately. Store invalidates changed runtime
	// data in place, preserving membership and in-flight request counters.
	if err := SyncEndpoints(c.store, ms, sub.last); err != nil {
		return err
	}
	sub.ms = ms
	sub.version++
	signal(sub.wake)
	return nil
}

func (c *Controller) DeleteModelServer(name types.NamespacedName) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if t := c.targets[name]; t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.current != nil {
			t.current.cancel()
			t.current = nil
		}
	}
	if err := c.store.DeleteModelServer(name); err != nil {
		return err
	}
	metrics.DefaultMetrics.DeleteDiscoverySnapshot(name.String())
	return nil
}

func signal(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (c *Controller) watch(name types.NamespacedName, t *target, sub *subscription) {
	defer c.workers.Done()
	var source Source
	defer func() {
		if source != nil {
			source.Close()
		}
	}()
	retry := wait.Backoff{Duration: c.minRefreshInterval, Factor: 2, Cap: c.interval, Steps: 64}
	for {
		if err := c.refresh(name, t, sub, &source); err != nil && sub.ctx.Err() == nil {
			klog.Warningf("endpoint discovery %s: %v; retaining last successful snapshot", name, err)
		}
		t.mu.Lock()
		synced := sub.synced
		t.mu.Unlock()
		delay := c.interval
		if !synced {
			delay = retry.Step()
		}
		// Spread periodic queries across replicas; initial retries stay bounded.
		delay = max(c.minRefreshInterval, wait.Jitter(delay, 0.2)-delay/10)
		if !synced {
			delay = min(delay, c.interval)
		}
		nextRefresh := time.Now().Add(c.minRefreshInterval)
		timer := time.NewTimer(delay)
		select {
		case <-sub.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-sub.wake:
			// Notifications may arrive before the minimum refresh interval.
			timer.Stop()
			timer.Reset(max(0, time.Until(nextRefresh)))
			select {
			case <-sub.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		timer.Stop()
		select {
		case <-sub.wake:
		default:
		}
	}
}

func (c *Controller) refresh(name types.NamespacedName, t *target, sub *subscription, source *Source) (err error) {
	start := time.Now()
	result := "discarded"
	defer func() {
		if err != nil && sub.ctx.Err() == nil {
			result = "error"
		}
		metrics.DefaultMetrics.RecordDiscoveryRefresh(name.String(), result, time.Since(start))
	}()
	t.mu.Lock()
	if t.current != sub || sub.ctx.Err() != nil {
		t.mu.Unlock()
		return nil
	}
	version := sub.version
	t.mu.Unlock()
	endpoints, err := c.snapshot(name, t, sub, source)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current != sub || sub.ctx.Err() != nil || sub.version != version {
		return nil
	}
	// Metadata-only updates do not invalidate a snapshot; always publish the
	// latest resource metadata rather than the copy from before the lookup.
	ms := sub.ms
	if err := validateSnapshot(ms, endpoints, sub.last); err != nil {
		return err
	}
	snapshot := make([]api.Endpoint, len(endpoints))
	for i := range endpoints {
		snapshot[i] = *endpoints[i].DeepCopy()
	}
	if err := SyncEndpoints(c.store, ms, snapshot); err != nil {
		return err
	}
	sub.last = snapshot
	sub.synced = true
	metrics.DefaultMetrics.SetDiscoverySnapshot(name.String(), len(snapshot), float64(time.Now().UnixNano())/float64(time.Second))
	result = "success"
	return nil
}

func (c *Controller) snapshot(name types.NamespacedName, t *target, sub *subscription, source *Source) ([]api.Endpoint, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-sub.ctx.Done():
		return nil, sub.ctx.Err()
	}
	if err := sub.ctx.Err(); err != nil {
		return nil, err
	}
	if *source == nil {
		s, err := c.factory(name, t.parameters, func() { signal(sub.wake) })
		if err != nil {
			return nil, err
		}
		if s == nil {
			return nil, fmt.Errorf("provider returned a nil source")
		}
		*source = s
	}
	ctx, cancel := context.WithTimeout(sub.ctx, c.timeout)
	defer cancel()
	endpoints, err := (*source).Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return endpoints, nil
}
