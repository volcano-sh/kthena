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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// RolloutRestartDeployment triggers a rolling restart by patching restartedAt on the pod template.
// It waits until all replicas of the new generation are updated, ready, and available.
func RolloutRestartDeployment(ctx context.Context, kubeClient kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s/%s: %w", namespace, name, err)
	}
	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	updated, err := kubeClient.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update deployment %s/%s: %w", namespace, name, err)
	}

	targetGeneration := updated.Generation
	err = wait.PollUntilContextTimeout(ctx, defaultPollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		d, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if d.Status.ObservedGeneration < targetGeneration {
			return false, nil
		}
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		return d.Status.UpdatedReplicas == desired &&
			d.Status.ReadyReplicas == desired &&
			d.Status.AvailableReplicas == desired &&
			d.Status.Replicas == desired, nil
	})
	if err != nil {
		return fmt.Errorf("rollout of deployment %s/%s did not complete within %v: %w", namespace, name, timeout, err)
	}
	return nil
}

// ScaleDeploymentReplicas scales a deployment and returns a cleanup that restores the original replica count.
func ScaleDeploymentReplicas(t *testing.T, kubeClient kubernetes.Interface, namespace, name string, replicas int32) func() {
	t.Helper()
	ctx := context.Background()

	deploy, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)

	original := int32(1)
	if deploy.Spec.Replicas != nil {
		original = *deploy.Spec.Replicas
	}

	deploy.Spec.Replicas = &replicas
	_, err = kubeClient.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
	require.NoError(t, err)
	WaitForDeploymentReady(t, ctx, kubeClient, namespace, name, replicas, 5*time.Minute)

	return func() {
		latest, err := kubeClient.AppsV1().Deployments(namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return
		}
		latest.Spec.Replicas = &original
		if _, err := kubeClient.AppsV1().Deployments(namespace).Update(context.Background(), latest, metav1.UpdateOptions{}); err != nil {
			return
		}
		_ = WaitForDeploymentReadyE(context.Background(), kubeClient, namespace, name, 5*time.Minute)
	}
}

// DeleteDeploymentAndWait deletes a deployment and polls until it is gone.
func DeleteDeploymentAndWait(t *testing.T, kubeClient kubernetes.Interface, namespace, name string, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	_ = kubeClient.AppsV1().Deployments(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	require.Eventually(t, func() bool {
		_, err := kubeClient.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, timeout, defaultPollingInterval, "deployment %s/%s should be deleted", namespace, name)
}
