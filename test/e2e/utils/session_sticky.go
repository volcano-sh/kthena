/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package utils

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	routerConfigMapName    = "kthena-router-config"
	routerConfigMapDataKey = "routerConfiguration"
	routerDeploymentName   = "kthena-router"

	// SessionStickyModelRouteFile is the shared ModelRoute template for session-sticky E2E.
	SessionStickyModelRouteFile = "ModelRoute-session-sticky.yaml"

	// SessionStickyLogBindingExpired matches MemoryStore background sweep of expired bindings.
	SessionStickyLogBindingExpired = "session sticky: binding expired"
)

var (
	sessionStickyAccessLogTextPodRE = regexp.MustCompile(`selected_pod=([^\s]+)`)
	sessionStickyAccessLogJSONPodRE = regexp.MustCompile(`"selected_pod"\s*:\s*"([^"]*)"`)
)

// SessionStickyE2ERouterYAMLMemory returns router config for in-process session sticky store.
func SessionStickyE2ERouterYAMLMemory() string {
	return `scheduler:
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: random
          weight: 1
sessionSticky:
  backend: memory
`
}

// SessionStickyE2ERouterYAMLRedis returns router config using Redis for session sticky.
func SessionStickyE2ERouterYAMLRedis(redisAddr string) string {
	return fmt.Sprintf(`scheduler:
  plugins:
    Filter:
      enabled: []
    Score:
      enabled:
        - name: random
          weight: 1
sessionSticky:
  backend: redis
  redis:
    address: %s
`, redisAddr)
}

