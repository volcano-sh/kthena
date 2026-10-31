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

package tokenizer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	tokenizercontext "github.com/volcano-sh/kthena/test/e2e/router/tokenizer/context"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestTokenizerModelLoadLog verifies that after TestMain's SetupCommonComponents creates
// the ModelServer, the tokenizer logs "Tokenizer for model tokenizer-test-model ready at".
func TestTokenizerModelLoadLog(t *testing.T) {
	pod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)
	utils.WaitForTokenizerModelReady(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)
}

// TestTokenizerModelUnloadLog deletes the common ModelServer, waits for the unload log,
// then recreates it so subsequent tests are unaffected.
func TestTokenizerModelUnloadLog(t *testing.T) {
	pod := utils.GetTokenizerPod(t, testCtx.KubeClient, kthenaNamespace)

	// Ensure the renderer is ready before we delete the ModelServer.
	utils.WaitForTokenizerModelReady(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		tokenizercontext.ModelServerName, tokenizercontext.TokenizerReadyTimeout)

	ctx := context.Background()
	err := testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testCtx.Namespace).Delete(
		ctx, tokenizercontext.ModelServerName, metav1.DeleteOptions{})
	require.NoError(t, err, "delete common ModelServer")

	t.Cleanup(func() {
		// Recreate the ModelServer so health/tokenize tests that run after this work.
		ms := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](
			filepath.Join(tokenizercontext.TestDataDir, "ModelServer-tokenizer-test.yaml"))
		ms.Namespace = testCtx.Namespace
		_, _ = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testCtx.Namespace).Create(
			context.Background(), ms, metav1.CreateOptions{})
	})

	utils.WaitForPodLogsContain(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		tokenizercontext.TokenizerReadyTimeout,
		[]string{fmt.Sprintf("Unloading tokenizer for model %s", tokenizercontext.ModelServerName)},
		tokenizercontext.TokenizerReadyTimeout, 2*time.Second,
	)
}

