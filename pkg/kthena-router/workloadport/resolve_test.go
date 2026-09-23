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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

func TestResolveNamedPortPerPod(t *testing.T) {
	selector := networkingv1alpha1.WorkloadPort{PortName: "inference"}
	for _, want := range []int32{7100, 7107} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "server"},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Ports: []corev1.ContainerPort{
				{Name: "inference", ContainerPort: want, Protocol: corev1.ProtocolTCP},
			}}}},
		}
		got, err := Resolve(selector, pod)
		if err != nil || got != want {
			t.Fatalf("Pod port %d resolved to %d, %v", want, got, err)
		}
	}
	got, err := Resolve(networkingv1alpha1.WorkloadPort{Port: 8000}, nil)
	if err != nil || got != 8000 {
		t.Fatalf("static port resolved to %d, %v", got, err)
	}
}

func TestResolveRejectsAmbiguousOrInvalidNamedPort(t *testing.T) {
	for _, ports := range [][]corev1.ContainerPort{
		nil,
		{{Name: "inference", ContainerPort: 7100}, {Name: "inference", ContainerPort: 7101}},
		{{Name: "inference", ContainerPort: 7100, Protocol: corev1.ProtocolUDP}},
		{{Name: "inference", ContainerPort: 0}},
	} {
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Ports: ports}}}}
		if _, err := Resolve(networkingv1alpha1.WorkloadPort{PortName: "inference"}, pod); err == nil {
			t.Fatalf("invalid named port declarations accepted: %+v", ports)
		}
	}
	if _, err := Resolve(networkingv1alpha1.WorkloadPort{Port: 8000, PortName: "inference"}, &corev1.Pod{}); err == nil {
		t.Fatal("static and named port together accepted")
	}
}
