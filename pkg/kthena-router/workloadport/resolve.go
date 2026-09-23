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

package workloadport

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

// Resolve returns the port to use for this Pod. A named port must be present
// exactly once among the Pod's regular containers and use TCP.
func Resolve(spec networkingv1alpha1.WorkloadPort, pod *corev1.Pod) (int32, error) {
	if spec.PortName == "" {
		if spec.Port < 1 || spec.Port > 65535 {
			return 0, fmt.Errorf("invalid workload port %d", spec.Port)
		}
		return spec.Port, nil
	}
	if spec.Port != 0 {
		return 0, fmt.Errorf("both workload port and portName are set")
	}
	if pod == nil {
		return 0, fmt.Errorf("cannot resolve portName %q without a Pod", spec.PortName)
	}
	var resolved int32
	for _, container := range pod.Spec.Containers {
		for _, port := range container.Ports {
			if port.Name != spec.PortName {
				continue
			}
			if resolved != 0 {
				return 0, fmt.Errorf("Pod %s/%s declares portName %q more than once", pod.Namespace, pod.Name, spec.PortName)
			}
			if port.Protocol != "" && port.Protocol != corev1.ProtocolTCP {
				return 0, fmt.Errorf("Pod %s/%s portName %q must use TCP", pod.Namespace, pod.Name, spec.PortName)
			}
			if port.ContainerPort < 1 || port.ContainerPort > 65535 {
				return 0, fmt.Errorf("Pod %s/%s portName %q has invalid port %d", pod.Namespace, pod.Name, spec.PortName, port.ContainerPort)
			}
			resolved = port.ContainerPort
		}
	}
	if resolved == 0 {
		return 0, fmt.Errorf("Pod %s/%s does not declare portName %q", pod.Namespace, pod.Name, spec.PortName)
	}
	return resolved, nil
}
