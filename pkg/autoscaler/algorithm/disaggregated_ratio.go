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
	"fmt"
	"math"
	"strconv"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// ReplicaBounds defines the inclusive replica range for a scalable unit.
type ReplicaBounds struct {
	Min int32
	Max int32
}

// EnforceRoleRatio projects replicas into the feasible region described by the
// role ratio constraint. It follows the scale-up-biased repair from the P/D
// disaggregated autoscaling proposal: raise the deficient role first, and only
// reduce the other side if raising would exceed that role's max bound.
func EnforceRoleRatio(replicas map[string]int32, bounds map[string]ReplicaBounds, constraint *workload.RoleRatioConstraint) (map[string]int32, bool, string, error) {
	// Work on a copy so callers can keep the metric/behavior-corrected input for
	// status reporting and tests.
	finalReplicas := make(map[string]int32, len(replicas))
	for role, value := range replicas {
		if b, ok := bounds[role]; ok {
			value = min(max(value, b.Min), b.Max)
		}
		finalReplicas[role] = value
	}
	if constraint == nil {
		return finalReplicas, false, "", nil
	}

	numeratorRole := constraint.NumeratorRole
	denominatorRole := constraint.DenominatorRole
	numerator, ok := finalReplicas[numeratorRole]
	if !ok {
		return finalReplicas, false, "", fmt.Errorf("ratio numerator role %s not found", numeratorRole)
	}
	denominator, ok := finalReplicas[denominatorRole]
	if !ok {
		return finalReplicas, false, "", fmt.Errorf("ratio denominator role %s not found", denominatorRole)
	}
	numeratorBounds, ok := bounds[numeratorRole]
	if !ok {
		return finalReplicas, false, "", fmt.Errorf("bounds for ratio numerator role %s not found", numeratorRole)
	}
	denominatorBounds, ok := bounds[denominatorRole]
	if !ok {
		return finalReplicas, false, "", fmt.Errorf("bounds for ratio denominator role %s not found", denominatorRole)
	}

	// When both sides are zero, preserve coupled scale-to-zero and skip the ratio
	// calculation. When exactly one side is zero, seed that side to one before
	// ratio repair; a P/D deployment with only one live role cannot serve traffic.
	if numerator == 0 && denominator == 0 {
		return finalReplicas, false, "", nil
	}

	minRatio := constraint.MinRatio.AsFloat64Slow()
	maxRatio := constraint.MaxRatio.AsFloat64Slow()
	if math.IsNaN(minRatio) || math.IsNaN(maxRatio) || math.IsInf(minRatio, 0) || math.IsInf(maxRatio, 0) {
		return finalReplicas, false, "", fmt.Errorf("ratio constraint contains non-finite value")
	}

	adjusted := false
	if numerator == 0 {
		numerator = min(max(int32(1), numeratorBounds.Min), numeratorBounds.Max)
		adjusted = true
	}
	if denominator == 0 {
		denominator = min(max(int32(1), denominatorBounds.Min), denominatorBounds.Max)
		adjusted = true
	}
	ratio := float64(numerator) / float64(denominator)
	switch {
	case ratio < minRatio:
		// The numerator side is deficient. Raise it first so the final result
		// preserves metric-requested capacity whenever the max bound allows it.
		raisedNumerator := ceilInt32(minRatio * float64(denominator))
		if raisedNumerator <= numeratorBounds.Max {
			numerator = max(raisedNumerator, numeratorBounds.Min)
		} else if minRatio > 0 {
			// If raising numerator is impossible, saturate it before reducing denominator so we keep as much capacity as possible.
			numerator = numeratorBounds.Max
			denominator = floorInt32(float64(numerator) / minRatio)
			denominator = min(max(denominator, denominatorBounds.Min), denominatorBounds.Max)
		}
		adjusted = true
	case ratio > maxRatio:
		if maxRatio <= 0 {
			numerator = 0
		} else {
			// The denominator side is deficient. Raise it first for the same
			// scale-up-biased reason used in the minimum-ratio branch.
			raisedDenominator := ceilInt32(float64(numerator) / maxRatio)
			if raisedDenominator <= denominatorBounds.Max {
				denominator = max(raisedDenominator, denominatorBounds.Min)
			} else {
				// If denominator cannot be raised enough, saturate it before lowering numerator so we keep as much capacity as possible.
				denominator = denominatorBounds.Max
				numerator = floorInt32(maxRatio * float64(denominator))
				numerator = min(max(numerator, numeratorBounds.Min), numeratorBounds.Max)
			}
		}
		adjusted = true
	}

	finalReplicas[numeratorRole] = min(max(numerator, numeratorBounds.Min), numeratorBounds.Max)
	finalReplicas[denominatorRole] = min(max(denominator, denominatorBounds.Min), denominatorBounds.Max)
	currentRatio := ""
	if finalReplicas[denominatorRole] != 0 {
		currentRatio = strconv.FormatFloat(float64(finalReplicas[numeratorRole])/float64(finalReplicas[denominatorRole]), 'f', -1, 64)
	}
	return finalReplicas, adjusted, currentRatio, nil
}

// ceilInt32 returns ceil(value) with int32 saturation.
func ceilInt32(value float64) int32 {
	if math.IsNaN(value) {
		return 0
	}
	value = math.Ceil(value)
	if value > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	if value < float64(math.MinInt32) {
		return math.MinInt32
	}
	return int32(value)
}

// floorInt32 returns floor(value) with int32 saturation.
func floorInt32(value float64) int32 {
	if math.IsNaN(value) {
		return 0
	}
	value = math.Floor(value)
	if value > float64(math.MaxInt32) {
		return math.MaxInt32
	}
	if value < float64(math.MinInt32) {
		return math.MinInt32
	}
	return int32(value)
}
