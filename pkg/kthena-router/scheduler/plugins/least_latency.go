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

	"github.com/stretchr/testify/assert/yaml"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

var _ framework.ScorePlugin = &LeastLatency{}

const LeastLatencyPluginName = "least-latency"

// MaxScore is the highest possible score a pod can receive
const MaxScore = 100.0

type LeastLatency struct {
	name                 string
	TTFTTPOTWeightFactor float64
}

type LeastLatencyArgs struct {
	TTFTTPOTWeightFactor float64 `yaml:"TTFTTPOTWeightFactor,omitempty"`
}

func NewLeastLatency(pluginArg runtime.RawExtension) *LeastLatency {
	var leastLatencyArgs LeastLatencyArgs
	if yaml.Unmarshal(pluginArg.Raw, &leastLatencyArgs) != nil {
		klog.Errorf("Unmarshal LeastLatencyArgs error, setting default value")
		leastLatencyArgs = LeastLatencyArgs{
			0.5,
		}
	}

	return &LeastLatency{
		name:                 LeastLatencyPluginName,
		TTFTTPOTWeightFactor: leastLatencyArgs.TTFTTPOTWeightFactor,
	}
}

func (l *LeastLatency) Name() string {
	return l.name
}

// Score calculates a score for each pod based on their inference latency:
func (l *LeastLatency) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	// Stores the computed score for each pod
	scoreResults := make(map[*datastore.PodInfo]int)
	// Handle edge case: empty pod list
	if len(pods) == 0 {
		return scoreResults
	}
	// 1. First pass: Determine the minimum and maximum latency values
	// Initialize with extreme values to ensure any valid latency updates them
	// ctx.MaxToken is the max token that the model is allowed to generate in its response.
	// Calculate min/max values for TTFT and TPOT in calculateMinMaxMetrics
	minTTFT, maxTTFT, minTPOT, maxTPOT := calculateMinMaxMetrics(pods)
	// 2. Second pass: Compute scores using linear normalization
	// Note: If all pods have identical latency (max == min), all pods get MaxScore
	for _, info := range pods {
		scoreTTFT := MaxScore
		scoreTPOT := MaxScore
		ttft := info.GetTTFT()
		tpot := info.GetTPOT()
		// Only compute normalized score if there's variance in latency values
		if maxTTFT > minTTFT {
			scoreTTFT = MaxScore * (maxTTFT - ttft) / (maxTTFT - minTTFT)
		}
		if maxTPOT > minTPOT {
			scoreTPOT = MaxScore * (maxTPOT - tpot) / (maxTPOT - minTPOT)
		}
		scoreResults[info] = int(scoreTTFT*l.TTFTTPOTWeightFactor + scoreTPOT*(1-l.TTFTTPOTWeightFactor))
	}

	return scoreResults
}

func calculateMinMaxMetrics(pods []*datastore.PodInfo) (minTTFT, maxTTFT, minTPOT, maxTPOT float64) {
	minTTFT = math.MaxFloat64
	maxTTFT = 0.0
	minTPOT = math.MaxFloat64
	maxTPOT = 0.0

	for _, info := range pods {
		ttft := info.GetTTFT()
		tpot := info.GetTPOT()
		// Skip pods with invalid values
		if ttft < 0 || tpot < 0 {
			continue
		}

		// Update TTFT min/max
		if ttft < minTTFT {
			minTTFT = ttft
		}
		if ttft > maxTTFT {
			maxTTFT = ttft
		}

		// Update TPOT min/max
		if tpot < minTPOT {
			minTPOT = tpot
		}
		if tpot > maxTPOT {
			maxTPOT = tpot
		}
	}

	return minTTFT, maxTTFT, minTPOT, maxTPOT
}