// TestTokenizerFailedStartupNoPanic is a regression test for issue #1885.
// When the renderer command fails to start (cmd.Start() returns an error), r.cancel is
// never set to a non-nil value. Deleting the ModelServer while the renderer is in that
// state must NOT panic — the nil guard at manager.go:131 protects r.cancel.
func TestTokenizerFailedStartupNoPanic(t *testing.T) {
	// Patch VLLM_RENDER_COMMAND to a deliberately missing binary so cmd.Start() fails.
	const badCmd = "/nonexistent-binary-e2e-test"
	require.NoError(t, patchTokenizerRenderCommand(t, kthenaNamespace, badCmd))

	// Cleanups run LIFO: restore good command first, then recreate the common ModelServer.
	t.Cleanup(func() {
		ms := utils.LoadYAMLFromFile[networkingv1alpha1.ModelServer](
			filepath.Join(tokenizercontext.TestDataDir, "ModelServer-tokenizer-test.yaml"))
		ms.Namespace = testCtx.Namespace
		_, _ = testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testCtx.Namespace).Create(
			context.Background(), ms, metav1.CreateOptions{})
	})
	t.Cleanup(func() {
		_ = patchTokenizerRenderCommand(t, kthenaNamespace, "/usr/local/bin/kthena-tokenizer-stub")
	})

	// Poll until a ready pod with the patched env var appears.
	// GetTokenizerPod may transiently return a pod from the previous generation
	// if the API server hasn't yet GC'd it — retry until we see the new one.
	var pod *corev1.Pod
	require.Eventually(t, func() bool {
		ctx2 := context.Background()
		pods, err := testCtx.KubeClient.CoreV1().Pods(kthenaNamespace).List(ctx2, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/component=kthena-tokenizer",
		})
		if err != nil || len(pods.Items) == 0 {
			return false
		}
		for i := range pods.Items {
			if !utils.IsPodReady(pods.Items[i]) {
				continue
			}
			for _, c := range pods.Items[i].Spec.Containers {
				if c.Name != "tokenizer" && c.Name != "kthena-tokenizer" {
					continue
				}
				for _, e := range c.Env {
					if e.Name == "VLLM_RENDER_COMMAND" && e.Value == badCmd {
						pod = &pods.Items[i]
						return true
					}
				}
			}
		}
		return false
	}, 2*time.Minute, 2*time.Second,
		"a ready pod with VLLM_RENDER_COMMAND=%q must appear after rollout", badCmd)

	// The watcher syncs the common ModelServer ("tokenizer-test-model") to the new pod.
	// The manager attempts to launch the renderer with the bad binary — cmd.Start() fails
	// immediately, leaving r.cancel == nil.
	utils.WaitForPodLogsContain(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		2*time.Minute,
		[]string{fmt.Sprintf("Renderer for model %s failed:", tokenizercontext.ModelServerName)},
		2*time.Minute, 2*time.Second,
	)

	// Record the container restart count now; a panic would have incremented it.
	var restartsBefore int32
	if p, err := testCtx.KubeClient.CoreV1().Pods(kthenaNamespace).Get(
		context.Background(), pod.Name, metav1.GetOptions{}); err == nil {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "tokenizer" || cs.Name == "kthena-tokenizer" {
				restartsBefore = cs.RestartCount
				break
			}
		}
	}

	// Delete the common ModelServer while the renderer is in StatusFailed (r.cancel == nil).
	// Without the nil guard at manager.go:131, calling r.cancel() would panic.
	require.NoError(t, testCtx.KthenaClient.NetworkingV1alpha1().ModelServers(testCtx.Namespace).Delete(
		context.Background(), tokenizercontext.ModelServerName, metav1.DeleteOptions{}))

	// Confirm the manager handled the deletion cleanly — unload log must appear.
	utils.WaitForPodLogsContain(t, testCtx.KubeClient, kthenaNamespace, pod.Name,
		2*time.Minute,
		[]string{fmt.Sprintf("Unloading tokenizer for model %s", tokenizercontext.ModelServerName)},
		2*time.Minute, 2*time.Second,
	)

	// Pod must still be running and container restart count must be unchanged;
	// a panic would have crashed the container and incremented the restart count.
	require.Eventually(t, func() bool {
		p, err := testCtx.KubeClient.CoreV1().Pods(kthenaNamespace).Get(
			context.Background(), pod.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "tokenizer" || cs.Name == "kthena-tokenizer" {
				return cs.RestartCount == restartsBefore && cs.State.Running != nil
			}
		}
		return false
	}, 30*time.Second, 2*time.Second,
		"tokenizer container must remain running with no new restarts after nil-cancel delete (regression #1885)")
}

// patchTokenizerRenderCommand updates VLLM_RENDER_COMMAND on the kthena-tokenizer Deployment.
func patchTokenizerRenderCommand(t *testing.T, namespace, cmd string) error {
	t.Helper()
	ctx := context.Background()
	deploy, err := testCtx.KubeClient.AppsV1().Deployments(namespace).Get(ctx, utils.TokenizerDeploymentName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	found := false
	for i, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "tokenizer" || c.Name == "kthena-tokenizer" {
			found = true
			updated := false
			for j, e := range c.Env {
				if e.Name == "VLLM_RENDER_COMMAND" {
					deploy.Spec.Template.Spec.Containers[i].Env[j].Value = cmd
					updated = true
					break
				}
			}
			if !updated {
				deploy.Spec.Template.Spec.Containers[i].Env = append(
					deploy.Spec.Template.Spec.Containers[i].Env,
					corev1.EnvVar{Name: "VLLM_RENDER_COMMAND", Value: cmd},
				)
			}
			break
		}
	}
	if !found {
		return fmt.Errorf("no container named 'tokenizer' or 'kthena-tokenizer' found in Deployment %s/%s",
			namespace, utils.TokenizerDeploymentName)
	}
	_, err = testCtx.KubeClient.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return utils.RolloutRestartDeployment(ctx, testCtx.KubeClient, namespace, utils.TokenizerDeploymentName, 3*time.Minute)
}
