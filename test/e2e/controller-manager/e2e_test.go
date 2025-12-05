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

package controller_manager

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	// TODO: separate kthena system components and e2e test namespace
	testNamespace = "dev"
	routerSvc     = "kthena-router"
)

// TestModelCR creates a ModelBooster CR, waits for it to become active, and tests chat functionality.
func TestModelCR(t *testing.T) {
	ctx := context.Background()
	// Initialize Kubernetes clients
	config, err := getKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")
	kubeClient, err := kubernetes.NewForConfig(config)
	require.NoError(t, err, "Failed to create Kubernetes client")
	kthenaClient, err := clientset.NewForConfig(config)
	require.NoError(t, err, "Failed to create kthena client")
	// Create a Model CR in the test namespace
	model := createTestModel()
	createdModel, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Create(ctx, model, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create Model CR")
	assert.NotNil(t, createdModel)
	t.Logf("Created Model CR: %s/%s", createdModel.Namespace, createdModel.Name)
	// Wait for the Model to be Active
	require.Eventually(t, func() bool {
		model, err := kthenaClient.WorkloadV1alpha1().ModelBoosters(testNamespace).Get(ctx, model.Name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get model error: %v", err)
			return false
		}
		return true == meta.IsStatusConditionPresentAndEqual(model.Status.Conditions,
			string(workload.ModelStatusConditionTypeActive), metav1.ConditionTrue)
	}, 5*time.Minute, 5*time.Second, "Model did not become Active")
	// Test chat in cluster
	executeChatInCluster(t, kubeClient, ctx, config)
	// todo: test update modelBooster, delete modelBooster
}

func executeChatInCluster(t *testing.T, kubeClient *kubernetes.Clientset, ctx context.Context, config *rest.Config) {
	// start a nginx pod and then exec curl command in the pod
	podName, err := runNginxPod(t, kubeClient, ctx)
	require.NoError(t, err, "Failed to run nginx pod")
	var stdout, stderr bytes.Buffer
	req := kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(testNamespace).
		SubResource("exec")
	command := []string{
		"curl",
		"-s",
		"-X", "POST",
		"-H", "Content-Type: application/json",
		"-d", `{"model": "test-model", "messages": [{"role": "user", "content": "Where is the capital of China?"}], "stream": false}`,
		fmt.Sprintf("http://%s/v1/chat/completions", routerSvc),
	}
	option := &corev1.PodExecOptions{
		Command: command,
		Stdin:   false,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}
	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)
	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	require.NoError(t, err, "Failed to create executor")
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	require.NoError(t, err, "Failed to execute command in pod")
	// Log stderr for debugging
	if stderr.String() != "" {
		t.Logf("Command stderr: %s", stderr.String())
	}
	// Check if response contains expected result
	responseStr := stdout.String()
	t.Logf("Chat response: %s", responseStr)
	// Verify response is successful
	assert.NotEmpty(t, responseStr, "Chat response is empty")
	assert.NotContains(t, responseStr, "error", "Chat response contains error")
}

func runNginxPod(t *testing.T, kubeClient *kubernetes.Clientset, ctx context.Context) (string, error) {
	podName := "test-nginx-pod"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: testNamespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "nginx",
					Image:   "nginx:alpine",
					Command: []string{"sleep", "3600"},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("100Mi"),
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
	_, err := kubeClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create nginx pod")
	t.Logf("Created nginx pod: %s/%s", testNamespace, podName)
	require.Eventually(t, func() bool {
		pod, err := kubeClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			t.Logf("Get pod error: %v", err)
			return false
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}, 5*time.Minute, 5*time.Second, "Pod did not become ready")
	return podName, err
}

func getKubeConfig() (*rest.Config, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// Fall back to kubeconfig
	return clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
}

func createTestModel() *workload.ModelBooster {
	// Create a simple config as JSON
	config := &apiextensionsv1.JSON{}
	configRaw := `{
		"served-model-name": "test-model",
		"max-model-len": 32768,
		"max-num-batched-tokens": 65536,
		"block-size": 128,
		"enable-prefix-caching": ""
	}`
	config.Raw = []byte(configRaw)

	return &workload.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: testNamespace,
		},
		Spec: workload.ModelBoosterSpec{
			Name: "test-model",
			Backend: workload.ModelBackend{
				Name:        "backend1",
				Type:        workload.ModelBackendTypeVLLM,
				ModelURI:    "hf://Qwen/Qwen2.5-0.5B-Instruct",
				CacheURI:    "hostpath:///tmp/cache",
				MinReplicas: 1,
				MaxReplicas: 1,
				Workers: []workload.ModelWorker{
					{
						Type:     workload.ModelWorkerTypeServer,
						Image:    "ghcr.io/huntersman/vllm-cpu-env:latest",
						Replicas: 1,
						Pods:     1,
						Config:   *config,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("4"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					},
				},
			},
		},
	}
}
