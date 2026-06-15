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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// GetRouterPod is a helper function to get the first ready router pod.
func GetRouterPod(t *testing.T, kubeClient kubernetes.Interface, namespace string) *corev1.Pod {
	t.Helper()
	readyPods := GetReadyRouterPods(t, kubeClient, namespace)
	return &readyPods[0]
}

// GetReadyRouterPods returns all ready pods for the kthena-router deployment.
func GetReadyRouterPods(t *testing.T, kubeClient kubernetes.Interface, namespace string) []corev1.Pod {
	t.Helper()
	ctx := context.Background()
	deployment, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, "kthena-router", metav1.GetOptions{})
	require.NoError(t, err, "Failed to get kthena-router deployment")

	// Build label selector from deployment selector
	labelSelector := ""
	for key, value := range deployment.Spec.Selector.MatchLabels {
		if labelSelector != "" {
			labelSelector += ","
		}
		labelSelector += key + "=" + value
	}

	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list router pods")
	require.NotEmpty(t, pods.Items, "No router pods found")

	readyPods := make([]corev1.Pod, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if IsPodReady(pod) {
			readyPods = append(readyPods, pod)
		}
	}
	require.NotEmpty(t, readyPods, "No ready router pods found")
	t.Logf("Found %d ready router pods", len(readyPods))

	return readyPods
}

// ListPodsByLabel lists pods matching the given label selector in the namespace.
func ListPodsByLabel(t *testing.T, kubeClient kubernetes.Interface, namespace, labelSelector string) []corev1.Pod {
	t.Helper()
	pods, err := kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	require.NoError(t, err, "Failed to list pods with selector %s", labelSelector)
	return pods.Items
}

// ListReadyPodsByLabel returns pods matching the label selector that pass IsPodReady.
func ListReadyPodsByLabel(t *testing.T, kubeClient kubernetes.Interface, namespace, labelSelector string) []corev1.Pod {
	t.Helper()
	pods := ListPodsByLabel(t, kubeClient, namespace, labelSelector)
	ready := make([]corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if IsPodReady(pod) {
			ready = append(ready, pod)
		}
	}
	return ready
}

// RouterLogsSince returns kthena-router pod logs since the given time.
func RouterLogsSince(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string, since metav1.Time) string {
	t.Helper()
	routerPod := GetRouterPod(t, kubeClient, kthenaNamespace)
	opts := &corev1.PodLogOptions{SinceTime: &since}
	logs, err := kubeClient.CoreV1().Pods(kthenaNamespace).GetLogs(routerPod.Name, opts).Do(context.Background()).Raw()
	require.NoError(t, err)
	return string(logs)
}

// CountSubstringInRouterLogs counts occurrences of substring in kthena-router pod logs since the given time.
func CountSubstringInRouterLogs(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string, since metav1.Time, substring string) int {
	t.Helper()
	return strings.Count(RouterLogsSince(t, kubeClient, kthenaNamespace, since), substring)
}

// CountSelectedPodInRouterLogs counts router access log lines that selected the given pod.
func CountSelectedPodInRouterLogs(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace, podName string, since metav1.Time) int {
	t.Helper()
	return CountSubstringInRouterLogs(t, kubeClient, kthenaNamespace, since, "selected_pod="+podName)
}

// CountSelectedPodsInRouterLogs sums selected_pod counts for the given pods.
func CountSelectedPodsInRouterLogs(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string, since metav1.Time, pods []corev1.Pod) int {
	t.Helper()
	total := 0
	for _, pod := range pods {
		total += CountSelectedPodInRouterLogs(t, kubeClient, kthenaNamespace, pod.Name, since)
	}
	return total
}

// WaitForPodLogsContain polls pod logs until all substrings are present.
// This is useful for verifying async logs like access logs.
func WaitForPodLogsContain(
	t *testing.T,
	kubeClient kubernetes.Interface,
	namespace string,
	podName string,
	since time.Duration,
	substrings []string,
	timeout time.Duration,
	interval time.Duration,
) {
	t.Helper()
	require.NotEmpty(t, substrings, "substrings must not be empty")

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	require.Eventually(t, func() bool {
		sec := int64(since.Seconds())
		logs, err := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			SinceSeconds: &sec,
		}).Do(ctx).Raw()
		if err != nil {
			return false
		}
		s := string(logs)
		for _, sub := range substrings {
			if sub == "" {
				continue
			}
			if !strings.Contains(s, sub) {
				return false
			}
		}
		return true
	}, timeout, interval, "pod logs did not contain expected substrings; pod=%s/%s", namespace, podName)

	for _, sub := range substrings {
		if sub == "" {
			continue
		}
		t.Logf("%s", sub)
	}
}
