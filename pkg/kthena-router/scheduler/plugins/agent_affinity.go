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

/*
Agent Affinity Score Plugin

The program-affinity scoring plugin complements kvcache-aware and prefix-cache
by adding program-level semantic awareness to pod ranking. When a request carries
agent_hints.program_id, the plugin scores pods that already serve the same program
higher than unrelated pods, so cross-session prefix blocks (system prompts, tool
definitions) share KV cache blocks on the same ServingPod.

See Issue #1828 §2 for the full RFC.

Scoring model
------------

The plugin computes a score in [0, 100] for each candidate pod.

  • If the request has no program_id or no hints at all → return nil so the
    scheduler ignores this plugin and the workload falls back to pure hash-based
    or least-request routing.

  • For each candidate pod:
      score = AffinityScore
                + LoadBonus if the pod's running-request count is below the
                  configurable load threshold.

  • If the pod's load exceeds `maxRunningRequests` it is downgraded to 0 and
    the scheduler picks up the fallback plugin chain.

Redis coordination
------------------

The plugin maintains a `program_id -> ServingPod` mapping in the Redis state store
using the same client initialisation pattern as the kvcache-aware plugin:

  key   = "matrix:agent:program:<program_id>"
  value = HASH { "<podName.namespace>": "<unix_timestamp>" }
  TTL   = sliding; refreshed on every successful program-affinity score

This sliding TTL lets the plugin tolerate pod restarts — the stale field filter
mirrors kvcache-aware's own `freshOwners` logic.

Backward compatibility
----------------------

This plugin is strictly opt-in: absent agent_hints → no-op, absent Redis → no-op,
pod above load threshold → 0. The plugin never blocks scheduling.
*/

package plugins

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// AgentAffinityPluginName is the registered plugin name used in scheduler configs.
const AgentAffinityPluginName = "agent-affinity"

// Redis key prefix for program->pod ownership.
// Format: matrix:agent:program:<program_id>
const agentAffinityKeyPrefix = "matrix:agent:program:"

// Default configuration values.
const (
	// defaultAffinityScore is the base score awarded to pods currently known to
	// serve the same program_id.
	defaultAffinityScore = 80

	// defaultLoadBonus is added to the affinity score when the pod's
	// RequestRunningNum is below maxRunningRequests — it rewards "affinity +
	// headroom" pods.
	defaultLoadBonus = 15

	// defaultMaxRunningRequests is the load ceiling above which a known-affine
	// pod is downgraded to 0 (hard cap).
	defaultMaxRunningRequests = 32

	// defaultMaxWaitRequests is an extra guard: pods with more waiting than this
	// lose the full bonus even if they're under the running cap.
	defaultMaxWaitRequests = 64

	// defaultProgramKeyTTL is the Redis HASH key TTL in seconds. The plugin
	// refreshes the TTL on every successful affinity score so the mapping is
	// sliding rather than wall-clock.
	defaultProgramKeyTTL = 3600 // 1 hour

	// agentAffinityQueryTimeout bounds each Redis query.
	agentAffinityQueryTimeout = 3 * time.Second
)

// AgentAffinityArgs carries the YAML-decoded plugin arguments read from
// scheduler config under `scorePlugins[agent-affinity].args`.
type AgentAffinityArgs struct {
	// AffinityScore is the base score for pods known to serve the same program.
	// Default 80. Clamped to [0, 100].
	AffinityScore int `yaml:"affinityScore,omitempty"`

	// LoadBonus added when the pod has headroom. Default 15.
	LoadBonus int `yaml:"loadBonus,omitempty"`

	// MaxRunningRequests above which a known-affine pod is downgraded to 0.
	// Default 32. Set to 0 to disable (affinity wins regardless of load).
	MaxRunningRequests float64 `yaml:"maxRunningRequests,omitempty"`

	// MaxWaitRequests — if the pod has more waiting requests than this, the
	// load bonus is suppressed even if running is below MaxRunningRequests.
	// Default 64. Set to 0 to disable.
	MaxWaitRequests float64 `yaml:"maxWaitRequests,omitempty"`

	// ProgramKeyTTLSeconds is the Redis HASH key TTL. Default 3600 (1 hour).
	ProgramKeyTTLSeconds int `yaml:"programKeyTTLSeconds,omitempty"`

	// EnableGC toggles the background goroutine that trims stale Redis fields.
	// Default true. Useful to disable in tight unit tests.
	EnableGC *bool `yaml:"enableGC,omitempty"`
}

// normalize validates defaults for missing / out-of-range fields.
func (a *AgentAffinityArgs) normalize() {
	if a.AffinityScore <= 0 {
		a.AffinityScore = defaultAffinityScore
	}
	if a.AffinityScore > 100 {
		a.AffinityScore = 100
	}
	if a.LoadBonus <= 0 {
		a.LoadBonus = defaultLoadBonus
	}
	if a.AffinityScore+a.LoadBonus > 100 {
		a.LoadBonus = 100 - a.AffinityScore
	}
	if a.MaxRunningRequests == 0 {
		a.MaxRunningRequests = defaultMaxRunningRequests
	}
	if a.MaxWaitRequests == 0 {
		a.MaxWaitRequests = defaultMaxWaitRequests
	}
	if a.ProgramKeyTTLSeconds <= 0 {
		a.ProgramKeyTTLSeconds = defaultProgramKeyTTL
	}
	if a.EnableGC == nil {
		b := true
		a.EnableGC = &b
	}
}