// SessionStickySelectedPodAfterChatURLHeaders sends a chat request to url and reads selected_pod from router logs.
func SessionStickySelectedPodAfterChatURLHeaders(t *testing.T, kube kubernetes.Interface, kthenaNamespace, url, modelName string, messages []ChatMessage, extra map[string]string) string {
	t.Helper()
	requestID := "e2e-ss-" + RandomString(16)
	headers := map[string]string{"x-request-id": requestID}
	for k, v := range extra {
		headers[k] = v
	}
	_ = CheckChatCompletionsWithURLAndHeaders(t, url, modelName, messages, headers)

	ctx := context.Background()
	deadline := time.Now().Add(45 * time.Second)
	tailLines := int64(4000)
	for time.Now().Before(deadline) {
		for _, pod := range GetReadyRouterPods(t, kube, kthenaNamespace) {
			tl := tailLines
			raw, err := kube.CoreV1().Pods(kthenaNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{TailLines: &tl}).Do(ctx).Raw()
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if podName := sessionStickyParseSelectedPodFromAccessLogLine(line, requestID); podName != "" {
					return podName
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	require.Failf(t, "selected_pod not found in router access logs", "request_id=%s", requestID)
	return ""
}

func sessionStickyParseSelectedPodFromAccessLogLine(line, requestID string) string {
	if !strings.Contains(line, "request_id="+requestID) &&
		!strings.Contains(line, `"request_id":"`+requestID+`"`) {
		return ""
	}
	if m := sessionStickyAccessLogTextPodRE.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	if m := sessionStickyAccessLogJSONPodRE.FindStringSubmatch(line); len(m) > 1 {
		return m[1]
	}
	return ""
}

// SessionStickyWaitBindingExpiredLog waits for router pod logs after a binding TTL eviction.
func SessionStickyWaitBindingExpiredLog(t *testing.T, kube kubernetes.Interface, kthenaNamespace string) {
	t.Helper()
	routerPod := GetRouterPod(t, kube, kthenaNamespace)
	WaitForPodLogsContain(t, kube, kthenaNamespace, routerPod.Name, 2*time.Minute,
		[]string{SessionStickyLogBindingExpired}, 90*time.Second, 2*time.Second)
}

// SessionStickyUpdateRouterConfigMapData writes routerConfiguration with optimistic-lock retries.
func SessionStickyUpdateRouterConfigMapData(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace, yamlBody string) {
	t.Helper()
	ctx := context.Background()
	const maxAttempts = 15
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cm, err := kubeClient.CoreV1().ConfigMaps(kthenaNamespace).Get(ctx, routerConfigMapName, metav1.GetOptions{})
		require.NoError(t, err)
		cm = cm.DeepCopy()
		cm.Data[routerConfigMapDataKey] = yamlBody
		_, err = kubeClient.CoreV1().ConfigMaps(kthenaNamespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err == nil {
			return
		}
		if apierrors.IsConflict(err) && attempt+1 < maxAttempts {
			time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
			continue
		}
		require.NoError(t, err)
	}
}

// SessionStickyRolloutRestartRouterAndWait restarts the router Deployment to reload mounted ConfigMap.
func SessionStickyRolloutRestartRouterAndWait(
	t *testing.T,
	kubeClient kubernetes.Interface,
	kthenaClient *clientset.Clientset,
	kthenaNamespace, testNamespace, webhookProbeModelServer string,
	scalingTimeout time.Duration,
) {
	t.Helper()
	if scalingTimeout <= 0 {
		scalingTimeout = 3 * time.Minute
	}
	ctx := context.Background()
	dep, err := kubeClient.AppsV1().Deployments(kthenaNamespace).Get(ctx, routerDeploymentName, metav1.GetOptions{})
	require.NoError(t, err)
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":"%s"}}}}}`,
		time.Now().UTC().Format(time.RFC3339Nano),
	))
	_, err = kubeClient.AppsV1().Deployments(kthenaNamespace).Patch(
		ctx, routerDeploymentName, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
	require.NoError(t, err)
	WaitForDeploymentReady(t, ctx, kubeClient, kthenaNamespace, routerDeploymentName, want, scalingTimeout)
	require.Eventually(t, func() bool {
		d, err := kubeClient.AppsV1().Deployments(kthenaNamespace).Get(ctx, routerDeploymentName, metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil {
			return false
		}
		wantReplicas := *d.Spec.Replicas
		if d.Status.ObservedGeneration < d.Generation {
			return false
		}
		s := d.Status
		return s.Replicas == wantReplicas && s.ReadyReplicas == wantReplicas && s.UpdatedReplicas == wantReplicas
	}, 5*time.Minute, 750*time.Millisecond, "kthena-router deployment not settled (replicas/ready/updated vs spec)")
	require.NoError(t, WaitForKthenaRouterModelRouteValidatingWebhook(ctx, kthenaClient, testNamespace, webhookProbeModelServer, "ss-webhook-probe-", 3*time.Second, 4*time.Minute, t.Logf),
		"kthena-router ModelRoute validating webhook did not become ready after router rollout")
}

// SessionStickyRegisterModelRouteCleanup deletes the ModelRoute when the test finishes.
func SessionStickyRegisterModelRouteCleanup(t *testing.T, kthenaClient *clientset.Clientset, testNamespace string, mr *networkingv1alpha1.ModelRoute) {
	t.Helper()
	if mr == nil {
		return
	}
	t.Cleanup(func() {
		_ = kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Delete(
			context.Background(), mr.Name, metav1.DeleteOptions{})
	})
}

// SessionStickyCreateModelRoute loads the template, applies customize and Gateway ParentRefs, then creates the route.
func SessionStickyCreateModelRoute(
	t *testing.T,
	ctx context.Context,
	kthenaClient *clientset.Clientset,
	testdataDir, testNamespace, kthenaNamespace string,
	useGatewayAPI bool,
	customize func(*networkingv1alpha1.ModelRoute),
) *networkingv1alpha1.ModelRoute {
	t.Helper()
	mr := LoadYAMLFromFile[networkingv1alpha1.ModelRoute](filepath.Join(testdataDir, SessionStickyModelRouteFile))
	mr.Namespace = testNamespace
	if customize != nil {
		customize(mr)
	}
	if useGatewayAPI {
		ktNamespace := gatewayv1.Namespace(kthenaNamespace)
		if len(mr.Spec.ParentRefs) > 0 {
			for i := range mr.Spec.ParentRefs {
				mr.Spec.ParentRefs[i].Namespace = &ktNamespace
			}
		} else {
			kind := gatewayv1.Kind("Gateway")
			mr.Spec.ParentRefs = []gatewayv1.ParentReference{
				{
					Name:      "default",
					Namespace: &ktNamespace,
					Kind:      &kind,
				},
			}
		}
	}
	out, err := kthenaClient.NetworkingV1alpha1().ModelRoutes(testNamespace).Create(ctx, mr, metav1.CreateOptions{})
	require.NoError(t, err)
	return out
}

// SessionStickyRouterConn holds a test-local port-forward to kthena-router (avoids global :8080).
type SessionStickyRouterConn struct {
	URL string
	pf  PortForwarder
}

// SessionStickyConnectRouter port-forwards svc/kthena-router on a free local port.
func SessionStickyConnectRouter(t *testing.T, kthenaNamespace string) *SessionStickyRouterConn {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := fmt.Sprintf("%d", listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())

	pf, err := SetupPortForward(kthenaNamespace, routerDeploymentName, port, "80")
	require.NoError(t, err)
	return &SessionStickyRouterConn{
		URL: fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", port),
		pf:  pf,
	}
}

// Close stops the port-forward.
func (c *SessionStickyRouterConn) Close() {
	if c != nil && c.pf != nil {
		c.pf.Close()
	}
}

// SessionStickyPreRolloutRouterPodNames records current ready router pod names before a rollout.
// Call before SessionStickyPatchRouterConfigAndRollout; pass the result to SessionStickyWaitRouterReplicasAfterRollout.
func SessionStickyPreRolloutRouterPodNames(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string) map[string]bool {
	t.Helper()
	preRolloutPods := GetReadyRouterPods(t, kubeClient, kthenaNamespace)
	names := make(map[string]bool, len(preRolloutPods))
	for _, pod := range preRolloutPods {
		names[pod.Name] = true
	}
	return names
}

// SessionStickyWaitRouterReplicasAfterRollout waits until pre-rollout pods are replaced, the deployment
// has expectedReplicas ready, and at least minPods stable (non-terminating, ready) router pods exist.
// Matches the post-restart stabilization used in TestRouterConfigUpdateShared before port-forward.
func SessionStickyWaitRouterReplicasAfterRollout(
	t *testing.T,
	kubeClient kubernetes.Interface,
	kthenaNamespace string,
	preRolloutPodNames map[string]bool,
	expectedReplicas int32,
	minPods int,
	timeout time.Duration,
) []corev1.Pod {
	t.Helper()
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx := context.Background()
	selector := sessionStickyRouterPodLabelSelector(t, kubeClient, kthenaNamespace)

	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return false
		}
		for _, pod := range pods.Items {
			if preRolloutPodNames[pod.Name] {
				return false
			}
		}
		return len(pods.Items) > 0
	}, timeout, 2*time.Second, "Pre-rollout router pods should be replaced after rollout")

	WaitForDeploymentReady(t, ctx, kubeClient, kthenaNamespace, routerDeploymentName, expectedReplicas, timeout)
	t.Logf("kthena-router deployment is ready with %d replicas after rollout", expectedReplicas)

	var stablePods []corev1.Pod
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			return false
		}
		stablePods = stablePods[:0]
		for _, pod := range pods.Items {
			if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
				continue
			}
			if IsPodReady(pod) {
				stablePods = append(stablePods, pod)
			}
		}
		return len(stablePods) >= minPods
	}, timeout, 2*time.Second, "need at least %d stable router pods after rollout", minPods)

	return stablePods
}

func sessionStickyRouterPodLabelSelector(t *testing.T, kubeClient kubernetes.Interface, kthenaNamespace string) string {
	t.Helper()
	ctx := context.Background()
	dep, err := kubeClient.AppsV1().Deployments(kthenaNamespace).Get(ctx, routerDeploymentName, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get kthena-router deployment")
	return metav1.FormatLabelSelector(dep.Spec.Selector)
}

// SessionStickyPatchRouterConfigAndRollout updates router config, rolls out, then runs afterRollout (e.g. reconnect port-forward).
func SessionStickyPatchRouterConfigAndRollout(
	t *testing.T,
	kubeClient kubernetes.Interface,
	kthenaClient *clientset.Clientset,
	kthenaNamespace, testNamespace, yamlBody, webhookProbeModelServer string,
	scalingTimeout time.Duration,
	afterRollout func(),
) {
	t.Helper()
	SessionStickyUpdateRouterConfigMapData(t, kubeClient, kthenaNamespace, yamlBody)
	SessionStickyRolloutRestartRouterAndWait(t, kubeClient, kthenaClient, kthenaNamespace, testNamespace, webhookProbeModelServer, scalingTimeout)
	if afterRollout != nil {
		afterRollout()
	}
}
