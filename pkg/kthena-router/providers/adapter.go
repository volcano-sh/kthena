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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http/httpguts"
	corev1 "k8s.io/api/core/v1"

	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/common"
)

type Adapter interface {
	BuildRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, modelRequest map[string]interface{}) (*http.Request, error)
	// ResponseParser returns the usage parser for a request path. The gin
	// context carries request-scoped adapter state (for example whether the
	// adapter injected usage reporting in BuildRequest), so callers must pass
	// the same context that BuildRequest received.
	ResponseParser(c *gin.Context, path string) ResponseUsageParser
}

// TokenUsage is the provider-neutral token accounting view returned by a
// response parser.
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// StreamUsageParseResult describes token usage found in a provider stream
// event. SuppressLine is true when the event exists only to satisfy usage
// reporting that the router itself injected into the request, so the line
// must not be forwarded to the client.
type StreamUsageParseResult struct {
	Usage        TokenUsage
	HasUsage     bool
	SuppressLine bool
}

// ResponseUsageParser normalizes provider-specific response bodies and stream
// events into TokenUsage while tracking whether a streaming response completed.
type ResponseUsageParser interface {
	ParseStreamLine(line string) StreamUsageParseResult
	ParseBody(body []byte) (TokenUsage, bool)
	FinalStreamUsage() (TokenUsage, bool)
	RecordStreamLineWritten(line string)
	StreamCompleted() bool
}

type UnsupportedPathError struct {
	ProviderType networkingv1alpha1.ExternalProviderType
	Path         string
}

func (e *UnsupportedPathError) Error() string {
	return fmt.Sprintf("provider type %q does not support path %q", e.ProviderType, e.Path)
}

// ConfigurationError identifies invalid or unavailable provider configuration.
// Router-facing code can classify it without knowing credential or header
// details owned by a protocol adapter.
type ConfigurationError struct {
	err error
}

func (e *ConfigurationError) Error() string {
	return e.err.Error()
}

func (e *ConfigurationError) Unwrap() error {
	return e.err
}

func newConfigurationError(format string, args ...interface{}) error {
	return &ConfigurationError{err: fmt.Errorf(format, args...)}
}

func NewAdapter(providerType networkingv1alpha1.ExternalProviderType) (Adapter, error) {
	switch providerType {
	case "", networkingv1alpha1.OpenAI:
		return openAIAdapter{}, nil
	case networkingv1alpha1.Anthropic:
		return anthropicAdapter{}, nil
	default:
		return nil, newConfigurationError("unsupported provider type %q", providerType)
	}
}

// DefaultAdapter returns the adapter for in-cluster model servers, which
// expose OpenAI-compatible APIs.
func DefaultAdapter() Adapter {
	return openAIAdapter{}
}

// routerInjectedUsage reports whether the router injected usage reporting
// into the upstream request on behalf of the client, in which case the
// resulting usage-only stream event must not reach the client.
func routerInjectedUsage(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(common.TokenUsageKey)
	if !ok {
		return false
	}
	injected, ok := value.(bool)
	return ok && injected
}

// modelOverride returns the configured upstream model override, if any.
func modelOverride(provider *networkingv1alpha1.ExternalModelProvider) (string, bool) {
	if provider == nil || provider.Spec.Model == nil || *provider.Spec.Model == "" {
		return "", false
	}
	return *provider.Spec.Model, true
}

// UpstreamModelName returns the model name the provider serves for a request:
// the configured spec.model override when set, otherwise the route model from
// the client request. It mirrors the rewrite applied by BuildRequest.
func UpstreamModelName(provider *networkingv1alpha1.ExternalModelProvider, requestModel string) string {
	if model, ok := modelOverride(provider); ok {
		return model
	}
	return requestModel
}

