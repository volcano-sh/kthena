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

package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/assert"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestStrategyHelpers(t *testing.T) {
	assert.False(t, Enabled(nil))
	assert.False(t, Enabled(&workloadv1alpha1.ModelServing{}))
	assert.Empty(t, Provider(nil))
	assert.False(t, InScope(nil, "prefill"))
	assert.Nil(t, ScopedRoleNames(nil))

	allRoles := &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			BootstrapAccelerateStrategy: &workloadv1alpha1.BootstrapAccelerateStrategy{},
		},
	}
	assert.True(t, Enabled(allRoles))
	assert.Equal(t, workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress, Provider(allRoles))
	assert.True(t, InScope(allRoles, "prefill"))
	assert.True(t, InScope(allRoles, "decode"))
	assert.Nil(t, ScopedRoleNames(allRoles))

	filtered := &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			BootstrapAccelerateStrategy: &workloadv1alpha1.BootstrapAccelerateStrategy{
				Provider: workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress,
				Roles:    []string{"prefill", "decode"},
			},
		},
	}
	assert.True(t, Enabled(filtered))
	assert.Equal(t, workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress, Provider(filtered))
	assert.True(t, InScope(filtered, "prefill"))
	assert.True(t, InScope(filtered, "decode"))
	assert.False(t, InScope(filtered, "router"))

	names := ScopedRoleNames(filtered)
	assert.Equal(t, []string{"prefill", "decode"}, names)
	names[0] = "mutated"
	assert.Equal(t, "prefill", filtered.Spec.BootstrapAccelerateStrategy.Roles[0])
}
