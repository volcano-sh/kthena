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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

const (
	// EndpointPortAnnotation carries the per-endpoint port of a statically
	// configured model serving instance. It is only set on the synthetic pods
	// built by EndpointPod and overrides `spec.workloadPort.port`.
	EndpointPortAnnotation = "networking.serving.volcano.sh/endpoint-port"
	// StaticEndpointLabel marks a synthetic pod as originating from
	// `ModelServer.spec.endpoints` rather than from the Kubernetes API server.
	StaticEndpointLabel = "networking.serving.volcano.sh/static-endpoint"
)

// EndpointPodName returns the name of the synthetic pod representing the given
// endpoint. Endpoint names are only unique within one ModelServer, so the name
// combines both. The ":" separator cannot occur in Kubernetes object names,
// which keeps the mapping unambiguous and avoids collisions with real pods.
func EndpointPodName(modelServerName, endpointName string) string {
	return modelServerName + ":" + endpointName
}

// EndpointPod builds the synthetic pod representing a statically configured
// endpoint of the given ModelServer. Representing endpoints as pods lets the
// whole router pipeline (scheduling, metrics scraping, proxying) treat static
// and discovered instances identically.
func EndpointPod(ms *aiv1alpha1.ModelServer, endpoint aiv1alpha1.Endpoint) *corev1.Pod {
	labels := map[string]string{StaticEndpointLabel: "true"}
	if ms.Spec.WorkloadSelector != nil {
		for k, v := range ms.Spec.WorkloadSelector.MatchLabels {
			labels[k] = v
		}
	}
	for k, v := range endpoint.Labels {
		labels[k] = v
	}

	var annotations map[string]string
	if endpoint.Port != nil {
		annotations = map[string]string{
			EndpointPortAnnotation: strconv.FormatInt(int64(*endpoint.Port), 10),
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        EndpointPodName(ms.Name, endpoint.Name),
			Namespace:   ms.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: endpoint.Address,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// EndpointPort returns the port to reach the given pod on. Synthetic pods built
// from `ModelServer.spec.endpoints` may carry a per-endpoint port; every other
// pod uses the ModelServer's workload port passed as fallback.
func EndpointPort(pod *corev1.Pod, fallback int32) int32 {
	if pod == nil {
		return fallback
	}
	value, ok := pod.Annotations[EndpointPortAnnotation]
	if !ok {
		return fallback
	}
	port, err := strconv.ParseInt(value, 10, 32)
	if err != nil || port <= 0 || port > 65535 {
		return fallback
	}
	return int32(port)
}
