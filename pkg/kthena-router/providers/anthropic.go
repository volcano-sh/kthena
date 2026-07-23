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

package providers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
)

type anthropicAdapter struct{}

func (anthropicAdapter) BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error) {
	if req.URL.Path != "/v1/messages" {
		return nil, &UnsupportedPathError{ProviderType: provider.Spec.ProviderType, Path: req.URL.Path}
	}
	rewriteBody := provider.Spec.Model != nil && *provider.Spec.Model != ""
	upstream, err := buildProviderRequest(c, req, provider, secret, modelRequest, rewriteBody)
	if err != nil {
		return nil, err
	}
	token, err := providerToken(provider, secret)
	if err != nil {
		return nil, err
	}
	if token != "" {
		upstream.Header.Set("x-api-key", token)
	}
	return upstream, nil
}
