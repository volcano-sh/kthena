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
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GetRouterPod is a helper function to get the router pod
func GetRouterPod(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string) *corev1.Pod {
	deployment, err := kubeClient.AppsV1().Deployments(kthenaNamespace).Get(context.Background(), "kthena-router", metav1.GetOptions{})
	require.NoError(t, err, "Failed to get router deployment")

	// Build label selector from deployment selector
	labelSelector := ""
	for key, value := range deployment.Spec.Selector.MatchLabels {
		if labelSelector != "" {
			labelSelector += ","
		}
		labelSelector += key + "=" + value
	}

	// Get router pod
	pods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list router pods")
	require.NotEmpty(t, pods.Items, "No router pods found")

	return &pods.Items[0]
}

// FetchPodLogs returns recent logs of the specified pod.
// This helper is primarily used by e2e tests to validate router access logs.
func FetchPodLogs(t *testing.T, kubeClient kubernetes.Interface, namespace, podName string, tailLines int64) string {
	t.Helper()

	req := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})

	stream, err := req.Stream(context.Background())
	require.NoError(t, err, "Failed to stream logs for pod %s/%s", namespace, podName)
	defer stream.Close()

	data, err := io.ReadAll(stream)
	require.NoError(t, err, "Failed to read logs for pod %s/%s", namespace, podName)

	return string(data)
}