// ValidateConfiguration validates provider settings that are required by every
// request. It is used both by reconciliation and as defense in depth in the
// request-building path.
func ValidateConfiguration(provider *networkingv1alpha1.ExternalModelProvider) error {
	if provider == nil {
		return newConfigurationError("provider is nil")
	}
	if _, err := NewAdapter(provider.Spec.ProviderType); err != nil {
		return err
	}
	if _, err := parseProviderBaseURL(provider.Spec.BaseURL); err != nil {
		return newConfigurationError("invalid provider base URL: %w", err)
	}
	if err := validateStaticHeaders(provider.Spec.Headers); err != nil {
		return err
	}
	if provider.Spec.Auth != nil {
		switch provider.Spec.Auth.Scheme {
		case "", networkingv1alpha1.ProviderAuthSchemeBearer, networkingv1alpha1.ProviderAuthSchemeAPIKey:
		default:
			return newConfigurationError("unsupported provider auth scheme %q", provider.Spec.Auth.Scheme)
		}
	}
	return nil
}

func buildProviderRequest(c *gin.Context, req *http.Request, provider *networkingv1alpha1.ExternalModelProvider, modelRequest map[string]interface{}, upstreamURL *url.URL, rewriteBody bool, protocolHeaders ...string) (*http.Request, error) {
	if model, ok := modelOverride(provider); ok {
		modelRequest["model"] = model
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

	reqCopy := req.Clone(req.Context())
	reqCopy.URL = upstreamURL
	reqCopy.Host = upstreamURL.Host
	reqCopy.RequestURI = ""
	reqCopy.Header = sanitizeRequestHeaders(req.Header, protocolHeaders)
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

func parseProviderBaseURL(baseURL string) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("provider base URL must use https and include a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, fmt.Errorf("provider base URL must not contain userinfo, query, or fragment")
	}
	return parsed, nil
}

func appendProviderPath(parsed *url.URL, requestPath, rawQuery string) *url.URL {
	upstreamURL := *parsed
	basePath := strings.TrimRight(parsed.Path, "/")
	pathSuffix := strings.TrimLeft(requestPath, "/")
	if pathSuffix == "" {
		upstreamURL.Path = basePath
	} else if basePath == "" {
		upstreamURL.Path = "/" + pathSuffix
	} else {
		upstreamURL.Path = basePath + "/" + pathSuffix
	}
	upstreamURL.RawPath = ""
	upstreamURL.RawQuery = rawQuery
	return &upstreamURL
}

func trimAPIVersionPrefix(requestPath string) string {
	path := strings.TrimLeft(requestPath, "/")
	path = strings.TrimPrefix(path, "v1/")
	if path == "v1" {
		return ""
	}
	return path
}

func providerToken(provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret) (string, error) {
	if provider.Spec.Auth == nil {
		return "", nil
	}
	if secret == nil {
		return "", newConfigurationError("secret %s is not loaded", provider.Spec.Auth.SecretRef.Name)
	}
	key := provider.Spec.Auth.SecretRef.Key
	value, ok := secret.Data[key]
	if !ok || len(value) == 0 {
		return "", newConfigurationError("secret key %s is not found", key)
	}
	return NormalizeCredential(value)
}

// NormalizeCredential removes surrounding whitespace commonly introduced by
// Secret creation workflows and validates that the result is safe to place in
// an HTTP header. Callers should use this for both status validation and
// request construction so they agree on whether a credential is usable.
func NormalizeCredential(value []byte) (string, error) {
	token := strings.TrimSpace(string(value))
	if token == "" {
		return "", newConfigurationError("provider credential is empty after trimming whitespace")
	}
	if !httpguts.ValidHeaderFieldValue(token) {
		return "", newConfigurationError("provider credential contains invalid HTTP header characters")
	}
	return token, nil
}

func applyProviderAuth(headers http.Header, provider *networkingv1alpha1.ExternalModelProvider, secret *corev1.Secret, defaultScheme networkingv1alpha1.ProviderAuthScheme) error {
	token, err := providerToken(provider, secret)
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}

	scheme := defaultScheme
	if provider.Spec.Auth.Scheme != "" {
		scheme = provider.Spec.Auth.Scheme
	}
	headers.Del("Authorization")
	headers.Del("x-api-key")
	switch scheme {
	case networkingv1alpha1.ProviderAuthSchemeBearer:
		headers.Set("Authorization", "Bearer "+token)
	case networkingv1alpha1.ProviderAuthSchemeAPIKey:
		headers.Set("x-api-key", token)
	default:
		return newConfigurationError("unsupported provider auth scheme %q", scheme)
	}
	return nil
}

