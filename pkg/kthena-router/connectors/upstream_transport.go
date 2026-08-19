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

package connectors

import (
	"net"
	"net/http"
	"time"
)

const (
	// upstreamMaxIdleConnsPerHost widens http.DefaultTransport's per-host idle
	// pool (default 2) for PD-disaggregated forwarding (prefillerProxy,
	// decoderProxy, NIXLConnector.prefill). The default of 2 causes connection
	// churn under high concurrency that surfaces as EOF/500 on streaming
	// decode responses, which cannot be retried once the first byte is written.
	upstreamMaxIdleConnsPerHost = 64
	upstreamMaxIdleConns        = 100
	upstreamIdleConnTimeout     = 90 * time.Second
	upstreamDialTimeout         = 30 * time.Second
	upstreamDialKeepAlive       = 30 * time.Second
	upstreamTLSHandshakeTimeout = 10 * time.Second
)

// upstreamTransport is the shared RoundTripper for PD-disaggregated forwarding
// to upstream pods (prefillerProxy, decoderProxy, NIXLConnector.prefill).
//
// It clones http.DefaultTransport and only widens the per-host idle pool, so
// hot prefill/decode pods reuse pooled connections under high concurrency
// instead of rebuilding them on every request. A wider pool here is essential
// because the PD retry loop cannot retry a decode once streaming has begun,
// so connection churn that triggers EOF propagates directly to the client.
var upstreamTransport = newUpstreamTransport()

// newUpstreamTransport builds a transport tuned for high-concurrency PD
// forwarding. Kept as a constructor so the configuration is testable in
// isolation.
func newUpstreamTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Fall back to a fully-specified transport when the default has been
		// replaced with a non-*http.Transport RoundTripper.
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   upstreamDialTimeout,
				KeepAlive: upstreamDialKeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          upstreamMaxIdleConns,
			MaxIdleConnsPerHost:   upstreamMaxIdleConnsPerHost,
			IdleConnTimeout:       upstreamIdleConnTimeout,
			TLSHandshakeTimeout:   upstreamTLSHandshakeTimeout,
			ExpectContinueTimeout: time.Second,
		}
	}

	t := base.Clone()
	t.MaxIdleConnsPerHost = upstreamMaxIdleConnsPerHost
	t.MaxIdleConns = upstreamMaxIdleConns
	t.IdleConnTimeout = upstreamIdleConnTimeout
	t.TLSHandshakeTimeout = upstreamTLSHandshakeTimeout
	t.DialContext = (&net.Dialer{
		Timeout:   upstreamDialTimeout,
		KeepAlive: upstreamDialKeepAlive,
	}).DialContext
	t.ForceAttemptHTTP2 = true
	return t
}
