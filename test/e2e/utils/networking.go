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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CreateModelServerFromFile loads a ModelServer YAML from testDataDir and creates it in namespace.
func CreateModelServerFromFile(t *testing.T, ctx context.Context, kthena *clientset.Clientset, testDataDir, namespace, file string) *networkingv1alpha1.ModelServer {
	t.Helper()
	ms := LoadYAMLFromFile[networkingv1alpha1.ModelServer](filepath.Join(testDataDir, file))
	ms.Namespace = namespace
	created, err := kthena.NetworkingV1alpha1().ModelServers(namespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kthena.NetworkingV1alpha1().ModelServers(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	})
	return created
}

// CreateModelRouteFromFile loads a ModelRoute YAML from testDataDir and creates it in namespace.
func CreateModelRouteFromFile(t *testing.T, ctx context.Context, kthena *clientset.Clientset, testDataDir, namespace, file string) *networkingv1alpha1.ModelRoute {
	t.Helper()
	modelRoute := LoadYAMLFromFile[networkingv1alpha1.ModelRoute](filepath.Join(testDataDir, file))
	modelRoute.Namespace = namespace
	created, err := kthena.NetworkingV1alpha1().ModelRoutes(namespace).Create(ctx, modelRoute, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = kthena.NetworkingV1alpha1().ModelRoutes(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	})
	return created
}
