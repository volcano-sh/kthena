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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

// EnsureRedis applies the Redis manifest at manifestPath and waits for every Deployment
// in it to become ready, each in the namespace it actually lands in. Namespaced objects
// without a namespace land in the given one. The returned cleanup deletes what this call
// created, newest first; preexisting objects are left in place.
func EnsureRedis(t *testing.T, kubeClient kubernetes.Interface, namespace, manifestPath string) func() {
	t.Helper()
	ctx := context.Background()

	config, err := GetKubeConfig()
	require.NoError(t, err, "Failed to get kubeconfig")

	dynamicClient, err := dynamic.NewForConfig(config)
	require.NoError(t, err, "Failed to create dynamic client")

	redisObjects := LoadUnstructuredYAMLFromFile(manifestPath)
	require.NotEmpty(t, redisObjects, "Redis manifest is empty")

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(kubeClient.Discovery()))
	type createdResourceRef struct {
		gvr       schema.GroupVersionResource
		namespace string
		name      string
	}
	createdRefs := make([]createdResourceRef, 0, len(redisObjects))
	type deploymentRef struct {
		namespace string
		name      string
	}
	deployments := make([]deploymentRef, 0, 1)

	for _, obj := range redisObjects {
		gvk := obj.GroupVersionKind()
		mapping, mapErr := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		require.NoError(t, mapErr, "Failed to map GVK %s", gvk.String())

		resourceClient := dynamicClient.Resource(mapping.Resource)
		namespaceToUse := obj.GetNamespace()
		resource := func() dynamic.ResourceInterface {
			if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
				if namespaceToUse == "" {
					namespaceToUse = namespace
					obj.SetNamespace(namespaceToUse)
				}
				return resourceClient.Namespace(namespaceToUse)
			}
			return resourceClient
		}()

		if obj.GetKind() == "Deployment" {
			// Captured after the namespace resolves so the wait polls where the object
			// actually lands, and before Create so preexisting Deployments still get waited on.
			deployments = append(deployments, deploymentRef{namespace: namespaceToUse, name: obj.GetName()})
		}

		_, createErr := resource.Create(ctx, obj, metav1.CreateOptions{})
		if createErr != nil {
			require.True(t, apierrors.IsAlreadyExists(createErr), "Failed to create %s/%s: %v", gvk.Kind, obj.GetName(), createErr)
			continue
		}

		createdRefs = append(createdRefs, createdResourceRef{
			gvr:       mapping.Resource,
			namespace: namespaceToUse,
			name:      obj.GetName(),
		})
	}

	require.NotEmpty(t, deployments, "Redis Deployment not found in manifest")

	for _, d := range deployments {
		WaitForDeploymentReady(t, ctx, kubeClient, d.namespace, d.name, 1, 2*time.Minute)
	}
	t.Log("Redis is ready")

	return func() {
		cleanupCtx := context.Background()
		for i := len(createdRefs) - 1; i >= 0; i-- {
			ref := createdRefs[i]
			resourceClient := dynamicClient.Resource(ref.gvr)
			if ref.namespace != "" {
				_ = resourceClient.Namespace(ref.namespace).Delete(cleanupCtx, ref.name, metav1.DeleteOptions{})
			} else {
				_ = resourceClient.Delete(cleanupCtx, ref.name, metav1.DeleteOptions{})
			}
		}
	}
}
