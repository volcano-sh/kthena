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
	"math"

	"istio.io/istio/pkg/slices"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const LeastRequestPluginName = "least-request"

var _ framework.ScorePlugin = &LeastRequest{}
var _ framework.FilterPlugin = &LeastRequest{}

type LeastRequest struct {
	name               string
	maxWaitingRequests int
}

type LeastRequestArgs struct {
	// MaxWaitingRequests filters out pods whose engine-reported waiting-queue
	// depth exceeds this value. It captures backpressure that the router cannot
	// observe directly (e.g. requests queued inside the engine before execution).
	MaxWaitingRequests int `yaml:"maxWaitingRequests,omitempty"`
}

func NewLeastRequest(pluginArg runtime.RawExtension) *LeastRequest {
	var leastRequestArgs LeastRequestArgs
	if pluginArg.Raw == nil || yaml.Unmarshal(pluginArg.Raw, &leastRequestArgs) != nil {
		klog.Errorf("Unmarshal LeastRequestArgs error, setting default value")
		leastRequestArgs = LeastRequestArgs{
			MaxWaitingRequests: 10,
		}
	}
	if leastRequestArgs.MaxWaitingRequests == 0 {
		leastRequestArgs.MaxWaitingRequests = 10
	}

	return &LeastRequest{
		name:               LeastRequestPluginName,
		maxWaitingRequests: leastRequestArgs.MaxWaitingRequests,
	}
}

func (l *LeastRequest) Name() string {
	return l.name
}

func (l *LeastRequest) Filter(ctx *framework.Context, pods []*datastore.PodInfo) []*datastore.PodInfo {
	return slices.FilterInPlace(pods, func(info *datastore.PodInfo) bool {
		// Filter on engine-reported waiting queue: catches backlog the router
		// cannot observe (requests already inside the engine but not yet running).
		return info.GetRequestWaitingNum() < float64(l.maxWaitingRequests)
	})
}

func (l *LeastRequest) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)
	if len(pods) == 0 {
		return scoreResults
	}

	// Score formula: base = onFlight + 100 * max(onFlight - running, 0)
	//
	//   - onFlight: router-tracked in-flight count, updated with zero delay.
	//     Acts as a proxy for current pod load and avoids the ~1 s engine-metrics
	//     poll lag.
	//   - max(onFlight - running, 0): estimates the number of requests that the
	//     router has dispatched but the engine has not yet started executing
	//     (i.e. likely queued inside the engine). Weighted ×100 to strongly
	//     penalise pods whose queue is building up.
	baseScores := make(map[*datastore.PodInfo]float64)
	maxScore := 0.0
	for _, info := range pods {
		// Estimate queued requests as max(onFlight - running, 0). The engine-reported
		// running count has a poll lag (~1 s), so this may briefly over-count, but
		// it provides a leading indicator of queue build-up at the pod.
		base := float64(info.GetOnFlightRequestNum()) + 100*math.Max(float64(info.GetOnFlightRequestNum())-float64(info.GetRequestRunningNum()), 0)
		baseScores[info] = base
		if base > maxScore {
			maxScore = base
		}
	}

	// Normalise to [0, 100]: the least-loaded pod gets 100.
	for _, info := range pods {
		score := 100.0
		if maxScore > 0 {
			score = ((maxScore - baseScores[info]) / maxScore) * 100
		}
		scoreResults[info] = int(score)
	}

	return scoreResults
}
