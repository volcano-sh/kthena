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
	"slices"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

const (
	// Get the top five scoring podinfo
	topN = 5
)

type SchedulerImpl struct {
	store datastore.Store

	// globalChain is the plugin chain built from the router ConfigMap at
	// startup, used when the request context carries no profile chain.
	globalChain *PluginChain
}

type scorePlugin struct {
	plugin framework.ScorePlugin
	weight int
}

type podInfoWithValue struct {
	pod   *datastore.PodInfo
	score int
}

func NewScheduler(store datastore.Store, routerConfig *conf.RouterConfiguration) (Scheduler, error) {
	// For backward compatibility, use the default registry and ensure plugins are registered
	registry := NewPluginRegistry()
	registerDefaultPlugins(registry)

	// Default plugin configuration.
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
			return nil, fmt.Errorf("failed to load scheduler config: %w", err)
		}
	}

	scorePlugins, err := getScorePlugins(registry, store, scorePluginMap, pluginsArgMap)
	if err != nil {
		return nil, err
	}
	filters, err := getFilterPlugins(registry, filterPlugins, pluginsArgMap)
	if err != nil {
		return nil, err
	}
	globalChain := &PluginChain{
		filters:           filters,
		scores:            scorePlugins,
		postScheduleHooks: getPostScheduleHooks(scorePlugins),
		syncOnFlight:      chainNeedsOnFlightSync(filters, scorePlugins),
	}
	s := &SchedulerImpl{
		store:       store,
		globalChain: globalChain,
	}
	return s, nil
}

