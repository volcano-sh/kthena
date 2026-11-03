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

package scheduler

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
)

// PluginChain bundles the plugin instances one scheduling round uses.
type PluginChain struct {
	// profile is the SchedulerProfile this chain was built from, empty for
	// the global chain; it identifies the chain in lifecycle logs.
	profile           types.NamespacedName
	filters           []framework.FilterPlugin
	scores            []*scorePlugin
	postScheduleHooks []framework.PostScheduleHook
	// syncOnFlight is true when least-request is among the plugins: its
	// scoring requires on-flight counts synced from Redis first (multiple
	// router replicas each only see their own dispatches).
	syncOnFlight bool

	// A retired chain stays open until every request still using it has
	// finished (after RunPostHooks), then closes.
	inflight  atomic.Int64
	retired   atomic.Bool
	closeOnce sync.Once
}

// Acquire marks a request as using the chain; pair it with Release.
func (c *PluginChain) Acquire() { c.inflight.Add(1) }

// Release marks a request done; the last release on a retired chain
// triggers the close.
func (c *PluginChain) Release() {
	if c.inflight.Add(-1) < 0 {
		c.inflight.Store(0)
	}
	c.tryClose()
}

// retire marks the chain as replaced or deleted; it closes when the
// in-flight count drains to zero. Must be called with the manager's mu held
// (or on a chain never published to the map).
func (c *PluginChain) retire() {
	c.retired.Store(true)
	klog.V(2).Infof("SchedulerProfile %s: plugin chain retired with %d in-flight requests; close deferred until drain",
		c.profile.String(), c.inflight.Load())
	c.tryClose()
}

func (c *PluginChain) tryClose() {
	if c.retired.Load() && c.inflight.Load() == 0 {
		c.closeOnce.Do(c.close)
	}
}

// least-request scoring requires on-flight counts synced from Redis first
// (multiple router replicas each only see their own dispatches).
func chainNeedsOnFlightSync(filters []framework.FilterPlugin, scores []*scorePlugin) bool {
	for _, f := range filters {
		if f.Name() == plugins.LeastRequestPluginName {
			return true
		}
	}
	for _, sc := range scores {
		if sc.plugin.Name() == plugins.LeastRequestPluginName {
			return true
		}
	}
	return false
}

// close runs only after the chain is retired and drained. Plugins without
// framework.PluginCloser hold nothing and are skipped.
func (c *PluginChain) close() {
	closeBuiltPlugins(c.profile, c.filters, c.scores)
	klog.V(4).Infof("SchedulerProfile %s: closed plugin chain (filters: %s, scores: %s)",
		c.profile.String(), pluginNames(c.filters), scorePluginNames(c.scores))
}

// closeBuiltPlugins releases the resources of constructed plugin instances
// (a closed chain, or a failed/discarded build whose instances were never
// served). Plugins without framework.PluginCloser hold nothing and are
// skipped; Close must be idempotent.
func closeBuiltPlugins(owner types.NamespacedName, filters []framework.FilterPlugin, scores []*scorePlugin) {
	for _, f := range filters {
		if closer, ok := f.(framework.PluginCloser); ok {
			if err := closer.Close(); err != nil {
				klog.Warningf("SchedulerProfile %s: failed to close filter plugin %s: %v",
					owner.String(), f.Name(), err)
			}
		}
	}
	for _, sc := range scores {
		if closer, ok := sc.plugin.(framework.PluginCloser); ok {
			if err := closer.Close(); err != nil {
				klog.Warningf("SchedulerProfile %s: failed to close score plugin %s: %v",
					owner.String(), sc.plugin.Name(), err)
			}
		}
	}
}

func pluginNames(filters []framework.FilterPlugin) string {
	names := make([]string, 0, len(filters))
	for _, f := range filters {
		names = append(names, f.Name())
	}
	return strings.Join(names, ", ")
}

func scorePluginNames(scores []*scorePlugin) string {
	names := make([]string, 0, len(scores))
	for _, sc := range scores {
		names = append(names, sc.plugin.Name())
	}
	return strings.Join(names, ", ")
}

// ProfileChainManager builds one plugin chain per SchedulerProfile and
// resolves ModelServer references to them.
type ProfileChainManager struct {
	store datastore.Store

	// mu guards chains and buildErrs as one unit: a build's commit (record
	// error, or swap chain and drop the previous error) is atomic for
	// readers.
	mu        sync.RWMutex
	chains    map[types.NamespacedName]*PluginChain
	buildErrs map[types.NamespacedName]error

	// buildSettled carries profile keys whose build outcome just settled.
	// Full buffer drops the hint; conditions converge on the next event.
	buildSettled chan types.NamespacedName
}

