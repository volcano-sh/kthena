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
	"math/rand"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const RandomPluginName = "random"

var _ framework.ScorePlugin = &Random{}

// Random is a score plugin that assigns random scores to pods.
// This plugin should be used independently and not mixed with other scoring plugins,
// as combining random scores with meaningful scores defeats the purpose of intelligent scheduling.
// It is primarily intended for testing purposes to validate scheduler behavior with random pod selection.
type Random struct {
	name string
}

func NewRandom(pluginArg runtime.RawExtension) *Random {
	return &Random{
		name: RandomPluginName,
	}
}

func (r *Random) Name() string {
	return r.name
}

// Score assigns random scores to pods within the range [0, 100]
func (r *Random) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)

	if len(pods) == 0 {
		return scoreResults
	}

	// Assign random scores between 0 and 100 to each pod
	for _, pod := range pods {
		score := rand.Intn(101) // Generate random number between 0-100 (inclusive)
		scoreResults[pod] = score
	}

	return scoreResults
}
