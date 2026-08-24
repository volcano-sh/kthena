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

package convert

import (
	"testing"

	"github.com/stretchr/testify/assert"
	networking "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	registry "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildModelServer(t *testing.T) {
	tests := []struct {
		name         string
		input        *registry.ModelBooster
		expected     []*networking.ModelServer
		expectErrMsg string
	}{
		{
			name:     "normal case with VLLM backend",
			input:    loadYaml[registry.ModelBooster](t, "testdata/input/model.yaml"),
			expected: []*networking.ModelServer{loadYaml[networking.ModelServer](t, "testdata/expected/model-server.yaml")},
		},
		{
			name:     "PD disaggregation case",
			input:    loadYaml[registry.ModelBooster](t, "testdata/input/pd-disaggregated-model-npu.yaml"),
			expected: []*networking.ModelServer{loadYaml[networking.ModelServer](t, "testdata/expected/pd-model-server.yaml")},
		},
		{
			name: "invalid backend type",
			input: &registry.ModelBooster{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-backend",
					Namespace: "default",
				},
				Spec: registry.ModelBoosterSpec{
					Backend: registry.ModelBackend{
						Name: "invalid",
						Type: "InvalidType",
					},
				},
			},
			expectErrMsg: "not support InvalidType backend yet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildModelServer(tt.input)
			if tt.expectErrMsg != "" {
				assert.Contains(t, err.Error(), tt.expectErrMsg)
				return
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildModelServerCustomPort(t *testing.T) {
	model := loadYaml[registry.ModelBooster](t, "testdata/input/model.yaml")
	model.Spec.Backend.Workers[0].Config.Raw = []byte(`{"port":9000}`)

	servers, err := BuildModelServer(model)

	assert.NoError(t, err)
	assert.Equal(t, int32(9000), servers[0].Spec.WorkloadPort.Port)
}

func TestBuildModelServerExplicitServedModelNameOverridesModelBoosterSpecName(t *testing.T) {
	model := loadYaml[registry.ModelBooster](t, "testdata/input/model.yaml")
	model.Spec.Name = "client-facing-model-name"

	modelServers, err := BuildModelServer(model)
	assert.NoError(t, err)
	assert.Len(t, modelServers, 1)
	assert.NotNil(t, modelServers[0].Spec.Model)
	assert.Equal(t, "deepseek-v3", *modelServers[0].Spec.Model)
}

func mooncakeWorker(workerType registry.ModelWorkerType, role string) registry.ModelWorker {
	return registry.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"MooncakeConnector\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func nixlWorker(workerType registry.ModelWorkerType, role string) registry.ModelWorker {
	return registry.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"NixlConnector\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func mooncakeV1Worker(workerType registry.ModelWorkerType, role string) registry.ModelWorker {
	return registry.ModelWorker{
		Type: workerType,
		Config: apiextensionsv1.JSON{
			Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"MooncakeConnectorV1\",\"kv_role\":\"` + role + `\"}"}`),
		},
	}
}

func TestGetKvConnectorSpec(t *testing.T) {
	tests := []struct {
		name         string
		workers      []registry.ModelWorker
		expected     *networking.KVConnectorSpec
		expectErrMsg string
	}{
		{
			name: "no workers is valid",
			workers: []registry.ModelWorker{
				{Type: registry.ModelWorkerTypeServer},
			},
			expected: nil,
		},
		{
			name: "server worker is ignored",
			workers: []registry.ModelWorker{
				{
					Type: registry.ModelWorkerTypeServer,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"MooncakeConnector\"}"}`),
					},
				},
			},
			expected: nil,
		},
		{
			name: "PD backend with no kv-transfer-config on either worker is valid",
			workers: []registry.ModelWorker{
				{Type: registry.ModelWorkerTypePrefill},
				{Type: registry.ModelWorkerTypeDecode},
			},
			expected: nil,
		},
		{
			name: "prefill producer + decode consumer with matching NIXL connector succeeds",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expected: &networking.KVConnectorSpec{Type: networking.ConnectorTypeNIXL},
		},
		{
			name: "matching Mooncake connector succeeds",
			workers: []registry.ModelWorker{
				mooncakeWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expected: &networking.KVConnectorSpec{Type: networking.ConnectorTypeMoonCake},
		},
		{
			// Matches the vllm-ascend connector name used by the existing
			// examples/model-booster/prefill-decode-disaggregation.yaml ModelBooster example.
			name: "matching MooncakeConnectorV1 connector succeeds",
			workers: []registry.ModelWorker{
				mooncakeV1Worker(registry.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeV1Worker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expected: &networking.KVConnectorSpec{Type: networking.ConnectorTypeMoonCake},
		},
		{
			name: "kv_both role is valid for both prefill and decode",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_both"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_both"),
			},
			expected: &networking.KVConnectorSpec{Type: networking.ConnectorTypeNIXL},
		},
		{
			name: "worker order does not change the result",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
			},
			expected: &networking.KVConnectorSpec{Type: networking.ConnectorTypeNIXL},
		},
		{
			name: "duplicate prefill workers with different connectors are rejected regardless of order",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "backend test-backend: found 2 prefill workers, expected at most 1",
		},
		{
			name: "duplicate prefill workers (reversed order) are rejected the same way",
			workers: []registry.ModelWorker{
				mooncakeWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "backend test-backend: found 2 prefill workers, expected at most 1",
		},
		{
			name: "duplicate decode workers with different connectors are rejected regardless of order",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
				mooncakeWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "backend test-backend: found 2 decode workers, expected at most 1",
		},
		{
			name: "duplicate decode workers (reversed order) are rejected the same way",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "backend test-backend: found 2 decode workers, expected at most 1",
		},
		{
			name: "duplicate prefill workers with identical connectors are still rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "backend test-backend: found 2 prefill workers, expected at most 1",
		},
		{
			name: "prefill NIXL + decode Mooncake mismatch is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				mooncakeWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: `kv_connector mismatch between prefill ("NixlConnector") and decode ("MooncakeConnector")`,
		},
		{
			name: "prefill Mooncake + decode NIXL mismatch is rejected regardless of order",
			workers: []registry.ModelWorker{
				mooncakeWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: `kv_connector mismatch between prefill ("MooncakeConnector") and decode ("NixlConnector")`,
		},
		{
			name: "prefill with connector but decode missing entirely is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
			},
			expectErrMsg: "decode worker has no kv-transfer-config",
		},
		{
			name: "decode with connector but prefill missing entirely is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "prefill worker has no kv-transfer-config",
		},
		{
			name: "prefill missing kv-transfer-config while decode has one is rejected",
			workers: []registry.ModelWorker{
				{Type: registry.ModelWorkerTypePrefill},
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "prefill worker has no kv-transfer-config",
		},
		{
			name: "decode missing kv-transfer-config while prefill has one is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				{Type: registry.ModelWorkerTypeDecode},
			},
			expectErrMsg: "decode worker has no kv-transfer-config",
		},
		{
			name: "missing kv_connector field is rejected",
			workers: []registry.ModelWorker{
				{
					Type: registry.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{\"kv_role\":\"kv_producer\"}"}`),
					},
				},
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: "worker prefill: kv-transfer-config is set but kv_connector is missing",
		},
		{
			name: "unknown kv_connector is rejected",
			workers: []registry.ModelWorker{
				{
					Type: registry.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{\"kv_connector\":\"UnknownConnector\",\"kv_role\":\"kv_producer\"}"}`),
					},
				},
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: `worker prefill: unsupported kv_connector "UnknownConnector"`,
		},
		{
			name: "prefill with kv_consumer role is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_consumer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_consumer"),
			},
			expectErrMsg: `worker prefill: invalid kv_role "kv_consumer", expected one of [kv_producer, kv_both]`,
		},
		{
			name: "decode with kv_producer role is rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_producer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_producer"),
			},
			expectErrMsg: `worker decode: invalid kv_role "kv_producer", expected one of [kv_consumer, kv_both]`,
		},
		{
			name: "reversed roles across prefill and decode are rejected",
			workers: []registry.ModelWorker{
				nixlWorker(registry.ModelWorkerTypePrefill, "kv_consumer"),
				nixlWorker(registry.ModelWorkerTypeDecode, "kv_producer"),
			},
			expectErrMsg: `worker prefill: invalid kv_role "kv_consumer"`,
		},
		{
			name: "malformed nested kv-transfer-config returns error",
			workers: []registry.ModelWorker{
				{
					Type: registry.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{"kv-transfer-config":"{"}`),
					},
				},
			},
			expectErrMsg: "worker prefill: invalid kv-transfer-config",
		},
		{
			name: "malformed worker config returns error",
			workers: []registry.ModelWorker{
				{
					Type: registry.ModelWorkerTypePrefill,
					Config: apiextensionsv1.JSON{
						Raw: []byte(`{`),
					},
				},
			},
			expectErrMsg: "worker prefill: invalid config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetKvConnectorSpec(registry.ModelBackend{
				Name:    "test-backend",
				Workers: tt.workers,
			})
			if tt.expectErrMsg != "" {
				if assert.Error(t, err) {
					assert.Contains(t, err.Error(), tt.expectErrMsg)
				}
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expected, got)
		})
	}
}