// NewProfileChainManager registers SchedulerProfile store callbacks; the
// store must be shared with the SchedulerProfile controller.
func NewProfileChainManager(store datastore.Store) *ProfileChainManager {
	m := &ProfileChainManager{
		store:        store,
		chains:       make(map[types.NamespacedName]*PluginChain),
		buildErrs:    make(map[types.NamespacedName]error),
		buildSettled: make(chan types.NamespacedName, 128),
	}
	store.RegisterCallback("SchedulerProfile", m.onProfileEvent)
	return m
}

func (m *ProfileChainManager) BuildSettled() <-chan types.NamespacedName {
	return m.buildSettled
}

func (m *ProfileChainManager) notifySettled(key types.NamespacedName) {
	select {
	case m.buildSettled <- key:
	default:
		klog.Warningf("SchedulerProfile %s: build-settled channel full, dropping the hint", key.String())
	}
}

// BuildStatus returns the profile's build outcome: built, or the last build
// error. A failed rebuild is reported even while the previous chain serves.
func (m *ProfileChainManager) BuildStatus(key types.NamespacedName) (built bool, buildErr error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err, ok := m.buildErrs[key]; ok {
		return false, err
	}
	_, ok := m.chains[key]
	return ok, nil
}

// onProfileEvent rebuilds and swaps the profile's chain on add/update,
// removes it on delete. Store callbacks run concurrently, so a commit is
// gated on the profile version: only the build of the version currently in
// the store may commit, a stale outcome is discarded — the newest event's
// build owns the final state regardless of completion order.
func (m *ProfileChainManager) onProfileEvent(data datastore.EventData) {
	if data.SchedulerProfile == nil {
		return
	}
	profile := data.SchedulerProfile
	key := types.NamespacedName{Namespace: profile.Namespace, Name: profile.Name}
	klog.V(4).Infof("SchedulerProfile %s: received %s event", key.String(), data.EventType)
	if data.EventType == datastore.EventDelete {
		m.mu.Lock()
		old, had := m.chains[key]
		delete(m.chains, key)
		delete(m.buildErrs, key)
		if had {
			// Retire under the lock so no AcquireProfile can grab the chain.
			old.retire()
		}
		m.mu.Unlock()
		m.notifySettled(key)
		klog.Infof("SchedulerProfile %s: removed; referencing ModelServers fail their requests until the reference is updated", key.String())
		return
	}

	chain, err := m.buildChain(profile)
	m.mu.Lock()
	cur := m.store.GetSchedulerProfile(key)
	stale := cur == nil || cur.ResourceVersion != profile.ResourceVersion
	var retired *PluginChain
	if !stale {
		if err != nil {
			m.buildErrs[key] = err
		} else {
			delete(m.buildErrs, key)
			retired = m.chains[key]
			m.chains[key] = chain
		}
	}
	if retired != nil {
		// Retire under the lock so in-flight requests drain before close.
		retired.retire()
	}
	m.mu.Unlock()

	if stale {
		if err == nil {
			// Never served; close now instead of leaking its plugins.
			chain.retire()
		}
		klog.V(4).Infof("SchedulerProfile %s: stale build (rv %q) discarded; the current version owns the outcome",
			key.String(), profile.ResourceVersion)
		return
	}
	if err != nil {
		klog.Warningf("SchedulerProfile %s: failed to build plugin chain: %v", key.String(), err)
		m.notifySettled(key)
		return
	}
	klog.V(4).Infof("SchedulerProfile %s: chain rebuilt (filters: [%s], scores: [%s])",
		key.String(), pluginNames(chain.filters), scorePluginNames(chain.scores))
	m.notifySettled(key)
}