func (s *SchedulerImpl) Schedule(ctx *framework.Context, pods []*datastore.PodInfo) error {
	// The router (doLoadbalance) resolves the chain for this round and carries
	// it on the context. Fall back to resolving here for callers that skip
	// doLoadbalance (e.g. tests).
	chain, ok := ctx.ScheduleChain.(*PluginChain)
	if !ok || chain == nil {
		chain = s.globalChain
		ctx.ScheduleChain = chain
		klog.V(4).Infof("ModelServer %s: scheduling model %s on the global chain", ctx.ModelServerName, ctx.Model)
	}

	// Sync on-flight counts from Redis before scoring when THIS chain's plugin
	// set includes least-request (per-chain derived state), so cross-router
	// traffic is reflected in pod scores. Rate-limited internally; no-op when
	// Redis is not enabled.
	if chain.syncOnFlight {
		s.store.SyncOnFlightCounts()
	}

	if ctx.PDGroup != nil {
		// Use optimized PDGroup scheduling with pre-categorized pods from store
		klog.V(4).Info("Using optimized PD disaggregated scheduling")

		// Get decode pods directly from store (O(1) lookup)
		decodePods, err := s.store.GetDecodePods(ctx.ModelServerName)
		if err != nil {
			return fmt.Errorf("failed to get decode pods: %v", err)
		}

		if len(decodePods) == 0 {
			return fmt.Errorf("no decode pod found")
		}

		// The initial pod list contains both prefill and decode roles. Filter the
		// role-specific list after retrieving it from the store so overloaded
		// decode pods cannot bypass filters in the optimized PD path.
		decodePods, err = s.RunFilterPlugins(chain.filters, decodePods, ctx)
		if err != nil {
			return err
		}

		var decodeCandidates []*datastore.PodInfo
		var prefillCandidates []*datastore.PodInfo
		// PD session sticky: pin a complete Prefill/Decode pair, then skip Score
		// (same as aggregated sticky). Half a pair is never preferred.
		// 1. Require both sticky names; otherwise fall through to normal scoring.
		if ctx.StickyPodName != "" && ctx.StickyPrefillPodName != "" {
			// 2. Sticky decode must still be in the filtered decode list.
			stickyDecodeIndex := slices.IndexFunc(decodePods, func(pod *datastore.PodInfo) bool {
				return pod != nil && pod.Pod != nil && pod.Pod.Name == ctx.StickyPodName
			})
			if stickyDecodeIndex >= 0 {
				stickyDecode := decodePods[stickyDecodeIndex]
				// 3. Load prefills in the same PD group as that decode, then Filter them.
				stickyPrefillPods, stickyErr := s.store.GetPrefillPodsForDecodeGroup(
					ctx.ModelServerName, stickyDecode.GetPodNamespacedName())
				if stickyErr == nil {
					stickyPrefillPods, stickyErr = s.RunFilterPlugins(chain.filters, stickyPrefillPods, ctx)
				}
				// 4. Sticky prefill must still be selectable in that group.
				stickyPrefillIndex := slices.IndexFunc(stickyPrefillPods, func(pod *datastore.PodInfo) bool {
					return pod != nil && pod.Pod != nil && pod.Pod.Name == ctx.StickyPrefillPodName
				})
				if stickyErr == nil && stickyPrefillIndex >= 0 {
					// 5. Both sides OK: pin only this pair and return (no Score, no topN fill).
					ctx.DecodePods = []*datastore.PodInfo{stickyDecode}
					ctx.PrefillPods = []*datastore.PodInfo{stickyPrefillPods[stickyPrefillIndex]}
					return nil
				}
			}
			// 6. Sticky miss (decode gone, prefill filtered, or group mismatch):
			// clear names and score new pairs normally.
			ctx.StickyPodName = ""
			ctx.StickyPrefillPodName = ""
		}

		klog.V(4).Info("Running score plugins for decode pod")
		scores := s.RunScorePlugins(chain.scores, decodePods, ctx)
		topNDecodePods := TopNPodInfos(scores, topN)

		for _, decodePod := range topNDecodePods {
			decodePodName := decodePod.GetPodNamespacedName()
			if decodePodName.Name == "" {
				continue
			}
			// Get prefill pods for the same PD group as the decode pod (O(1) lookup)
			selectedPods, err := s.store.GetPrefillPodsForDecodeGroup(ctx.ModelServerName, decodePodName)
			if err != nil || len(selectedPods) == 0 {
				klog.V(4).InfoS("prefill pods for decode group not found", "decode instance", decodePodName, "error", err)
				continue
			}

			selectedPods, err = s.RunFilterPlugins(chain.filters, selectedPods, ctx)
			if err != nil {
				klog.V(4).InfoS("prefill pods were filtered out", "decode instance", decodePodName, "error", err)
				continue
			}

			klog.V(4).Info("Running score plugins for prefill pod")
			scores = s.RunScorePlugins(chain.scores, selectedPods, ctx)
			bestPrefillPod := TopNPodInfos(scores, 1)
			if len(bestPrefillPod) == 0 {
				klog.V(4).InfoS("no valid prefill pods after scoring, skipping",
					"decode instance", decodePodName)
				continue
			}
			decodeCandidates = append(decodeCandidates, decodePod)
			prefillCandidates = append(prefillCandidates, bestPrefillPod[0])
		}
		ctx.DecodePods = decodeCandidates
		ctx.PrefillPods = prefillCandidates
		if len(decodeCandidates) == 0 {
			return fmt.Errorf("no valid prefill-decode pod pairs found")
		}

		return nil
	}

	pods, err := s.RunFilterPlugins(chain.filters, pods, ctx)
	if err != nil {
		return err
	}

	if ctx.StickyPodName != "" {
		for _, p := range pods {
			if p.Pod != nil && p.Pod.Name == ctx.StickyPodName {
				// Sticky pod already passed filters; skip score plugins.
				ctx.BestPods = []*datastore.PodInfo{p}
				return nil
			}
		}
		ctx.StickyPodName = ""
	}

	klog.V(4).Info("Running score plugins for PD aggregated pod")
	scores := s.RunScorePlugins(chain.scores, pods, ctx)
	ctx.BestPods = TopNPodInfos(scores, topN)

	return nil
}