// AgentAffinity implements framework.ScorePlugin and writes program→pod mappings
// to Redis so cross-router deployments converge on the same affinity table.
type AgentAffinity struct {
	name  string
	args  AgentAffinityArgs
	redis *redis.Client
}

var _ framework.ScorePlugin = &AgentAffinity{}

// NewAgentAffinity constructs the plugin. When Redis is unavailable (nil client)
// the plugin is effectively a no-op and returns nil for every Score call.
func NewAgentAffinity(_ datastore.Store, rawArgs runtime.RawExtension) *AgentAffinity {
	var args AgentAffinityArgs
	if len(rawArgs.Raw) > 0 {
		if err := yaml.Unmarshal(rawArgs.Raw, &args); err != nil {
			klog.Warningf("AgentAffinity: failed to parse args %q (%v), using defaults",
				string(rawArgs.Raw), err)
		}
	}
	args.normalize()

	plugin := &AgentAffinity{
		name: AgentAffinityPluginName,
		args: args,
	}
	plugin.redis = utils.TryGetRedisClient()
	if plugin.redis == nil {
		klog.Warning("AgentAffinity: Redis client is nil — program-affinity scoring will return no scores. " +
			"Router configs that rely on this plugin should ensure Redis is reachable.")
	}

	klog.Infof("AgentAffinity: enabled, affinityScore=%d, loadBonus=%d, maxRunning=%.0f, maxWait=%.0f, ttl=%ds, redis=%v",
		args.AffinityScore, args.LoadBonus, args.MaxRunningRequests, args.MaxWaitRequests,
		args.ProgramKeyTTLSeconds, plugin.redis != nil)
	return plugin
}

// Name implements framework.ScorePlugin.
func (p *AgentAffinity) Name() string { return p.name }

// Score implements framework.ScorePlugin.
//
// Returns nil when:
//   - ctx is nil or has no AgentHints
//   - AgentHints.ProgramID is empty
//   - Redis is not initialised (cross-router state is unavailable)
//
// Otherwise, pods currently known to serve the program_id receive
// [AffinityScore, AffinityScore+LoadBonus], clamped to [0, 100].
// Unknown pods receive 0 so the scheduler picks up fallback plugins.
func (p *AgentAffinity) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	if ctx == nil || ctx.AgentHints == nil || !ctx.AgentHints.HasProgramAffinity() {
		klog.V(4).Infof("AgentAffinity.Score: no program_id — skipping")
		return nil
	}
	if len(pods) == 0 {
		return nil
	}
	if p.redis == nil {
		return nil
	}

	programID := ctx.AgentHints.ProgramID
	key := agentAffinityKeyPrefix + programID

	ctxRedis, cancel := context.WithTimeout(context.Background(), agentAffinityQueryTimeout)
	defer cancel()

	owners, err := p.redis.HGetAll(ctxRedis, key).Result()
	if err != nil {
		klog.V(4).Infof("AgentAffinity.Score: HGetAll %s failed: %v", key, err)
		return nil
	}

	klog.V(4).Infof("AgentAffinity.Score: program=%q, known_owners=%d, candidates=%d",
		programID, len(owners), len(pods))

	scores := make(map[*datastore.PodInfo]int, len(pods))
	now := time.Now().Unix()
	activeOwners := make(map[string]struct{}, len(owners))
	ownerSetUpdated := false

	for _, pod := range pods {
		owner := podCacheOwnerIdentifier(pod)
		registeredAt, known := owners[owner]

		// A pod that doesn't carry a timestamp can't be treated as "known".
		var registeredUnix int64
		if known {
			if n, err := strconv.ParseInt(registeredAt, 10, 64); err == nil {
				registeredUnix = n
			} else {
				known = false
			}
		}

		score := 0
		if known {
			score = p.args.AffinityScore

			if p.hasLoadHeadroom(pod) {
				score += p.args.LoadBonus
			}

			// Hard cap — if the pod is beyond the running-request ceiling it is
			// too busy to give real affinity benefit; downgrade to 0.
			if pod.RequestRunningNum > p.args.MaxRunningRequests {
				klog.V(4).Infof("AgentAffinity.Score: pod %s running=%.0f > max=%.0f, downgrading",
					owner, pod.RequestRunningNum, p.args.MaxRunningRequests)
				score = 0
			}

			if score > 100 {
				score = 100
			}

			// Refresh the registration timestamp if the mapping is older than a
			// second. This is a cheap, single-field HSET.
			if now-registeredUnix > 1 {
				activeOwners[owner] = struct{}{}
				ownerSetUpdated = true
			} else {
				activeOwners[owner] = struct{}{}
			}
		}

		scores[pod] = score
	}

	if ownerSetUpdated {
		p.refreshOwners(key, activeOwners, now)
	}

	return scores
}

