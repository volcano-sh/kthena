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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/metrics"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

const (
	// Get the top five scoring podinfo
	topN = 5
)

type SchedulerImpl struct {
	store datastore.Store

	filterPlugins []framework.FilterPlugin
	scorePlugins  []*scorePlugin

	postScheduleHooks []framework.PostScheduleHook
}

type scorePlugin struct {
	plugin framework.ScorePlugin
	weight int
}

type podInfoWithValue struct {
	pod   *datastore.PodInfo
	score int
}

func NewScheduler(store datastore.Store, routerConfig *conf.RouterConfiguration) Scheduler {
	// For backward compatibility, use the default registry and ensure plugins are registered
	registry := NewPluginRegistry()
	registerDefaultPlugins(registry)

	// Default plugin configuration.
	scorePluginMap := map[string]int{
		"least-request": 1,
		"least-latency": 1,
		"prefix-cache":  1,
	}
	filterPluginMap := []string{
		"least-request",
	}
	pluginsArgMap := map[string]runtime.RawExtension{
		"least-request": {Raw: []byte(`{"maxWaitingRequests": 10}`)},
		"least-latency": {Raw: []byte(`{"TTFTTPOTWeightFactor": 0.5}`)},
		"prefix-cache":  {Raw: []byte(`{"blockSizeToHash": 64, "maxBlocksToMatch": 128, "maxHashCacheSize": 50000}`)},
	}

	var err error
	if routerConfig == nil {
		// If no scheduler configuration is provided, use the default configuration
		klog.Warning("No scheduler configuration found, using default configuration")
	} else {
		scorePluginMap, filterPluginMap, pluginsArgMap, err = conf.LoadSchedulerConfig(&routerConfig.Scheduler)
		if err != nil {
			klog.Fatalf("failed to Load Scheduler: %v", err)
		}
	}

	prefixCache := plugins.NewPrefixCache(store, pluginsArgMap[plugins.PrefixCachePluginName])
	return &SchedulerImpl{
		store:         store,
		filterPlugins: getFilterPlugins(registry, filterPluginMap, pluginsArgMap),
		scorePlugins:  getScorePlugins(registry, prefixCache, scorePluginMap, pluginsArgMap),
		postScheduleHooks: []framework.PostScheduleHook{
			prefixCache,
		},
	}
}

func (s *SchedulerImpl) Schedule(ctx *framework.Context, pods []*datastore.PodInfo) error {
	// first filter out invalid pods that wonot be selected to loadbalance to.
	pods, err := s.RunFilterPlugins(pods, ctx)
	if err != nil {
		return err
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

		klog.V(4).Info("Running score plugins for decode pod")
		scores := s.RunScorePlugins(decodePods, ctx)

		topNDecodePods := TopNPodInfos(scores, topN)
		ctx.DecodePods = topNDecodePods
		prefillPods := make([]*datastore.PodInfo, len(topNDecodePods))
		validPairs := 0

		for i, decodePod := range ctx.DecodePods {
			// Get prefill pods for the same PD group as the decode pod (O(1) lookup)
			selectedPods, err := s.store.GetPrefillPodsForDecodeGroup(ctx.ModelServerName,
				types.NamespacedName{
					Namespace: decodePod.Pod.Namespace,
					Name:      decodePod.Pod.Name,
				})
			if err != nil || len(selectedPods) == 0 {
				klog.V(4).InfoS("prefill pods for decode group not found", "decode instance", klog.KObj(decodePod.Pod), "error", err)
				continue
			}

			klog.V(4).Info("Running score plugins for prefill pod")
			scores = s.RunScorePlugins(selectedPods, ctx)
			bestPrefillPod := TopNPodInfos(scores, 1)
			if len(bestPrefillPod) == 0 {
				klog.V(4).InfoS("no valid prefill pods after scoring, skipping",
					"decode instance", klog.KObj(decodePod.Pod))
				continue
			}
			prefillPods[i] = bestPrefillPod[0]
			validPairs++
		}
		ctx.PrefillPods = prefillPods
		if validPairs == 0 {
			return fmt.Errorf("no valid prefill-decode pod pairs found")
		}
		return nil
	}

	klog.V(4).Info("Running score plugins for PD aggregated pod")
	scores := s.RunScorePlugins(pods, ctx)
	ctx.BestPods = TopNPodInfos(scores, topN)

	return nil
}

func (s *SchedulerImpl) RunFilterPlugins(pods []*datastore.PodInfo, ctx *framework.Context) ([]*datastore.PodInfo, error) {
	for _, filterPlugin := range s.filterPlugins {
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

func (s *SchedulerImpl) RunScorePlugins(pods []*datastore.PodInfo, ctx *framework.Context) map[*datastore.PodInfo]int {
	res := make(map[*datastore.PodInfo]int)
	for _, scorePlugin := range s.scorePlugins {
		// Record score plugin execution time
		startTime := time.Now()
		scores := scorePlugin.plugin.Score(ctx, pods)
		duration := time.Since(startTime)

		// Use the MetricsRecorder from context to record plugin duration
		if ctx.MetricsRecorder != nil {
			ctx.MetricsRecorder.RecordSchedulerPluginDuration(scorePlugin.plugin.Name(), metrics.PluginTypeScore, duration)
		}

		klog.V(4).Infof("ScorePlugin: %s", scorePlugin.plugin.Name())
		for k, v := range scores {
			if k.Pod != nil {
				klog.V(4).Infof("Pod: %s/%s, Score: %d", k.Pod.Namespace, k.Pod.Name, v)
			}
			if _, ok := res[k]; !ok {
				res[k] = v * scorePlugin.weight
			} else {
				res[k] += v * scorePlugin.weight
			}
		}
	}

	if klog.V(4).Enabled() {
		klog.Info("Final Pod Scores:")
		for k, v := range res {
			if k.Pod != nil {
				klog.Infof("  Pod: %s/%s, Final Score: %d", k.Pod.Namespace, k.Pod.Name, v)
			}
		}
	}

	return res
}

func (s *SchedulerImpl) RunPostHooks(ctx *framework.Context, index int) {
	for _, hook := range s.postScheduleHooks {
		hook.PostSchedule(ctx, index)
	}
}

func TopNPodInfos(m map[*datastore.PodInfo]int, n int) []*datastore.PodInfo {
	var list []podInfoWithValue
	for k, v := range m {
		list = append(list, podInfoWithValue{pod: k, score: v})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].score > list[j].score
	})

	res := []*datastore.PodInfo{}
	for i := range list {
		if i >= n {
			break
		}
		res = append(res, list[i].pod)
	}

	return res
}
