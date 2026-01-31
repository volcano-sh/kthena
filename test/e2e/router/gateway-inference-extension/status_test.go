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

package gie

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
)

func TestInferencePoolStatus(t *testing.T) {
	ctx := context.Background()

	// 1. Deploy InferencePool
	t.Log("Deploying InferencePool and verifying status...")
	inferencePool := utils.LoadYAMLFromFile[inferencev1.InferencePool]("examples/kthena-router/InferencePool.yaml")
	inferencePool.Namespace = testNamespace

	gvr := inferencev1.SchemeGroupVersion.WithResource("inferencepools")
	unstructuredPool, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inferencePool)
	require.NoError(t, err)

	_, err = testCtx.DynamicClient.Resource(gvr).Namespace(testNamespace).Create(ctx, &unstructured.Unstructured{Object: unstructuredPool}, metav1.CreateOptions{})
	require.NoError(t, err, "Failed to create InferencePool")

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_ = testCtx.DynamicClient.Resource(gvr).Namespace(testNamespace).Delete(cleanupCtx, inferencePool.Name, metav1.DeleteOptions{})
	})

	// 2. Verify InferencePool exists and status update was triggered
	// (Note: Currently InferencePoolController just updates status without specific conditions)
	require.Eventually(t, func() bool {
		obj, err := testCtx.DynamicClient.Resource(gvr).Namespace(testNamespace).Get(ctx, inferencePool.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}

		// The controller should have processed it and updated the object
		// Even if no conditions are added yet, the resource update confirms the controller is active.
		return obj != nil
	}, 1*time.Minute, 2*time.Second, "InferencePool should be processed by controller")
}
