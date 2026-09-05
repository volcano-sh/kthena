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
	"math/big"
	"strconv"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ReplicaBounds defines the inclusive replica range for a scalable unit.
type ReplicaBounds struct {
	Min int32
	Max int32
}

// EnforceRoleRatio projects replicas into the feasible integer region described
// by the role ratio constraint. It preserves the scale-up bias from the P/D
// disaggregated autoscaling proposal by preferring candidates that avoid
// reducing the metric-derived replica counts.
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

	minRatio := quantityToRat(constraint.MinRatio)
	maxRatio := quantityToRat(constraint.MaxRatio)
	if minRatio.Sign() < 0 || maxRatio.Sign() <= 0 || minRatio.Cmp(maxRatio) > 0 {
		return finalReplicas, false, "", fmt.Errorf("ratio constraint must satisfy 0 <= minRatio <= maxRatio and maxRatio > 0")
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
	ratioViolated := denominator == 0
	if denominator != 0 {
		ratio := new(big.Rat).SetFrac(big.NewInt(int64(numerator)), big.NewInt(int64(denominator)))
		ratioViolated = ratio.Cmp(minRatio) < 0 || ratio.Cmp(maxRatio) > 0
	}
	if ratioViolated {
		var err error
		numerator, denominator, err = closestRatioConstrainedReplicas(
			numerator, denominator, numeratorBounds, denominatorBounds, minRatio, maxRatio,
		)
		if err != nil {
			return finalReplicas, false, "", err
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

// closestRatioConstrainedReplicas returns the first feasible integer replica
// pair from a deterministic search within the denominator range that can
// intersect the numerator bounds.
func closestRatioConstrainedReplicas(numerator, denominator int32, numeratorBounds, denominatorBounds ReplicaBounds, minRatio, maxRatio *big.Rat) (int32, int32, error) {
	minimumDenominator := max(
		int32(1),
		denominatorBounds.Min,
		ceilRatToInt32(new(big.Rat).Quo(big.NewRat(int64(numeratorBounds.Min), 1), maxRatio)),
	)
	maximumDenominator := denominatorBounds.Max
	if minRatio.Sign() > 0 {
		maximumDenominator = min(maximumDenominator, floorRatToInt32(new(big.Rat).Quo(big.NewRat(int64(numeratorBounds.Max), 1), minRatio)))
	}
	if minimumDenominator > maximumDenominator {
		return 0, 0, fmt.Errorf("ratio constraint has no feasible integer replica pair within role bounds")
	}

	candidateNumerator := func(candidateDenominator int32) (int32, bool) {
		candidateDenominatorRat := big.NewRat(int64(candidateDenominator), 1)
		minimumNumerator := max(numeratorBounds.Min, ceilRatToInt32(new(big.Rat).Mul(minRatio, candidateDenominatorRat)))
		maximumNumerator := min(numeratorBounds.Max, floorRatToInt32(new(big.Rat).Mul(maxRatio, candidateDenominatorRat)))
		if minimumNumerator > maximumNumerator {
			return 0, false
		}
		return min(max(numerator, minimumNumerator), maximumNumerator), true
	}

	startingDenominator := min(max(denominator, minimumDenominator), maximumDenominator)
	if denominator != 0 && new(big.Rat).SetFrac(big.NewInt(int64(numerator)), big.NewInt(int64(denominator))).Cmp(maxRatio) > 0 {
		startingDenominator = min(max(
			startingDenominator,
			ceilRatToInt32(new(big.Rat).Quo(big.NewRat(int64(numerator), 1), maxRatio)),
		), maximumDenominator)
	}

	for candidateDenominator := startingDenominator; ; candidateDenominator++ {
		candidate, feasible := candidateNumerator(candidateDenominator)
		if feasible {
			return candidate, candidateDenominator, nil
		}
		if candidateDenominator == maximumDenominator {
			break
		}
	}

	for candidateDenominator := startingDenominator - 1; candidateDenominator >= minimumDenominator; candidateDenominator-- {
		candidate, feasible := candidateNumerator(candidateDenominator)
		if feasible {
			return candidate, candidateDenominator, nil
		}
	}

	return 0, 0, fmt.Errorf("ratio constraint has no feasible integer replica pair within role bounds")
}

func quantityToRat(quantity resource.Quantity) *big.Rat {
	decimal := quantity.AsDec()
	numerator := new(big.Int).Set(decimal.UnscaledBig())
	denominator := big.NewInt(1)
	scale := int64(decimal.Scale())
	if scale < 0 {
		numerator.Mul(numerator, new(big.Int).Exp(big.NewInt(10), big.NewInt(-scale), nil))
	} else {
		denominator.Exp(big.NewInt(10), big.NewInt(scale), nil)
	}
	return new(big.Rat).SetFrac(numerator, denominator)
}

func ceilRatToInt32(value *big.Rat) int32 {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() != 0 && value.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return bigIntToInt32(quotient)
}

func floorRatToInt32(value *big.Rat) int32 {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() != 0 && value.Sign() < 0 {
		quotient.Sub(quotient, big.NewInt(1))
	}
	return bigIntToInt32(quotient)
}

func bigIntToInt32(value *big.Int) int32 {
	if value.Cmp(big.NewInt(math.MaxInt32)) > 0 {
		return math.MaxInt32
	}
	if value.Cmp(big.NewInt(math.MinInt32)) < 0 {
		return math.MinInt32
	}
	return int32(value.Int64())
}
