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
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
)

var _ framework.ScheduleChain = &PluginChain{}

// PluginChain bundles the plugin instances one scheduling round uses.
type PluginChain struct {
	filters           []framework.FilterPlugin
	scores            []framework.WeightedScorePlugin
	postScheduleHooks []framework.PostScheduleHook
	// syncOnFlight is true when least-request is among the plugins: its
	// scoring requires on-flight counts synced from Redis first (multiple
	// router replicas each only see their own dispatches).
	syncOnFlight bool

	// builtFrom is the exact profile object this chain was built from;
	// nil means the global chain.
	builtFrom *aiv1alpha1.SchedulerProfile

	// A retired chain stays open until every request still using it has
	// finished (after RunPostHooks), then closes.
	inflight  atomic.Int64
	retired   atomic.Bool
	closeOnce sync.Once
}

// Acquire marks a request as using the chain; pair it with Release.
func (c *PluginChain) Acquire() { c.inflight.Add(1) }

func (c *PluginChain) isGlobal() bool {
	return c.builtFrom == nil
}

// ownerName identifies the chain in logs; "global" when built from no profile.
func (c *PluginChain) ownerName() string {
	if c.isGlobal() {
		return "global"
	}
	return c.builtFrom.Namespace + "/" + c.builtFrom.Name
}

// Release marks a request done; the last release on a retired chain
// triggers the close.
func (c *PluginChain) Release() {
	// The global chain is not released here; its release timing is not
	// settled yet.
	if c.isGlobal() {
		return
	}
	if c.inflight.Add(-1) < 0 {
		c.inflight.Store(0)
	}
	c.tryClose()
}

// retire marks the chain as replaced or deleted; it closes when the
// in-flight count drains to zero. Called after the chain is removed from
// the map (or never published), so no new Acquire can reach it.
func (c *PluginChain) retire() {
	c.retired.Store(true)
	klog.V(2).Infof("SchedulerPluginChain %s: retired with %d in-flight requests; close deferred until drain",
		c.ownerName(), c.inflight.Load())
	c.tryClose()
}

func (c *PluginChain) tryClose() {
	if c.retired.Load() && c.inflight.Load() == 0 {
		c.closeOnce.Do(c.close)
	}
}

func (c *PluginChain) NeedsOnFlightSync() bool                         { return c.syncOnFlight }
func (c *PluginChain) Filters() []framework.FilterPlugin               { return c.filters }
func (c *PluginChain) Scores() []framework.WeightedScorePlugin         { return c.scores }
func (c *PluginChain) PostScheduleHooks() []framework.PostScheduleHook { return c.postScheduleHooks }

// least-request scoring requires on-flight counts synced from Redis first
// (multiple router replicas each only see their own dispatches).
func chainNeedsOnFlightSync(filters []framework.FilterPlugin, scores []framework.WeightedScorePlugin) bool {
	for _, f := range filters {
		if f.Name() == plugins.LeastRequestPluginName {
			return true
		}
	}
	for _, sc := range scores {
		if sc.Plugin.Name() == plugins.LeastRequestPluginName {
			return true
		}
	}
	return false
}

func getPostScheduleHooks(scorePlugins []framework.WeightedScorePlugin) []framework.PostScheduleHook {
	var hooks []framework.PostScheduleHook
	for _, scorePlugin := range scorePlugins {
		if hook, ok := scorePlugin.Plugin.(framework.PostScheduleHook); ok {
			hooks = append(hooks, hook)
		}
	}
	return hooks
}

// close runs only after the chain is retired and drained. Plugins without
// framework.PluginCloser hold nothing and are skipped.
func (c *PluginChain) close() {
	closeBuiltPlugins(c.ownerName(), c.filters, c.scores)
	klog.V(4).Infof("SchedulerPluginChain %s: closed (filters: %s, scores: %s)",
		c.ownerName(), pluginNames(c.filters), scorePluginNames(c.scores))
}

