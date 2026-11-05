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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

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

func TestConfigHash(t *testing.T) {
	base := func() *workloadv1alpha1.ModelServing {
		ms := withStrategy(testModelServing(2, testRole("prefill", 1), testRole("decode", 1)), seed("prefill", nil))
		ms.Spec.BootstrapAccelerateStrategy.ModelExpress = &workloadv1alpha1.ModelExpressConfig{
			EngineContainers: []string{"engine", "sidecar-engine"},
			ReadyURL:         "http://127.0.0.1:8000/health",
		}
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{
			Name: "demo-pod-tweaks", Type: workloadv1alpha1.PluginTypeBuiltIn,
			Config: &apiextensionsv1.JSON{Raw: []byte(`{"annotations":{"a":"1"},"runtimeClassName":"nvidia"}`)},
		}}
		return ms
	}
	reference, err := ConfigHash(base(), "prefill", "mx:8001")
	require.NoError(t, err)
	require.NotEmpty(t, reference)

	outOfScope, err := ConfigHash(base(), "decode", "mx:8001")
	require.NoError(t, err)
	assert.Empty(t, outOfScope, "out-of-scope roles have no hash")

	same := []struct {
		name   string
		mutate func(*workloadv1alpha1.ModelServing)
	}{
		{name: "identical", mutate: func(*workloadv1alpha1.ModelServing) {}},
		{name: "seed replicas", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Roles[0].InitialSeedReplicas = ptr.To(intstr.FromString("50%"))
		}},
		{name: "replica counts", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Replicas = ptr.To[int32](16)
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
		}},
		{name: "readyURL", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.ReadyURL = "http://127.0.0.1:9000/ready"
		}},
		{name: "engine container order", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"sidecar-engine", "engine"}
		}},
		{name: "plugin JSON key order", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[0].Config = &apiextensionsv1.JSON{Raw: []byte(`{"runtimeClassName":"nvidia","annotations":{"a":"1"}}`)}
		}},
		{name: "explicit default provider", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.Provider = workloadv1alpha1.BootstrapAccelerateStrategyProviderModelExpress
		}},
		{name: "role template", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "prefill:v2"
		}},
	}
	for _, tt := range same {
		t.Run("same/"+tt.name, func(t *testing.T) {
			ms := base()
			tt.mutate(ms)
			hash, err := ConfigHash(ms, "prefill", "mx:8001")
			require.NoError(t, err)
			assert.Equal(t, reference, hash)
		})
	}

	different := []struct {
		name    string
		address string
		mutate  func(*workloadv1alpha1.ModelServing)
	}{
		{name: "server address", address: "other:8001", mutate: func(*workloadv1alpha1.ModelServing) {}},
		{name: "engine containers", address: "mx:8001", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress.EngineContainers = []string{"engine"}
		}},
		{name: "modelExpress removed", address: "mx:8001", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.BootstrapAccelerateStrategy.ModelExpress = nil
		}},
		{name: "plugin configuration", address: "mx:8001", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins[0].Config = &apiextensionsv1.JSON{Raw: []byte(`{"runtimeClassName":"kata"}`)}
		}},
		{name: "plugin removed", address: "mx:8001", mutate: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins = nil
		}},
	}
	for _, tt := range different {
		t.Run("different/"+tt.name, func(t *testing.T) {
			ms := base()
			tt.mutate(ms)
			hash, err := ConfigHash(ms, "prefill", tt.address)
			require.NoError(t, err)
			assert.NotEqual(t, reference, hash)
		})
	}

	t.Run("per role", func(t *testing.T) {
		ms := base()
		ms.Spec.BootstrapAccelerateStrategy.Roles = append(ms.Spec.BootstrapAccelerateStrategy.Roles, seed("decode", nil))
		prefill, err := ConfigHash(ms, "prefill", "mx:8001")
		require.NoError(t, err)
		decode, err := ConfigHash(ms, "decode", "mx:8001")
		require.NoError(t, err)
		assert.Equal(t, reference, prefill, "adding another role keeps this role's pool")
		assert.NotEqual(t, prefill, decode)
	})
}
