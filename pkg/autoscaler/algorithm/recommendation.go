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

package algorithm

import (
	"math"

	"k8s.io/klog/v2"
)

type Metrics = map[string]float64

type RecommendedInstancesAlgorithm struct {
	MinInstances          int32
	MaxInstances          int32
	CurrentInstancesCount int32
	Tolerance             float64
	MetricTargets         Metrics
	// UnreadyInstancesCountByMetric and ReadyInstancesMetrics are keyed by metric
	// name and cover only the pods selected for that metric.
	UnreadyInstancesCountByMetric map[string]int32
	ReadyInstancesMetrics         map[string][]Metrics
	ExternalMetrics               Metrics
	// ReplicasByMetric is keyed by metric name and holds how many replicas the pods
	// selected for that metric belong to. A metric with no entry counts each pod as one
	// replica. External metrics don't use it.
	ReplicasByMetric map[string]ReplicaCount
}

// ReplicaCount holds how many replicas the pods selected for one metric belong to. The
// load ratio is measured per pod, and multiplying it by this count gives the desired
// replicas. This is what keeps the result in replicas when a replica owns several pods.
type ReplicaCount struct {
	// Ready counts the replicas behind the ready pods.
	Ready float64
	// All counts the replicas behind the ready and unready pods.
	All float64
}

func (alg *RecommendedInstancesAlgorithm) GetRecommendedInstances() (recommendedInstances int32, skip bool) {
	klog.V(4).InfoS("start to getRecommendedInstances", "args", alg)
	if alg.CurrentInstancesCount < alg.MinInstances {
		return alg.MinInstances, false
	}
	if alg.CurrentInstancesCount > alg.MaxInstances {
		return alg.MaxInstances, false
	}
	recommendedInstances = 0
	skip = true
	for name, target := range alg.MetricTargets {
		externalMetric, ok := alg.ExternalMetrics[name]
		if ok {
			updateRecommendation(&recommendedInstances, &skip,
				getDesiredInstancesForSingleExternalMetric(
					alg.CurrentInstancesCount,
					alg.Tolerance,
					target,
					externalMetric,
				))
		} else {
			if desired, ok := getDesiredInstancesForSingleInstanceMetric(
				alg.CurrentInstancesCount,
				alg.Tolerance,
				name,
				target,
				alg.UnreadyInstancesCountByMetric[name],
				alg.ReadyInstancesMetrics[name],
				alg.ReplicasByMetric[name],
			); ok {
				updateRecommendation(&recommendedInstances, &skip, desired)
			}
		}
	}
	if !skip {
		recommendedInstances = min(max(recommendedInstances, alg.MinInstances), alg.MaxInstances)
	}
	return recommendedInstances, skip
}

func updateRecommendation(recommendedInstances *int32, skip *bool, desired int32) {
	if *skip {
		*recommendedInstances = desired
		*skip = false
	} else {
		*recommendedInstances = max(*recommendedInstances, desired)
	}
}

func getDesiredInstancesForSingleExternalMetric(
	currentCount int32,
	tolerance float64,
	target float64,
	metric float64,
) int32 {
	desired := metric / target
	ratio := desired / float64(currentCount)
	if math.Abs(ratio-1.0) <= tolerance {
		return currentCount
	}
	return getCeilDesiredInstances(desired)
}

func getDesiredInstancesForSingleInstanceMetric(
	currentCount int32,
	tolerance float64,
	name string,
	target float64,
	unreadyCount int32,
	readyMetrics []Metrics,
	replicas ReplicaCount,
) (desired int32, ok bool) {
	currentMetricSum := 0.0
	missingCount := int32(0)
	metricsCount := int32(0)
	for _, readyInstance := range readyMetrics {
		metric, ok := readyInstance[name]
		if ok {
			metricsCount++
			currentMetricSum += metric
		} else {
			missingCount++
		}
	}
	if metricsCount == 0 {
		return 0, false
	}
	ratio := currentMetricSum / float64(metricsCount) / target
	shouldAddUnready := unreadyCount > 0 && getDirection(ratio) > 0
	klog.InfoS("recommendation", "metricsCount", metricsCount, "currentMetricSum", currentMetricSum, "ratio", ratio,
		"unreadyCount", unreadyCount, "shouldAddUnready", shouldAddUnready, "missingCount", missingCount, "tolerance", tolerance,
		"readyReplicas", replicas.Ready, "allReplicas", replicas.All)
	if !shouldAddUnready && missingCount == 0 {
		if math.Abs(ratio-1.0) <= tolerance {
			return currentCount, true
		}
		return getCeilDesiredInstances(ratio * replicasOrPods(replicas.Ready, metricsCount)), true
	}
	metricsCount += missingCount
	if getDirection(ratio) < 0 {
		currentMetricSum += float64(missingCount) * target
	}
	if shouldAddUnready {
		metricsCount += unreadyCount
	}
	newRatio := currentMetricSum / float64(metricsCount) / target
	if math.Abs(newRatio-1.0) <= tolerance || getDirection(ratio) != getDirection(newRatio) {
		return currentCount, true
	}
	replicaCount := replicas.Ready
	if shouldAddUnready {
		replicaCount = replicas.All
	}
	desired = getCeilDesiredInstances(newRatio * replicasOrPods(replicaCount, metricsCount))
	if (getDirection(newRatio) < 0 && desired > currentCount) ||
		(getDirection(newRatio) > 0 && desired < currentCount) {
		return currentCount, true
	}
	return desired, true
}

// replicasOrPods returns the count to multiply the load ratio by. When the replica count
// is unknown, each pod counts as one replica.
func replicasOrPods(replicas float64, podCount int32) float64 {
	if replicas > 0 {
		return replicas
	}
	return float64(podCount)
}

func getDirection(ratio float64) int32 {
	if ratio >= 1.0 {
		return 1
	} else {
		return -1
	}
}

func getCeilDesiredInstances(value float64) int32 {
	if math.IsNaN(value) {
		return 0
	}
	value = math.Ceil(value)
	const bound = int32(1000000000)
	if value < float64(bound) {
		return max(0, int32(value))
	}
	return bound
}
