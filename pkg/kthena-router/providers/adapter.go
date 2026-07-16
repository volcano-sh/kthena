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
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type Adapter interface {
	BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error)
}

type UnsupportedPathError struct {
	ProviderType networkingv1alpha1.ExternalProviderType
	Path         string
}

func (e *UnsupportedPathError) Error() string {
	return fmt.Sprintf("provider type %q does not support path %q", e.ProviderType, e.Path)
}

type openAIAdapter struct{}
type anthropicAdapter struct{}

func NewAdapter(providerType networkingv1alpha1.ExternalProviderType) (Adapter, error) {
	switch providerType {
	case "", networkingv1alpha1.OpenAI:
		return openAIAdapter{}, nil
	case networkingv1alpha1.Anthropic:
		return anthropicAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported provider type %q", providerType)
	}
}

func (openAIAdapter) BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error) {
	if !isOpenAIPath(req.URL.Path) {
		return nil, &UnsupportedPathError{ProviderType: provider.Spec.ProviderType, Path: req.URL.Path}
	}
	rewriteBody := provider.Spec.Model != nil && *provider.Spec.Model != ""
	if req.URL.Path != "/v1/responses" {
		if addOpenAIStreamingTokenUsage(c, modelRequest) {
			rewriteBody = true
		}
	}
	upstream, err := buildProviderRequest(c, req, provider, secret, modelRequest, rewriteBody)
	if err != nil {
		return nil, err
	}
	token, err := providerToken(provider, secret)
	if err != nil {
		return nil, err
	}
	if token != "" {
		upstream.Header.Set("Authorization", "Bearer "+token)
	}
	return upstream, nil
}

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

func buildProviderRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}, rewriteBody bool) (*http.Request, error) {
	if provider.Spec.Model != nil && *provider.Spec.Model != "" {
		modelRequest["model"] = *provider.Spec.Model
		rewriteBody = true
	}

	var body []byte
	if !rewriteBody {
		if raw, exists := c.Get(common.RawRequestBodyKey); exists {
			if rawBody, ok := raw.([]byte); ok {
				body = rawBody
			}
		}
	}
	if body == nil {
		var err error
		body, err = json.Marshal(modelRequest)
		if err != nil {
			return nil, err
		}
	}

	upstreamURL, err := buildProviderURL(provider.Spec.BaseURL, req.URL.Path, req.URL.RawQuery, provider.Spec.ProviderType)
	if err != nil {
		return nil, err
	}

	reqCopy := req.Clone(req.Context())
	reqCopy.URL = upstreamURL
	reqCopy.Host = upstreamURL.Host
	reqCopy.RequestURI = ""
	reqCopy.Header = sanitizeRequestHeaders(req.Header)
	if err := applyStaticHeaders(reqCopy.Header, provider.Spec.Headers); err != nil {
		return nil, err
	}
	if reqCopy.Header.Get("Content-Type") == "" {
		reqCopy.Header.Set("Content-Type", "application/json")
	}
	reqCopy.Body = io.NopCloser(bytes.NewReader(body))
	reqCopy.ContentLength = int64(len(body))
	return reqCopy, nil
}

func buildProviderURL(baseURL, requestPath, rawQuery string, providerType networkingv1alpha1.ExternalProviderType) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	pathSuffix := strings.TrimLeft(requestPath, "/")
	baseIncludesAPIVersion := providerType == networkingv1alpha1.Anthropic && strings.HasSuffix(basePath, "/v1")
	if basePath != "" && (providerType == "" || providerType == networkingv1alpha1.OpenAI || baseIncludesAPIVersion) {
		pathSuffix = strings.TrimPrefix(pathSuffix, "v1/")
		if pathSuffix == "v1" {
			pathSuffix = ""
		}
	}
	if pathSuffix == "" {
		parsed.Path = basePath
	} else if basePath == "" {
		parsed.Path = "/" + pathSuffix
	} else {
		parsed.Path = basePath + "/" + pathSuffix
	}
	parsed.RawQuery = rawQuery
	return parsed, nil
}

func addOpenAIStreamingTokenUsage(c *gin.Context, modelRequest map[string]interface{}) bool {
	streaming, _ := modelRequest["stream"].(bool)
	if !streaming {
		return false
	}

	streamOptions, ok := modelRequest["stream_options"].(map[string]interface{})
	if !ok {
		streamOptions = map[string]interface{}{}
		modelRequest["stream_options"] = streamOptions
	}
	if includeUsage, _ := streamOptions["include_usage"].(bool); includeUsage {
		return false
	}

	streamOptions["include_usage"] = true
	c.Set(common.TokenUsageKey, true)
	return true
}

func isOpenAIPath(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/responses"
}

func providerToken(provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret) (string, error) {
	if provider.Spec.Auth == nil {
		return "", nil
	}
	if secret == nil {
		return "", fmt.Errorf("secret %s is not loaded", provider.Spec.Auth.SecretRef.Name)
	}
	key := provider.Spec.Auth.SecretRef.Key
	value, ok := secret.Data[key]
	if !ok || len(value) == 0 {
		return "", fmt.Errorf("secret key %s is not found", key)
	}
	return string(value), nil
}

func sanitizeRequestHeaders(headers http.Header) http.Header {
	clean := http.Header{}
	for key, values := range headers {
		if !isAllowedForwardHeader(key) {
			continue
		}
		for _, value := range values {
			clean.Add(key, value)
		}
	}
	return clean
}

func applyStaticHeaders(headers http.Header, staticHeaders map[string]string) error {
	for key, value := range staticHeaders {
		if isReservedHeader(key) {
			return fmt.Errorf("static header %q is reserved", key)
		}
		headers.Set(key, value)
	}
	return nil
}

func isAllowedForwardHeader(header string) bool {
	for _, allowed := range allowedForwardHeaders {
		if strings.EqualFold(header, allowed) {
			return true
		}
	}
	return false
}

func isReservedHeader(header string) bool {
	for _, reserved := range reservedForwardHeaders {
		if strings.EqualFold(header, reserved) {
			return true
		}
	}
	return false
}

var allowedForwardHeaders = []string{
	"Content-Type",
	"Accept",
	"Anthropic-Version",
	"X-Request-Id",
	"Traceparent",
	"Tracestate",
	"Baggage",
	"X-B3-Traceid",
	"X-B3-Spanid",
	"X-B3-Parentspanid",
	"X-B3-Sampled",
	"X-B3-Flags",
}

var reservedForwardHeaders = []string{
	"Authorization",
	"Proxy-Authorization",
	"Cookie",
	"X-API-Key",
	"Host",
	"Content-Length",
	"Connection",
	"Keep-Alive",
	"Proxy-Connection",
	"Transfer-Encoding",
	"Upgrade",
	"Trailer",
	"TE",
}

var secureClient = newProviderClient(false)
var insecureClient = newProviderClient(true)

func newProviderClient(insecureSkipVerify bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecureSkipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func Do(req *http.Request, insecureSkipVerify bool) (*http.Response, error) {
	if insecureSkipVerify {
		return insecureClient.Do(req)
	}
	return secureClient.Do(req)
}
