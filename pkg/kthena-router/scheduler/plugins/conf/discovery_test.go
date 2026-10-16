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

package conf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseEndpointDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`scheduler:
  plugins:
    Score:
      enabled:
        - name: least-request
          weight: 1
endpointDiscovery:
  provider: custom
  refreshInterval: 5s
  snapshotTimeout: 2s
  mappings:
    - modelServer: default/model
      parameters:
        service: model-service
`), 0600))
	config, err := ParseRouterConfig(path)
	require.NoError(t, err)
	require.NoError(t, config.EndpointDiscovery.Validate())
	require.True(t, config.EndpointDiscovery.External())
	require.Equal(t, "custom", config.EndpointDiscovery.Provider)
	require.Equal(t, "model-service", config.EndpointDiscovery.Mappings[0].Parameters["service"])
	require.Equal(t, "least-request", config.Scheduler.Plugins.Score.Enabled[0].Name)
}