func (s *SchedulerImpl) RunFilterPlugins(filters []framework.FilterPlugin, pods []*datastore.PodInfo, ctx *framework.Context) ([]*datastore.PodInfo, error) {
	for _, filterPlugin := range filters {
		// Record filter plugin execution time
		startTime := time.Now()
		pods = filterPlugin.Filter(ctx, pods)
		duration := time.Since(startTime)

		// Use the MetricsRecorder from context to record plugin duration
		if ctx.MetricsRecorder != nil {
			ctx.MetricsRecorder.RecordSchedulerPluginDuration(filterPlugin.Name(), metrics.PluginTypeFilter, duration)
		}

		if len(pods) == 0 {
			return nil, fmt.Errorf("pods have all been filtered out by %q", filterPlugin.Name())
		}
	}

	return pods, nil
}

func (s *SchedulerImpl) RunScorePlugins(scores []*scorePlugin, pods []*datastore.PodInfo, ctx *framework.Context) map[*datastore.PodInfo]int {
	res := make(map[*datastore.PodInfo]int, len(pods))
	for _, pod := range pods {
		res[pod] = 0
	}
	// Checked once: V(4) arguments are boxed before klog can discard them
	logScores := klog.V(4).Enabled()
	for _, scorePlugin := range scores {
		// Record score plugin execution time
		startTime := time.Now()
		scores := scorePlugin.plugin.Score(ctx, pods)
		duration := time.Since(startTime)

		// Use the MetricsRecorder from context to record plugin duration
		if ctx.MetricsRecorder != nil {
			ctx.MetricsRecorder.RecordSchedulerPluginDuration(scorePlugin.plugin.Name(), metrics.PluginTypeScore, duration)
		}

		if logScores {
			klog.Infof("ScorePlugin: %s", scorePlugin.plugin.Name())
		}
		for k, v := range scores {
			if logScores {
				if podName := k.GetPodNamespacedName(); podName.Name != "" {
					klog.Infof("Pod: %s/%s, Score: %d", podName.Namespace, podName.Name, v)
				}
			}
			if _, ok := res[k]; !ok {
				continue
			}
			res[k] += v * scorePlugin.weight
		}
	}

	if logScores {
		klog.Info("Final Pod Scores:")
		for k, v := range res {
			if podName := k.GetPodNamespacedName(); podName.Name != "" {
				klog.Infof("  Pod: %s/%s, Final Score: %d", podName.Namespace, podName.Name, v)
			}
		}
	}

	return res
}

func (s *SchedulerImpl) RunPostHooks(ctx *framework.Context, index int) {
	// Reuse the chain resolved in Schedule (carried on the context); fall
	// back to the global chain when absent (e.g. a caller skipped Schedule).
	// Log at V(2): a profile-chain request landing on the global chain here
	// is a semantic deviation that must be visible in the logs.
	chain, ok := ctx.ScheduleChain.(*PluginChain)
	if !ok || chain == nil {
		chain = s.globalChain
		klog.V(2).Infof("ModelServer %s: no schedule chain on context, running post hooks on the global chain", ctx.ModelServerName)
	}
	for _, hook := range chain.postScheduleHooks {
		hook.PostSchedule(ctx, index)
	}
}

func TopNPodInfos(m map[*datastore.PodInfo]int, n int) []*datastore.PodInfo {
	// One entry per scored pod, so the length is known before the loop
	list := make([]podInfoWithValue, 0, len(m))
	for k, v := range m {
		list = append(list, podInfoWithValue{pod: k, score: v})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})

	// Clamp first, make() panics on a negative capacity
	if n > len(list) {
		n = len(list)
	}
	if n < 0 {
		n = 0
	}
	res := make([]*datastore.PodInfo, 0, n)
	for i := 0; i < n; i++ {
		res = append(res, list[i].pod)
	}

	return res
}
