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
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/framework"
)

const LoraAffinityPluginName = "lora-affinity"

type LoraAffinity struct {
	name string
}

var _ framework.FilterPlugin = &LoraAffinity{}

func NewLoraAffinity() *LoraAffinity {
	return &LoraAffinity{
		name: LoraAffinityPluginName,
	}
}

func (l *LoraAffinity) Name() string {
	return l.name
}

func (l *LoraAffinity) Filter(ctx *framework.Context, pods []*datastore.PodInfo) []*datastore.PodInfo {
	return slices.DeleteFunc(pods, func(info *datastore.PodInfo) bool {
		return !info.Contains(ctx.Model)
	})
}

// ValidateLoraAffinityArgs validates the lora-affinity plugin arguments: the
// plugin takes no arguments, so any non-empty args are rejected.
func ValidateLoraAffinityArgs(pluginArg runtime.RawExtension) error {
	if len(pluginArg.Raw) > 0 {
		return fmt.Errorf("lora-affinity plugin takes no arguments")
	}
	return nil
}
