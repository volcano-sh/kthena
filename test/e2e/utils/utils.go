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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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
