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
	"github.com/stretchr/testify/require"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestStrategyHelpers(t *testing.T) {
	ms := testModelServing(1, testRole("prefill", 1), testRole("decode", 1))
	assert.False(t, Enabled(ms))
	assert.Empty(t, Provider(ms))
	assert.Nil(t, ScopedRoleNames(ms))
	_, ok := RoleConfig(ms, "prefill")
	assert.False(t, ok)

	ms.Spec.BootstrapAccelerateStrategy = &workloadv1alpha1.BootstrapAccelerateStrategy{}
	assert.False(t, Enabled(ms), "roles are required")

	withStrategy(ms, seed("decode", nil), seed("prefill", nil))
	assert.True(t, Enabled(ms))
	assert.Equal(t, workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress, Provider(ms), "provider defaults to ModelExpress")
	assert.Equal(t, []string{"decode", "prefill"}, ScopedRoleNames(ms))
	config, ok := RoleConfig(ms, "decode")
	require.True(t, ok)
	assert.Equal(t, "decode", config.Name)

	ms.Spec.BootstrapAccelerateStrategy.Provider = "Other"
	assert.Equal(t, workloadv1alpha1.BootstrapAccelerateStrategyProvider("Other"), Provider(ms))
}
