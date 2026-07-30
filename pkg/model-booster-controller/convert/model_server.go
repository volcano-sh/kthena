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
	"fmt"
	"time"

	networking "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	icUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var VLLMKvConnectorType = map[string]networking.KVConnectorType{
	"MooncakeConnector":  networking.ConnectorTypeMoonCake,
	"NixlConnector":      networking.ConnectorTypeNIXL,
	"LMCacheConnectorV1": networking.ConnectorTypeLMCache,
}

const (
	vLLMKVRoleProducer = "kv_producer"
	vLLMKVRoleConsumer = "kv_consumer"
)

// BuildModelServer creates arrays of ModelServer for the given model.
// Each model backend will create one model server.
func BuildModelServer(model *workload.ModelBooster) ([]*networking.ModelServer, error) {
	var modelServers []*networking.ModelServer
	var backend = model.Spec.Backend
	var inferenceEngine networking.InferenceEngine
	switch backend.Type {
	case workload.ModelBackendTypeVLLM, workload.ModelBackendTypeVLLMDisaggregated:
		inferenceEngine = networking.VLLM
	default:
		return nil, fmt.Errorf("not support %s backend yet, please use vLLM backend", backend.Type)
	}
	servedModelName := getServedModelName(model, backend)
	pdGroup := getPdGroup(backend)
	kvConnector, err := getKvConnectorSpec(backend)
	if err != nil {
		return nil, err
	}
	modelServer := networking.ModelServer{
		TypeMeta: metav1.TypeMeta{
			Kind:       networking.ModelServerKind,
			APIVersion: networking.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      utils.GetBackendResourceName(model.Name, backend.Name),
			Namespace: model.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				utils.NewModelOwnerRef(model),
			},
		},
		Spec: networking.ModelServerSpec{
			Model:           &servedModelName,
			InferenceEngine: inferenceEngine,
			WorkloadSelector: &networking.WorkloadSelector{
				MatchLabels: map[string]string{
					utils.OwnerUIDKey: string(model.UID),
				},
				PDGroup: pdGroup,
			},
			WorkloadPort: networking.WorkloadPort{
				Port: 8000, // todo: get port from config
			},
			TrafficPolicy: &networking.TrafficPolicy{
				Retry: &networking.Retry{
					Attempts:      5,
					RetryInterval: &metav1.Duration{Duration: time.Duration(0) * time.Second},
				},
			},
			KVConnector: kvConnector,
		},
	}
	modelServer.Labels = utils.GetModelControllerLabels(model, backend.Name, icUtils.Revision(modelServer.Spec))
	modelServers = append(modelServers, &modelServer)

	return modelServers, nil
}

func getKvConnectorSpec(backend workload.ModelBackend) (*networking.KVConnectorSpec, error) {
	var connectorType networking.KVConnectorType
	var connectorName string
	foundConfig := false
	var workersMissingConfig []workload.ModelWorkerType

	for _, worker := range backend.Workers {
		if worker.Type != workload.ModelWorkerTypePrefill && worker.Type != workload.ModelWorkerTypeDecode {
			continue
		}

		if len(worker.Config.Raw) == 0 {
			workersMissingConfig = append(workersMissingConfig, worker.Type)
			continue
		}
		kvTransferConfig, err := utils.TryGetField(worker.Config.Raw, "kv-transfer-config")
		if err != nil {
			return nil, fmt.Errorf("failed to get kv-transfer-config for worker %s: %w", worker.Type, err)
		}
		if kvTransferConfig == nil {
			workersMissingConfig = append(workersMissingConfig, worker.Type)
			continue
		}
		kvTransferConfigStr, ok := kvTransferConfig.(string)
		if !ok {
			return nil, fmt.Errorf("kv-transfer-config for worker %s must be a string, got %T", worker.Type, kvTransferConfig)
		}

		kvTransferType, err := utils.TryGetField([]byte(kvTransferConfigStr), "kv_connector")
		if err != nil {
			return nil, fmt.Errorf("failed to get kv_connector for worker %s: %w", worker.Type, err)
		}
		if kvTransferType == nil {
			return nil, fmt.Errorf("worker %s missing kv_connector", worker.Type)
		}

		converted, ok := kvTransferType.(string)
		if !ok || converted == "" {
			return nil, fmt.Errorf("kv_connector for worker %s must be a non-empty string, got %T", worker.Type, kvTransferType)
		}
		currentConnectorType, exists := VLLMKvConnectorType[converted]
		if !exists {
			return nil, fmt.Errorf("unknown kv_connector type %q for worker %s", converted, worker.Type)
		}

		kvRole, err := utils.TryGetField([]byte(kvTransferConfigStr), "kv_role")
		if err != nil {
			return nil, fmt.Errorf("failed to get kv_role for worker %s: %w", worker.Type, err)
		}
		if kvRole == nil {
			return nil, fmt.Errorf("worker %s missing kv_role", worker.Type)
		}
		kvRoleStr, ok := kvRole.(string)
		if !ok {
			return nil, fmt.Errorf("kv_role for worker %s must be a string, got %T", worker.Type, kvRole)
		}
		expectedRole := vLLMKVRoleProducer
		if worker.Type == workload.ModelWorkerTypeDecode {
			expectedRole = vLLMKVRoleConsumer
		}
		if kvRoleStr != expectedRole {
			return nil, fmt.Errorf("worker %s kv_role must be %q, got %q", worker.Type, expectedRole, kvRoleStr)
		}

		if foundConfig && converted != connectorName {
			return nil, fmt.Errorf(
				"workers must use the same kv_connector: got %q and %q",
				connectorName,
				converted,
			)
		}
		connectorName = converted
		connectorType = currentConnectorType
		foundConfig = true
	}

	if !foundConfig {
		return nil, nil
	}
	if len(workersMissingConfig) > 0 {
		return nil, fmt.Errorf(
			"worker %s missing kv-transfer-config while another PD worker configures kv_connector %q",
			workersMissingConfig[0],
			connectorName,
		)
	}

	return &networking.KVConnectorSpec{Type: connectorType}, nil
}

// ValidateKVConnectorConfig validates the connector and role contract shared by PD workers.
func ValidateKVConnectorConfig(backend workload.ModelBackend) error {
	_, err := getKvConnectorSpec(backend)
	return err
}

func getPdGroup(backend workload.ModelBackend) *networking.PDGroup {
	switch backend.Type {
	case workload.ModelBackendTypeVLLMDisaggregated, workload.ModelBackendTypeMindIEDisaggregated:
		return &networking.PDGroup{
			GroupKey: workload.GroupNameLabelKey,
			PrefillLabels: map[string]string{
				workload.RoleLabelKey: string(workload.ModelWorkerTypePrefill),
			},
			DecodeLabels: map[string]string{
				workload.RoleLabelKey: string(workload.ModelWorkerTypeDecode),
			},
		}
	}
	return nil
}

// getServedModelName gets served model name from the worker config. Default is the model name.
func getServedModelName(model *workload.ModelBooster, backend workload.ModelBackend) string {
	servedModelName := model.Name
	for _, worker := range backend.Workers {
		if worker.Type == workload.ModelWorkerTypeServer ||
			worker.Type == workload.ModelWorkerTypeDecode {
			valStr, err := utils.TryGetField(worker.Config.Raw, "served-model-name")
			if err != nil {
				return servedModelName
			}
			if valStr == nil {
				continue
			}
			if val, ok := valStr.(string); ok {
				servedModelName = val
				break
			}
		}
	}
	return servedModelName
}
