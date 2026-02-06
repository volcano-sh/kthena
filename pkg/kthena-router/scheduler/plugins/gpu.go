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
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

var _ framework.ScorePlugin = &GPUCacheUsage{}

const GPUCacheUsagePluginName = "gpu-usage"

type GPUCacheUsage struct {
	name string
}

func NewGPUCacheUsage() *GPUCacheUsage {
	return &GPUCacheUsage{
		name: GPUCacheUsagePluginName,
	}
}

func (g *GPUCacheUsage) Name() string {
	return g.name
}
func (g *GPUCacheUsage) Score(ctx *framework.Context, pods []*datastore.PodInfo) map[*datastore.PodInfo]int {
	scoreResults := make(map[*datastore.PodInfo]int)
	for _, info := range pods {
		score := int((1.0 - info.GetGPUCacheUsage()) * 100)
		scoreResults[info] = score
	}

	return scoreResults
}
