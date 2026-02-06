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
	"github.com/stretchr/testify/assert/yaml"
	"istio.io/istio/pkg/slices"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const LeastRequestPluginName = "least-request"

var _ framework.ScorePlugin = &LeastRequest{}
var _ framework.FilterPlugin = &LeastRequest{}

type LeastRequest struct {
	name              string
	maxWaitingRequest int
}

type LeastRequestArgs struct {
	MaxWaitingRequests int `yaml:"maxWaitingRequests,omitempty"`
}

func NewLeastRequest(pluginArg runtime.RawExtension) *LeastRequest {
	var leastRequestArgs LeastRequestArgs
	if yaml.Unmarshal(pluginArg.Raw, &leastRequestArgs) != nil {
		klog.Errorf("Unmarshal LeastRequestArgs error, setting default value")
		leastRequestArgs = LeastRequestArgs{
			10,
		}
	}

	return &LeastRequest{
		name:              LeastRequestPluginName,
		maxWaitingRequest: leastRequestArgs.MaxWaitingRequests,
	}
}

func (l *LeastRequest) Name() string {
	return l.name
}

func (l *LeastRequest) Filter(ctx *framework.Context, pods []*datastore.PodInfo) []*datastore.PodInfo {
	return slices.FilterInPlace(pods, func(info *datastore.PodInfo) bool {
		return info.GetRequestWaitingNum() < float64(l.maxWaitingRequest)
	})
}

func (l *LeastRequest) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)
	if len(pods) == 0 {
		return scoreResults
	}

	// 1. Calculate the base score (running reqs + 100 * waiting reqs) for each pod
	baseScores := make(map[*datastore.PodInfo]float64)
	maxScore := 0.0
	for _, info := range pods {
		// The weight of waiting requests is 100. It's a magic number just to sinificantly lower the score of the pod when there are waiting reqs.
		base := info.GetRequestRunningNum() + 100*info.GetRequestWaitingNum()
		baseScores[info] = base
		if base > maxScore {
			maxScore = base
		}
	}

	// 2. Calculate the score for each pod as a percentage of the max base score
	for _, info := range pods {
		score := 100.0
		if maxScore > 0 {
			score = ((maxScore - baseScores[info]) / maxScore) * 100
		}
		scoreResults[info] = int(score)
	}

	return scoreResults
}
