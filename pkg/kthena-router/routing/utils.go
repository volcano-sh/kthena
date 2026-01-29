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

package routing

import (
	"math/rand"
	"time"
)

// selectFromWeights randomly selects an index from a weighted slice
// The probability of selecting index i is proportional to weights[i]
func selectFromWeights(weights []uint32) int {
	if len(weights) == 0 {
		return 0
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	totalWeight := uint32(0)
	for _, weight := range weights {
		totalWeight += weight
	}

	if totalWeight == 0 {
		return 0
	}

	randomNum := uint32(rng.Intn(int(totalWeight)))

	for i, weight := range weights {
		if randomNum < weight {
			return i
		}
		randomNum -= weight
	}

	return 0
}
