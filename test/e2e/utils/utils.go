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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultPollingInterval = 2 * time.Second
)

// WaitForModelServingReady waits for a ModelServing to become ready by checking
// if all expected replicas are available.
func WaitForModelServingReady(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, namespace, name string) {
	t.Log("Waiting for ModelServing to be ready...")
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Logf("Error getting ModelServing %s, retrying: %v", name, err)
			return false, nil
		}
		// Check if all replicas are available
		expectedReplicas := int32(1)
		if ms.Spec.Replicas != nil {
			expectedReplicas = *ms.Spec.Replicas
		}
		return ms.Status.AvailableReplicas >= expectedReplicas, nil
	})
	require.NoError(t, err, "ModelServing did not become ready")
}

// IsPodReady checks if a pod is in Running phase and has PodReady condition set to True.
func IsPodReady(pod corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// WaitForDeploymentReady polls until the named Deployment has at least replicas ready pods.
func WaitForDeploymentReady(t *testing.T, ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, replicas int32, timeout time.Duration) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, defaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		return deploy.Status.ReadyReplicas >= replicas, nil
	})
	require.NoError(t, err, "Deployment %q did not become ready after scaling to %d replicas within %v", name, replicas, timeout)
}

// WaitForDeploymentReadyE is like WaitForDeploymentReady but returns an error instead of calling t.Fatal.
// It polls until ReadyReplicas == *Spec.Replicas, or until ReadyReplicas >= 1 when Spec.Replicas is nil.
func WaitForDeploymentReadyE(ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	err := wait.PollUntilContextTimeout(ctx, defaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if deploy.Spec.Replicas == nil {
			return deploy.Status.ReadyReplicas >= 1, nil
		}
		return deploy.Status.ReadyReplicas == *deploy.Spec.Replicas, nil
	})
	if err != nil {
		return fmt.Errorf("deployment %q did not become ready within %v: %w", name, timeout, err)
	}
	return nil
}

// CreateTestNamespace creates a test namespace and tolerates if it already exists.
func CreateTestNamespace(kubeClient kubernetes.Interface, name string) error {
	fmt.Printf("Creating test namespace: %s\n", name)
	ctx := context.Background()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	_, err := kubeClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace %s: %w", name, err)
	}
	return nil
}

// DeleteTestNamespaceAndWait deletes a test namespace and polls until it is fully gone.
func DeleteTestNamespaceAndWait(kubeClient kubernetes.Interface, name string, timeout time.Duration) error {
	fmt.Printf("Deleting test namespace: %s\n", name)
	ctx := context.Background()
	err := kubeClient.CoreV1().Namespaces().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete namespace %s: %w", name, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = wait.PollUntilContextCancel(waitCtx, defaultPollingInterval, true, func(ctx context.Context) (bool, error) {
		_, err := kubeClient.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil // namespace is gone
			}
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("timeout waiting for namespace %s deletion: %w", name, err)
	}
	return nil
}