func sanitizeRequestHeaders(headers http.Header, protocolHeaders []string) http.Header {
	clean := http.Header{}
	for key, values := range headers {
		if !isAllowedForwardHeader(key, protocolHeaders) {
			continue
		}
		for _, value := range values {
			clean.Add(key, value)
		}
	}
	return clean
}

func applyStaticHeaders(headers http.Header, staticHeaders map[string]string) error {
	if err := validateStaticHeaders(staticHeaders); err != nil {
		return err
	}
	for key, value := range staticHeaders {
		headers.Set(key, value)
	}
	return nil
}

func validateStaticHeaders(staticHeaders map[string]string) error {
	for key, value := range staticHeaders {
		if !httpguts.ValidHeaderFieldName(key) {
			return newConfigurationError("static header %q has an invalid name", key)
		}
		if common.IsReservedProviderHeader(key) {
			return newConfigurationError("static header %q is reserved", key)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return newConfigurationError("static header %q has an invalid value", key)
		}
	}
	return nil
}

func isAllowedForwardHeader(header string, protocolHeaders []string) bool {
	for _, allowed := range allowedForwardHeaders {
		if strings.EqualFold(header, allowed) {
			return true
		}
	}
	for _, allowed := range protocolHeaders {
		if strings.EqualFold(header, allowed) {
			return true
		}
	}
	return false
}

var allowedForwardHeaders = []string{
	"Content-Type",
	"Accept",
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

// providerTransportBaseline preserves the connection-stage defaults from Go's
// standard transport for the isolated fallback used with custom RoundTrippers.
var providerTransportBaseline = newProviderTransportBaseline()

var secureClient = newProviderClient(false)
var insecureClient = newProviderClient(true)

func newProviderTransportBaseline() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		return transport.Clone()
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

func newProviderClient(insecureSkipVerify bool) *http.Client {
	return &http.Client{
		Transport: providerTransport(http.DefaultTransport, insecureSkipVerify),
		// Do not set Client.Timeout: external streaming responses can be
		// long-lived. The downstream request context still cancels the upstream
		// request when the client disconnects.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func providerTransport(base http.RoundTripper, insecureSkipVerify bool) http.RoundTripper {
	if transport, ok := base.(*http.Transport); ok {
		cloned := transport.Clone()
		if insecureSkipVerify {
			enableInsecureSkipVerify(cloned)
		}
		return cloned
	}
	if !insecureSkipVerify {
		return base
	}
	// A custom RoundTripper cannot be cloned or have its TLS policy adjusted.
	// Use an isolated transport for the opt-in insecure policy instead of
	// panicking or mutating a replaceable package-level variable. Start from
	// the standard transport defaults so dial and TLS handshake bounds remain.
	fallback := providerTransportBaseline.Clone()
	enableInsecureSkipVerify(fallback)
	return fallback
}

func enableInsecureSkipVerify(transport *http.Transport) {
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
}

func Do(req *http.Request, insecureSkipVerify bool) (*http.Response, error) {
	if insecureSkipVerify {
		return insecureClient.Do(req)
	}
	return secureClient.Do(req)
}

func isJSONStreamEvent(line, eventType string) bool {
	payload, ok := streamDataPayload(line)
	if !ok {
		return false
	}
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return false
	}
	return event.Type == eventType
}

func streamDataPayload(line string) ([]byte, bool) {
	const dataPrefix = "data:"
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, dataPrefix) {
		return nil, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, dataPrefix))
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	return []byte(payload), true
}