// buildChain validates the profile's plugins and instantiates the chain.
// Validation is re-run even though the webhook already checked the same
// rules, guarding against webhook/binary version skew or objects written
// while the webhook was down. On failure, already-constructed plugin
// instances are closed so a rejected profile leaks nothing.
func (m *ProfileChainManager) buildChain(profile *aiv1alpha1.SchedulerProfile) (chain *PluginChain, buildErr error) {
	var filters []framework.FilterPlugin
	var scores []*scorePlugin
	//If chain construction fails, close the already constructed plugin instance.
	defer func() {
		if chain == nil {
			closeBuiltPlugins(types.NamespacedName{Namespace: profile.Namespace, Name: profile.Name}, filters, scores)
		}
	}()

	argsByPlugin := make(map[string]runtime.RawExtension, len(profile.Spec.PluginConfig))
	seenConfig := sets.New[string]()
	for i, pc := range profile.Spec.PluginConfig {
		if !plugins.IsKnownPlugin(pc.Name) {
			return nil, fmt.Errorf("pluginConfig[%d]: unknown plugin %q", i, pc.Name)
		}
		if seenConfig.Has(pc.Name) {
			return nil, fmt.Errorf("pluginConfig[%d]: duplicate plugin %q", i, pc.Name)
		}
		seenConfig.Insert(pc.Name)
		if err := plugins.ValidatePluginArgs(pc.Name, pc.Args); err != nil {
			return nil, fmt.Errorf("pluginConfig[%d] (%s): %w", i, pc.Name, err)
		}
		argsByPlugin[pc.Name] = pc.Args
	}

	seenFilters := sets.New[string]()
	for i, name := range profile.Spec.Plugins.Filter.Enabled {
		meta, ok := plugins.SchedulerPlugins[name]
		if !ok || meta.NewFilter == nil {
			return nil, fmt.Errorf("plugins.Filter.enabled[%d]: plugin %q cannot be used as a filter", i, name)
		}
		if seenFilters.Has(name) {
			return nil, fmt.Errorf("plugins.Filter.enabled[%d]: duplicate plugin %q", i, name)
		}
		seenFilters.Insert(name)
		f, err := meta.NewFilter(argsByPlugin[name])
		if err != nil {
			return nil, fmt.Errorf("plugins.Filter.enabled[%d] (%s): %w", i, name, err)
		}
		filters = append(filters, f)
	}

	seenScores := sets.New[string]()
	enabledScoreNames := make([]string, 0, len(profile.Spec.Plugins.Score.Enabled))
	for i, pw := range profile.Spec.Plugins.Score.Enabled {
		meta, ok := plugins.SchedulerPlugins[pw.Name]
		if !ok || meta.NewScore == nil {
			return nil, fmt.Errorf("plugins.Score.enabled[%d]: plugin %q cannot be used as a score plugin", i, pw.Name)
		}
		if seenScores.Has(pw.Name) {
			return nil, fmt.Errorf("plugins.Score.enabled[%d]: duplicate plugin %q", i, pw.Name)
		}
		seenScores.Insert(pw.Name)
		if pw.Weight <= 0 {
			return nil, fmt.Errorf("plugins.Score.enabled[%d] (%s): weight must be greater than 0, got %d", i, pw.Name, pw.Weight)
		}
		sc, err := meta.NewScore(m.store, argsByPlugin[pw.Name])
		if err != nil {
			return nil, fmt.Errorf("plugins.Score.enabled[%d] (%s): %w", i, pw.Name, err)
		}
		scores = append(scores, &scorePlugin{
			plugin: sc,
			weight: pw.Weight,
		})
		enabledScoreNames = append(enabledScoreNames, pw.Name)
	}

	if err := plugins.ValidateScorePluginCombination(enabledScoreNames); err != nil {
		return nil, fmt.Errorf("plugins.Score.enabled: %w", err)
	}

	return &PluginChain{
		profile:           types.NamespacedName{Namespace: profile.Namespace, Name: profile.Name},
		filters:           filters,
		scores:            scores,
		postScheduleHooks: getPostScheduleHooks(scores),
		syncOnFlight:      chainNeedsOnFlightSync(filters, scores),
	}, nil
}

// AcquireProfile resolves the chain for the ModelServer's profile reference
// and marks it in use atomically (lookup + Acquire under the same lock the
// retire path uses), so an already-retired chain is never handed out. The
// chain may still be retired later while in use; Release keeps it open
// until this request is done. A ModelServer without a reference returns
// (nil, nil).
func (m *ProfileChainManager) AcquireProfile(ms *aiv1alpha1.ModelServer) (*PluginChain, error) {
	if ms == nil || ms.Spec.TrafficPolicy == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == nil {
		return nil, nil
	}
	name := *ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile
	key := types.NamespacedName{Namespace: ms.Namespace, Name: name}
	m.mu.Lock()
	defer m.mu.Unlock()
	chain, ok := m.chains[key]
	if ok {
		// In-map chains are never retired; the increment under the lock
		// makes a concurrent retire wait for this request's Release.
		chain.Acquire()
		return chain, nil
	}
	if buildErr := m.buildErrs[key]; buildErr != nil {
		return nil, fmt.Errorf("SchedulerProfile %q failed to build: %w", key.String(), buildErr)
	}
	return nil, fmt.Errorf("SchedulerProfile %q not found in namespace %q", name, ms.Namespace)
}
