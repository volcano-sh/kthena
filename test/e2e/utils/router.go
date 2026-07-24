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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	RouterConfigMapName  = "kthena-router-config"
	RouterConfigKey      = "routerConfiguration"
	RouterDeploymentName = "kthena-router"
	RouterRolloutTimeout = 3 * time.Minute
	RouterDebugPort      = "15000"
)

// SetupRouterPortForwardAfterRestart uses a dynamic local port so rollout restart does not
// break the framework TestMain port-forward on :8080.
func SetupRouterPortForwardAfterRestart(t *testing.T, kthenaNamespace string) (chatURL, metricsURL string, closePF func()) {
	t.Helper()
	localPort := AllocateLocalPort(t)
	pf, err := SetupPortForward(kthenaNamespace, RouterDeploymentName, localPort, "80")
	require.NoError(t, err, "failed to setup port-forward to restarted router")
	chatURL = fmt.Sprintf("http://127.0.0.1:%s/v1/chat/completions", localPort)
	metricsURL = fmt.Sprintf("http://127.0.0.1:%s/metrics", localPort)
	return chatURL, metricsURL, func() { pf.Close() }
}

// ApplySchedulerConfig patches router scheduler config, restarts the router, and returns URLs plus a restore func.
func ApplySchedulerConfig(
	t *testing.T,
	kube kubernetes.Interface,
	kthena *clientset.Clientset,
	kthenaNamespace, testNamespace, schedulerYAML, probeModelServerName, probeModelName string,
) (chatURL, metricsURL string, restore func()) {
	t.Helper()
	ctx := context.Background()
	cm, err := kube.CoreV1().ConfigMaps(kthenaNamespace).Get(ctx, RouterConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	original := cm.Data[RouterConfigKey]

	cm.Data[RouterConfigKey] = schedulerYAML
	_, err = kube.CoreV1().ConfigMaps(kthenaNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, RolloutRestartDeployment(ctx, kube, kthenaNamespace, RouterDeploymentName, RouterRolloutTimeout))
	WaitForRouterValidatingWebhook(t, ctx, kthena, testNamespace, probeModelServerName, probeModelName)
	chatURL, metricsURL, closePF := SetupRouterPortForwardAfterRestart(t, kthenaNamespace)

	restore = func() {
		closePF()
		cleanupCtx := context.Background()
		latest, err := kube.CoreV1().ConfigMaps(kthenaNamespace).Get(cleanupCtx, RouterConfigMapName, metav1.GetOptions{})
		if err != nil {
			return
		}
		latest.Data[RouterConfigKey] = original
		// Restore etcd config only; do not rollout here. The next plugin test's
		// ApplySchedulerConfig performs a single rollout when it patches the CM.
		// Rolling out on cleanup caused back-to-back restarts and webhook dial timeouts.
		_, _ = kube.CoreV1().ConfigMaps(kthenaNamespace).Update(cleanupCtx, latest, metav1.UpdateOptions{})
	}
	return chatURL, metricsURL, restore
}