// hasLoadHeadroom reports whether the pod is below both the running and waiting
// request caps — qualifies for the LoadBonus.
func (p *AgentAffinity) hasLoadHeadroom(pod *datastore.PodInfo) bool {
	if p.args.MaxRunningRequests > 0 && pod.RequestRunningNum >= p.args.MaxRunningRequests {
		return false
	}
	if p.args.MaxWaitRequests > 0 && pod.RequestWaitingNum >= p.args.MaxWaitRequests {
		return false
	}
	return true
}

// refreshOwners writes the current owners back to Redis and slides the key TTL.
func (p *AgentAffinity) refreshOwners(key string, owners map[string]struct{}, now int64) {
	ctxRedis, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := p.redis.Pipeline()
	for owner := range owners {
		pipe.HSet(ctxRedis, key, owner, strconv.FormatInt(now, 10))
	}
	pipe.Expire(ctxRedis, key, time.Duration(p.args.ProgramKeyTTLSeconds)*time.Second)
	if _, err := pipe.Exec(ctxRedis); err != nil {
		klog.V(4).Infof("AgentAffinity.refreshOwners: %v", err)
	}
}

// --- Optional helpers -------------------------------------------------------

// RecordProgramOwnership is a best-effort post-schedule hook that lets callers
// (typically the Router) register a program→pod mapping after the routing
// decision is made, even if Score() chose a non-affine pod because the target
// was overloaded. This seeds future affinity decisions.
func (p *AgentAffinity) RecordProgramOwnership(programID, podOwner string) {
	if p == nil || p.redis == nil || programID == "" || podOwner == "" {
		return
	}
	key := agentAffinityKeyPrefix + programID
	now := time.Now().Unix()

	// Ensure TTL is positive even if the plugin was constructed by hand (tests,
	// in-process router). An explicit 0 TTL would make Redis EXPIRE immediately
	// delete the key we just wrote.
	ttlSeconds := p.args.ProgramKeyTTLSeconds
	if ttlSeconds <= 0 {
		ttlSeconds = defaultProgramKeyTTL
	}

	ctxRedis, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := p.redis.Pipeline()
	pipe.HSet(ctxRedis, key, podOwner, strconv.FormatInt(now, 10))
	pipe.Expire(ctxRedis, key, time.Duration(ttlSeconds)*time.Second)
	if _, err := pipe.Exec(ctxRedis); err != nil {
		klog.V(4).Infof("AgentAffinity.RecordProgramOwnership: %v", err)
	}
}

// ProgramOwnersSnapshot is returned by SnapshotProgramOwners for observability / tests.
type ProgramOwnersSnapshot struct {
	ProgramID string
	Owners    []string // pod cache owner identifiers
}

// SnapshotProgramOwners is a debugging helper that dumps the current program→pod
// mapping from Redis. Returns nil if Redis is unavailable.
func (p *AgentAffinity) SnapshotProgramOwners(ctx context.Context, programIDs ...string) []ProgramOwnersSnapshot {
	if p == nil || p.redis == nil {
		return nil
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
	}

	out := make([]ProgramOwnersSnapshot, 0, len(programIDs))
	for _, pid := range programIDs {
		key := agentAffinityKeyPrefix + pid
		fields, err := p.redis.HGetAll(ctx, key).Result()
		if err != nil {
			continue
		}
		snap := ProgramOwnersSnapshot{ProgramID: pid, Owners: make([]string, 0, len(fields))}
		for owner := range fields {
			snap.Owners = append(snap.Owners, owner)
		}
		out = append(out, snap)
	}
	return out
}

// --- Helpers for other plugins ----------------------------------------------

// BuildAgentAffinityKey is exposed so other components (sidecar, tests) can
// compute the same Redis key without hard-coding the prefix.
func BuildAgentAffinityKey(programID string) string {
	return agentAffinityKeyPrefix + programID
}

// AgentPriorityScore normalizes AgentHints.TaskPriority into a score multiplier
// in [0.5, 2.0] that plugins can fold into their own scoring.
//
//	priority <= 1 → 1.0 (baseline)
//	priority 10   → 2.0 (max)
//	priority 11+  → 2.0 (clamped)
//
// Negative priorities are also normalized to 1.0 — they are treated as "no hint".
func AgentPriorityScore(h *common.AgentHints) float64 {
	if h == nil {
		return 1.0
	}
	p := h.TaskPriority
	if p <= 1 {
		return 1.0
	}
	if p >= 10 {
		return 2.0
	}
	// Linear interpolation: p=1 → 1.0, p=10 → 2.0
	return 1.0 + float64(p-1)*(1.0/9.0)
}

// ContextWeight returns the context-weight multiplier used by weighted eviction.
// See common.ContextType.DefaultContextWeight for the derivation.
func ContextWeight(h *common.AgentHints) float64 {
	if h == nil {
		return 1.0
	}
	return h.ContextType.DefaultContextWeight()
}

// Ensure the plugin is an exact framework.ScorePlugin.
var _ framework.ScorePlugin = (*AgentAffinity)(nil)

// ensure fmt import is used (we use it in error-formatting but not elsewhere directly)
var _ = fmt.Sprintf
