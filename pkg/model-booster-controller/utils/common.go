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

package utils

import (
	"encoding/json"
	"fmt"
	"os"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

const (
	inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

var XPUList = []corev1.ResourceName{"nvidia.com/gpu", "huawei.com/ascend-1980"}

func TryGetField(config []byte, key string) (any, error) {
	var configMap map[string]interface{}
	if err := json.Unmarshal(config, &configMap); err != nil {
		return "", fmt.Errorf("failed to unmarshal config: %w", err)
	}
	if _, exists := configMap[key]; !exists {
		return nil, nil
	}
	return configMap[key], nil
}

func GetDeviceNum(worker *workloadv1alpha1.ModelWorker) int64 {
	sum := int64(0)
	// For GPU (extended) resources, if limits are specified, Kubernetes defaults requests to match limits.
	// Requests may be absent according to k8s docs, so we rely on limits here: https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/
	if worker.Resources.Limits != nil {
		for _, xpu := range XPUList {
			if val, exists := worker.Resources.Limits[xpu]; exists {
				sum += val.Value()
			}
		}
	}
	return sum
}

func NewModelOwnerRef(model *workloadv1alpha1.ModelBooster) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         workloadv1alpha1.GroupVersion.String(),
		Kind:               workloadv1alpha1.ModelKind.Kind,
		Name:               model.Name,
		UID:                model.UID,
		BlockOwnerDeletion: ptr.To(true),
		Controller:         ptr.To(true),
	}
}

// GetInClusterNameSpace gets the namespace of model controller
func GetInClusterNameSpace() (string, error) {
	if _, err := os.Stat(inClusterNamespacePath); os.IsNotExist(err) {
		return "", fmt.Errorf("not running in-cluster, please specify namespace")
	} else if err != nil {
		return "", fmt.Errorf("error checking namespace file: %v", err)
	}
	// Load the namespace file and return its content
	namespace, err := os.ReadFile(inClusterNamespacePath)
	if err != nil {
		return "", fmt.Errorf("error reading namespace file: %v", err)
	}
	return string(namespace), nil
}