// closeBuiltPlugins releases the resources of constructed plugin instances
// (a closed chain, or a failed/discarded build whose instances were never
// served). Plugins without framework.PluginCloser hold nothing and are
// skipped; Close must be idempotent.
func closeBuiltPlugins(owner string, filters []framework.FilterPlugin, scores []framework.WeightedScorePlugin) {
	for _, f := range filters {
		if closer, ok := f.(framework.PluginCloser); ok {
			if err := closer.Close(); err != nil {
				klog.Warningf("SchedulerPluginChain %s: failed to close filter plugin %s: %v",
					owner, f.Name(), err)
			}
		}
	}
	for _, sc := range scores {
		if closer, ok := sc.Plugin.(framework.PluginCloser); ok {
			if err := closer.Close(); err != nil {
				klog.Warningf("SchedulerPluginChain %s: failed to close score plugin %s: %v",
					owner, sc.Plugin.Name(), err)
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

func scorePluginNames(scores []framework.WeightedScorePlugin) string {
	names := make([]string, 0, len(scores))
	for _, sc := range scores {
		names = append(names, sc.Plugin.Name())
	}
	return strings.Join(names, ", ")
}

// reconcileStripes is the number of striped locks serializing per-profile
// reconcile decisions. The same key always hashes to the same stripe, so one
// profile's read-decide-commit sequence stays atomic; different profiles run
// in parallel unless their hashes collide on a stripe (harmless — they only
// occasionally serialize).
const reconcileStripes = 16

// PluginChainManager builds one plugin chain per SchedulerProfile and
// resolves ModelServer references to them.
type PluginChainManager struct {
	store datastore.Store

	// reconcileLocks serializes build/retire decisions per profile stripe.
	// Every decision reads the current Store state while holding its stripe,
	// so delayed callbacks are harmless and cannot apply an obsolete
	// reference snapshot.
	reconcileLocks [reconcileStripes]sync.Mutex

	// mu guards chains and buildErrs as one unit: a build's commit (record
	// error, or swap chain and drop the previous error) is atomic for
	// readers.
	mu          sync.RWMutex
	chains      map[types.NamespacedName]*PluginChain
	buildErrs   map[types.NamespacedName]error
	globalChain *PluginChain

	// notifyMu guards pendingNotifies: the profiles whose build status
	// changed and still need their ModelServer condition re-reported.
	// statusNotify coalesces the wake; disabled means no wake is ever sent.
	notifyMu             sync.Mutex
	pendingNotifies      map[types.NamespacedName]struct{}
	statusNotify         chan struct{}
	notificationsEnabled atomic.Bool
}

// reconcileStripe returns the stripe serializing decisions for key.
func (m *PluginChainManager) reconcileStripe(key types.NamespacedName) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(key.Namespace))
	h.Write([]byte{0})
	h.Write([]byte(key.Name))
	return &m.reconcileLocks[h.Sum32()%reconcileStripes]
}

// NewPluginChainManager registers SchedulerProfile store callbacks; the
// store must be shared with the SchedulerProfile controller.
func NewPluginChainManager(store datastore.Store) *PluginChainManager {
	m := &PluginChainManager{
		store:           store,
		chains:          make(map[types.NamespacedName]*PluginChain),
		buildErrs:       make(map[types.NamespacedName]error),
		pendingNotifies: make(map[types.NamespacedName]struct{}),
		statusNotify:    make(chan struct{}, 1),
	}
	m.notificationsEnabled.Store(true)
	store.RegisterCallback("SchedulerProfile", m.onProfileEvent)
	store.RegisterCallback("SchedulerProfileReference", m.onProfileReferenceEvent)
	return m
}

// DisableNotifications stops signaling build-status changes; the file source
// only builds chains and nothing would consume them. Must not be called
// concurrently with itself.
func (m *PluginChainManager) DisableNotifications() {
	m.notificationsEnabled.Store(false)
}

// StatusNotify signals that at least one profile's build status changed;
// nothing is sent while notifications are disabled.
func (m *PluginChainManager) StatusNotify() <-chan struct{} {
	return m.statusNotify
}

// TakePendingNotifies returns and clears the profiles whose build status
// changed.
func (m *PluginChainManager) TakePendingNotifies() []types.NamespacedName {
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	if len(m.pendingNotifies) == 0 {
		return nil
	}
	keys := make([]types.NamespacedName, 0, len(m.pendingNotifies))
	for key := range m.pendingNotifies {
		keys = append(keys, key)
	}
	clear(m.pendingNotifies)
	return keys
}

// notifyStatusNotify marks the profile and wakes the reporter; a full channel
// only coalesces the wake, the key is never dropped.
func (m *PluginChainManager) notifyStatusNotify(key types.NamespacedName) {
	if !m.notificationsEnabled.Load() {
		return
	}
	m.notifyMu.Lock()
	m.pendingNotifies[key] = struct{}{}
	m.notifyMu.Unlock()
	select {
	case m.statusNotify <- struct{}{}:
	default:
	}
}

// BuildStatus returns the profile's build outcome: built, or the last build
// error. A failed rebuild is reported even while the previous chain serves.
func (m *PluginChainManager) BuildStatus(key types.NamespacedName) (built bool, buildErr error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err, ok := m.buildErrs[key]; ok {
		return false, err
	}
	_, ok := m.chains[key]
	return ok, nil
}

// onProfileEvent reconciles only this profile. A profile is built only while
// at least one ModelServer references it.
func (m *PluginChainManager) onProfileEvent(data datastore.EventData) {
	key := data.SchedulerProfileKey
	if key.Namespace == "" && key.Name == "" {
		return
	}
	klog.V(4).Infof("SchedulerPluginChain %s: received %s event", key.String(), data.EventType)
	m.reconcileProfile(key, data.EventType != datastore.EventDelete)
}

// onProfileReferenceEvent reconciles the one profile whose reference set
// changed, using its current reference count from Store.
func (m *PluginChainManager) onProfileReferenceEvent(data datastore.EventData) {
	key := data.SchedulerProfileKey
	if key.Namespace == "" && key.Name == "" {
		return
	}
	m.reconcileProfile(key, false)
}

// reconcileProfile makes the cached chain match the current Store state.
// profileChanged is true for Profile add/update events, which must build from
// the new Profile even when a chain or previous build error is already cached.
// Reference events and AcquireProfile pass false to build only on a cache miss.
func (m *PluginChainManager) reconcileProfile(key types.NamespacedName, profileChanged bool) {
	stripe := m.reconcileStripe(key)
	stripe.Lock()
	defer stripe.Unlock()

	profile := m.store.GetSchedulerProfile(key)
	refCount := m.store.GetModelServerCountForSchedulerProfile(key)
	if profile == nil || refCount == 0 {
		m.mu.Lock()
		old, hadChain := m.chains[key]
		_, hadErr := m.buildErrs[key]
		delete(m.chains, key)
		delete(m.buildErrs, key)
		m.mu.Unlock()
		// Unreachable once deleted — must retire unconditionally, no early
		// return between the delete above and here.
		if old != nil {
			old.retire()
		}
		if hadChain || hadErr {
			m.notifyStatusNotify(key)
		}
		return
	}

	if !profileChanged {
		m.mu.RLock()
		_, hasChain := m.chains[key]
		_, hasErr := m.buildErrs[key]
		m.mu.RUnlock()
		if hasChain || hasErr {
			return
		}
	}

	m.mu.RLock()
	chain, hasChain := m.chains[key]
	m.mu.RUnlock()
	if hasChain && chain.builtFrom == profile {
		// Already built from this exact object: a delayed callback carries an
		// obsolete event, and rebuilding would only churn the serving chain.
		return
	}

	chain, err := m.buildChain(profile)
	// A profile/ref change may have landed while plugins were being built.
	// Only commit if the current state still calls for this exact version.
	cur := m.store.GetSchedulerProfile(key)
	stillReferenced := m.store.GetModelServerCountForSchedulerProfile(key) > 0
	stale := cur == nil || cur.ResourceVersion != profile.ResourceVersion || !stillReferenced
	m.mu.Lock()
	if stale {
		m.mu.Unlock()
		if err == nil {
			chain.retire()
		}
		return
	}
	var retired *PluginChain
	if err != nil {
		m.buildErrs[key] = err
	} else {
		delete(m.buildErrs, key)
		retired = m.chains[key]
		m.chains[key] = chain
	}
	m.mu.Unlock()
	// Same rule: retired unconditionally once removed, no early return
	// between the swap above and here.
	if retired != nil {
		retired.retire()
	}

	if err != nil {
		klog.Warningf("SchedulerPluginChain %s: failed to build plugin chain: %v", key.String(), err)
		m.notifyStatusNotify(key)
		return
	}
	klog.Infof("SchedulerPluginChain %s: chain built (filters: [%s], scores: [%s])",
		key.String(), pluginNames(chain.filters), scorePluginNames(chain.scores))
	m.notifyStatusNotify(key)
}

// buildChain validates the profile's plugins and instantiates the chain.
// Validation is re-run even though the webhook already checked the same
// rules, guarding against webhook/binary version skew or objects written
// while the webhook was down. On failure, already-constructed plugin
// instances are closed so a rejected profile leaks nothing.
func (m *PluginChainManager) buildChain(profile *aiv1alpha1.SchedulerProfile) (chain *PluginChain, buildErr error) {
	var filters []framework.FilterPlugin
	var scores []framework.WeightedScorePlugin
	bc := plugins.BuildContext{Profile: types.NamespacedName{Namespace: profile.Namespace, Name: profile.Name}}
	//If chain construction fails, close the already constructed plugin instance.
	defer func() {
		if chain == nil {
			closeBuiltPlugins(profile.Namespace+"/"+profile.Name, filters, scores)
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
		meta, ok := plugins.GetPlugin(name)
		if !ok || meta.NewFilter == nil {
			return nil, fmt.Errorf("plugins.filter.enabled[%d]: plugin %q cannot be used as a filter", i, name)
		}
		if seenFilters.Has(name) {
			return nil, fmt.Errorf("plugins.filter.enabled[%d]: duplicate plugin %q", i, name)
		}
		seenFilters.Insert(name)
		f, err := meta.NewFilter(bc, argsByPlugin[name])
		if err != nil {
			return nil, fmt.Errorf("plugins.filter.enabled[%d] (%s): %w", i, name, err)
		}
		if f == nil {
			return nil, fmt.Errorf("plugins.filter.enabled[%d] (%s): builder returned nil", i, name)
		}
		filters = append(filters, f)
	}

	seenScores := sets.New[string]()
	enabledScoreNames := make([]string, 0, len(profile.Spec.Plugins.Score.Enabled))
	for i, pw := range profile.Spec.Plugins.Score.Enabled {
		meta, ok := plugins.GetPlugin(pw.Name)
		if !ok || meta.NewScore == nil {
			return nil, fmt.Errorf("plugins.score.enabled[%d]: plugin %q cannot be used as a score plugin", i, pw.Name)
		}
		if seenScores.Has(pw.Name) {
			return nil, fmt.Errorf("plugins.score.enabled[%d]: duplicate plugin %q", i, pw.Name)
		}
		seenScores.Insert(pw.Name)
		if pw.Weight <= 0 {
			return nil, fmt.Errorf("plugins.score.enabled[%d] (%s): weight must be greater than 0, got %d", i, pw.Name, pw.Weight)
		}
		sc, err := meta.NewScore(bc, m.store, argsByPlugin[pw.Name])
		if err != nil {
			return nil, fmt.Errorf("plugins.score.enabled[%d] (%s): %w", i, pw.Name, err)
		}
		if sc == nil {
			return nil, fmt.Errorf("plugins.score.enabled[%d] (%s): builder returned nil", i, pw.Name)
		}
		scores = append(scores, framework.WeightedScorePlugin{
			Plugin: sc,
			Weight: int(pw.Weight),
		})
		enabledScoreNames = append(enabledScoreNames, pw.Name)
	}

	if err := plugins.ValidateScorePluginCombination(enabledScoreNames); err != nil {
		return nil, fmt.Errorf("plugins.score.enabled: %w", err)
	}

	return &PluginChain{
		filters:           filters,
		scores:            scores,
		postScheduleHooks: getPostScheduleHooks(scores),
		syncOnFlight:      chainNeedsOnFlightSync(filters, scores),
		builtFrom:         profile,
	}, nil
}

// AcquireProfile returns the chain for this ModelServer — its referenced
// profile chain, or the global chain when none is referenced — and marks it
// in use atomically so an already-retired chain is never handed out.
func (m *PluginChainManager) AcquireProfile(ms *aiv1alpha1.ModelServer) (*PluginChain, error) {
	if ms == nil || ms.Spec.TrafficPolicy == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer == nil ||
		ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile == nil {
		if ms != nil {
			klog.V(4).Infof("modelserver %s/%s: uses the global plugin chain", ms.Namespace, ms.Name)
		}
		return m.globalChain, nil
	}
	name := *ms.Spec.TrafficPolicy.LoadBalancer.SchedulerProfile
	key := types.NamespacedName{Namespace: ms.Namespace, Name: name}
	klog.V(4).Infof("modelserver %s/%s: uses SchedulerProfile chain %s", ms.Namespace, ms.Name, name)
	if chain, err := m.acquireCachedProfile(key); chain != nil || err != nil {
		return chain, err
	}

	// Store callbacks are asynchronous. Build synchronously on a referenced
	// cache miss so the first request need not wait for callback scheduling.
	m.reconcileProfile(key, false)
	if chain, err := m.acquireCachedProfile(key); chain != nil || err != nil {
		return chain, err
	}
	return nil, fmt.Errorf("SchedulerProfile %q not found in namespace %q", name, ms.Namespace)
}

// acquireCachedProfile performs a cache lookup and increments the chain's
// usage count while holding the same lock used by retirement.
func (m *PluginChainManager) acquireCachedProfile(key types.NamespacedName) (*PluginChain, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	chain, ok := m.chains[key]
	if ok {
		chain.Acquire()
		return chain, nil
	}
	if buildErr := m.buildErrs[key]; buildErr != nil {
		return nil, fmt.Errorf("SchedulerProfile %q failed to build: %w", key.String(), buildErr)
	}
	return nil, nil
}

// InitGlobalPluginChain builds the global chain from the router scheduler
// config; the default plugin set is used when routerConfig is nil.
func (m *PluginChainManager) InitGlobalPluginChain(routerConfig *conf.RouterConfiguration) error {
	scorePluginMap := map[string]int{
		"least-request": 1,
		"least-latency": 1,
		"prefix-cache":  1,
	}
	filterPlugins := []string{
		"least-request",
	}
	pluginsArgMap := map[string]runtime.RawExtension{
		"least-request": {Raw: []byte(`{"maxWaitingRequests": 10}`)},
		"least-latency": {Raw: []byte(`{"TTFTTPOTWeightFactor": 0.5}`)},
		"prefix-cache":  {Raw: []byte(`{"blockSizeToHash": 64, "maxBlocksToMatch": 128, "maxHashCacheSize": 50000, "topKMatches": 5}`)},
	}
	var err error
	if routerConfig == nil {
		// If no scheduler configuration is provided, use the default configuration
		klog.Warning("No scheduler configuration found, using default configuration")
	} else {
		scorePluginMap, filterPlugins, pluginsArgMap, err = conf.LoadSchedulerConfig(&routerConfig.Scheduler)
		if err != nil {
			return fmt.Errorf("failed to load scheduler config: %w", err)
		}
	}

	scoreNames := slices.Collect(maps.Keys(scorePluginMap))
	if err := plugins.ValidateScorePluginCombination(scoreNames); err != nil {
		klog.Warningf("Random plugin is configured along with other score plugins and will be removed: %v", err)
		delete(scorePluginMap, plugins.RandomPluginName)
	}

	// A non-positive weight leaves the plugin enabled but drops its scores
	// from pod selection, so it is rejected rather than guessed at.
	for name, weight := range scorePluginMap {
		if weight <= 0 {
			return fmt.Errorf("score plugin %q must set a weight greater than 0, got %d", name, weight)
		}
	}

	for name, arg := range pluginsArgMap {
		if name == plugins.LeastLatencyPluginName || name == plugins.LeastRequestPluginName {
			if err := plugins.ValidatePluginArgs(name, arg); err != nil {
				return fmt.Errorf("invalid %s plugin configuration: %w", name, err)
			}
		}
	}

	bc := plugins.BuildContext{}
	var filters []framework.FilterPlugin
	var scores []framework.WeightedScorePlugin

	for _, pluginName := range filterPlugins {
		meta, ok := plugins.GetPlugin(pluginName)
		if !ok || meta.NewFilter == nil {
			klog.Errorf("Failed to get plugin %s.", pluginName)
			continue
		}
		f, err := meta.NewFilter(bc, pluginsArgMap[pluginName])
		if err != nil {
			return fmt.Errorf("failed to build filter plugin %s: %w", pluginName, err)
		}
		if f == nil {
			return fmt.Errorf("failed to build filter plugin %s: builder returned nil", pluginName)
		}
		filters = append(filters, f)
	}
	for pluginName, weight := range scorePluginMap {
		meta, ok := plugins.GetPlugin(pluginName)
		if !ok || meta.NewScore == nil {
			klog.Errorf("Failed to get plugin %s.", pluginName)
			continue
		}
		sc, err := meta.NewScore(bc, m.store, pluginsArgMap[pluginName])
		if err != nil {
			return fmt.Errorf("failed to build score plugin %s: %w", pluginName, err)
		}
		if sc == nil {
			return fmt.Errorf("failed to build score plugin %s: builder returned nil", pluginName)
		}
		scores = append(scores, framework.WeightedScorePlugin{Plugin: sc, Weight: weight})
	}
	m.globalChain = &PluginChain{
		filters:           filters,
		scores:            scores,
		postScheduleHooks: getPostScheduleHooks(scores),
		syncOnFlight:      chainNeedsOnFlightSync(filters, scores),
	}
	return nil
}
