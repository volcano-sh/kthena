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

package kube

import (
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// BuildConfig returns a rest config with kubectl's loading semantics.
// When neither masterURL nor kubeconfigPath is given it prefers the
// in-cluster config, then falls back to the kubeconfig chain: the
// KUBECONFIG path list first, then ~/.kube/config. An explicit
// kubeconfigPath is used as the only kubeconfig file, and masterURL
// overrides the server address from whichever config was loaded.
func BuildConfig(masterURL, kubeconfigPath string) (*rest.Config, error) {
	if masterURL == "" && kubeconfigPath == "" {
		if config, err := rest.InClusterConfig(); err == nil {
			return config, nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigPath
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}},
	).ClientConfig()
}
