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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestEndpointPod(t *testing.T) {
	port := int32(9000)
	tests := []struct {
		name                string
		modelServer         *aiv1alpha1.ModelServer
		endpoint            aiv1alpha1.Endpoint
		expectedLabels      map[string]string
		expectedAnnotations map[string]string
	}{
		{
			name: "endpoint without selector or port",
			modelServer: &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "kthena"},
			},
			endpoint: aiv1alpha1.Endpoint{Name: "vllm-0", Address: "10.0.0.1"},
			expectedLabels: map[string]string{
				StaticEndpointLabel: "true",
			},
		},
		{
			name: "endpoint labels are merged over the selector labels",
			modelServer: &aiv1alpha1.ModelServer{
				ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "kthena"},
				Spec: aiv1alpha1.ModelServerSpec{
					WorkloadSelector: &aiv1alpha1.WorkloadSelector{
						MatchLabels: map[string]string{"app": "vllm", "role": "unset"},
					},
				},
			},
			endpoint: aiv1alpha1.Endpoint{
				Name:    "vllm-0",
				Address: "vllm-0.example.com",
				Port:    &port,
				Labels:  map[string]string{"role": "prefill"},
			},
			expectedLabels: map[string]string{
				StaticEndpointLabel: "true",
				"app":               "vllm",
				"role":              "prefill",
			},
			expectedAnnotations: map[string]string{EndpointPortAnnotation: "9000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := EndpointPod(tt.modelServer, tt.endpoint)

			assert.Equal(t, EndpointPodName(tt.modelServer.Name, tt.endpoint.Name), pod.Name)
			assert.Equal(t, tt.modelServer.Namespace, pod.Namespace)
			assert.Equal(t, tt.endpoint.Address, pod.Status.PodIP)
			assert.Equal(t, corev1.PodRunning, pod.Status.Phase)
			assert.Equal(t, tt.expectedLabels, pod.Labels)
			assert.Equal(t, tt.expectedAnnotations, pod.Annotations)
			assert.Contains(t, pod.Status.Conditions, corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			})
		})
	}
}

func TestEndpointPodName(t *testing.T) {
	// The separator must be unambiguous: endpoint names are only unique within
	// one ModelServer and real pods can never carry a ":" in their name.
	assert.Equal(t, "ms:vllm-0", EndpointPodName("ms", "vllm-0"))
}

func TestEndpointPort(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		fallback int32
		expected int32
	}{
		{
			name:     "nil pod falls back",
			fallback: 8080,
			expected: 8080,
		},
		{
			name:     "pod without annotation falls back",
			pod:      &corev1.Pod{},
			fallback: 8080,
			expected: 8080,
		},
		{
			name: "annotation overrides the fallback",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{EndpointPortAnnotation: "9000"},
			}},
			fallback: 8080,
			expected: 9000,
		},
		{
			name: "invalid annotation falls back",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{EndpointPortAnnotation: "not-a-port"},
			}},
			fallback: 8080,
			expected: 8080,
		},
		{
			name: "out of range annotation falls back",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{EndpointPortAnnotation: "70000"},
			}},
			fallback: 8080,
			expected: 8080,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, EndpointPort(tt.pod, tt.fallback))
		})
	}
}
