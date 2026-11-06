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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// acceptContentTypes prefers protobuf responses from kube-apiserver and falls
// back to JSON for resources that cannot be served as protobuf (e.g. CRDs).
const acceptContentTypes = runtime.ContentTypeProtobuf + "," + runtime.ContentTypeJSON

// BuildConfig returns a rest config with kubectl's loading semantics.
// When neither masterURL nor kubeconfigPath is given it prefers the
// in-cluster config, then falls back to the kubeconfig chain: the
// KUBECONFIG path list first, then ~/.kube/config. An explicit
// kubeconfigPath is used as the only kubeconfig file, and masterURL
// overrides the server address from whichever config was loaded.
// The returned config accepts protobuf responses from kube-apiserver,
// falling back to JSON, while request bodies stay JSON because CRD types
// do not implement protobuf marshalling. This keeps the config safe to
// share across built-in and CRD clientsets.
func BuildConfig(masterURL, kubeconfigPath string) (*rest.Config, error) {
	if masterURL == "" && kubeconfigPath == "" {
		if config, err := rest.InClusterConfig(); err == nil {
			return setContentTypes(config), nil
		}
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = kubeconfigPath
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: masterURL}},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	return setContentTypes(config), nil
}

func setContentTypes(config *rest.Config) *rest.Config {
	config.ContentType = runtime.ContentTypeJSON
	config.AcceptContentTypes = acceptContentTypes
	return config
}
