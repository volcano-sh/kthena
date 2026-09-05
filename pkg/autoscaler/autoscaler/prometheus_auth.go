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

package autoscaler

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// SecretGetter reads a Secret by namespace and name.
type SecretGetter func(ctx context.Context, namespace, name string) (*corev1.Secret, error)

// prometheusAuthMaterial holds the resolved contents of a PrometheusAuth.
type prometheusAuthMaterial struct {
	bearerToken        string
	caPEM              string
	insecureSkipVerify bool
}

// resolvePrometheusAuth reads the Secrets referenced by auth from namespace.
func resolvePrometheusAuth(ctx context.Context, getSecret SecretGetter, namespace string, auth *v1alpha1.PrometheusAuth) (prometheusAuthMaterial, error) {
	var material prometheusAuthMaterial
	if auth == nil {
		return material, nil
	}
	if auth.BearerTokenSecret != nil {
		token, err := readSecretKey(ctx, getSecret, namespace, auth.BearerTokenSecret)
		if err != nil {
			return material, fmt.Errorf("bearerTokenSecret: %w", err)
		}
		material.bearerToken = token
	}
	if auth.TLSConfig != nil {
		material.insecureSkipVerify = auth.TLSConfig.InsecureSkipVerify
		if auth.TLSConfig.CASecret != nil {
			ca, err := readSecretKey(ctx, getSecret, namespace, auth.TLSConfig.CASecret)
			if err != nil {
				return material, fmt.Errorf("caSecret: %w", err)
			}
			material.caPEM = ca
		}
	}
	return material, nil
}

// readSecretKey returns the referenced value trimmed of surrounding whitespace; an optional selector tolerates a missing or unlabeled Secret, a missing key, or an empty value.
func readSecretKey(ctx context.Context, getSecret SecretGetter, namespace string, ref *corev1.SecretKeySelector) (string, error) {
	optional := ref.Optional != nil && *ref.Optional
	secret, err := getSecret(ctx, namespace, ref.Name)
	switch {
	case apierrors.IsNotFound(err):
		return tolerate(optional, fmt.Errorf("read secret %s/%s: %w", namespace, ref.Name, err))
	case err != nil:
		return "", fmt.Errorf("read secret %s/%s: %w", namespace, ref.Name, err)
	case secret.Labels[v1alpha1.PrometheusAuthSecretLabelKey] != v1alpha1.PrometheusAuthSecretLabelValue:
		return tolerate(optional, fmt.Errorf("secret %s/%s does not have label %s=%s", namespace, ref.Name, v1alpha1.PrometheusAuthSecretLabelKey, v1alpha1.PrometheusAuthSecretLabelValue))
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return tolerate(optional, fmt.Errorf("secret %s/%s has no key %q", namespace, ref.Name, ref.Key))
	}
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return tolerate(optional, fmt.Errorf("secret %s/%s has an empty value for key %q", namespace, ref.Name, ref.Key))
	}
	return trimmed, nil
}

// tolerate turns a missing-value error into an empty value when the selector is optional.
func tolerate(optional bool, err error) (string, error) {
	if optional {
		return "", nil
	}
	return "", err
}

// cacheKey identifies a client by its auth material so a rotated Secret yields a new client.
func (m prometheusAuthMaterial) cacheKey() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d:%s:%t", len(m.bearerToken), m.bearerToken, len(m.caPEM), m.caPEM, m.insecureSkipVerify)))
	return hex.EncodeToString(sum[:])
}

// roundTripper builds the auth-carrying transport and returns the raw transport so a replaced client can be closed.
func (m prometheusAuthMaterial) roundTripper(timeout time.Duration) (http.RoundTripper, *http.Transport, error) {
	transport := &http.Transport{
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	if m.caPEM != "" || m.insecureSkipVerify {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: m.insecureSkipVerify}
		if m.caPEM != "" {
			tlsConfig.RootCAs = x509.NewCertPool()
			if !tlsConfig.RootCAs.AppendCertsFromPEM([]byte(m.caPEM)) {
				return nil, nil, fmt.Errorf("caSecret does not contain a PEM certificate")
			}
		}
		transport.TLSClientConfig = tlsConfig
	}
	if m.bearerToken == "" {
		return transport, transport, nil
	}
	return bearerRoundTripper{token: m.bearerToken, next: transport}, transport, nil
}

// bearerRoundTripper adds the Authorization header to every request.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(req)
}
