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

package router

import (
	"net"
	"net/http"
	"time"
)

const (
	// maxIdleConnsPerHost overrides http.DefaultTransport's default of 2.
	// Under high concurrency against a single upstream pod, the default of 2
	// forces frequent connection churn (dial/close/re-dial) that surfaces as
	// unexpected EOF / 500 on streaming responses, which cannot be retried
	// against another pod once the first byte is written. 64 keeps a deep idle
	// pool per host so hot pods reuse pooled connections instead of rebuilding.
	maxIdleConnsPerHost = 64
	// maxIdleConns caps the total idle pool across all hosts, matching the
	// value used by the external provider transport
	// (see providers.newProviderTransportBaseline).
	maxIdleConns = 100
	// idleConnTimeout reuses http.DefaultTransport's default so stale idle
	// connections still close when traffic drops.
	idleConnTimeout = 90 * time.Second
	// dialTimeout and dialKeepAlive mirror http.DefaultTransport's dialer so
	// connection establishment behaviour is unchanged.
	dialTimeout   = 30 * time.Second
	dialKeepAlive = 30 * time.Second
	// tlsHandshakeTimeout mirrors http.DefaultTransport's default.
	tlsHandshakeTimeout = 10 * time.Second
)

// proxyTransport is the shared RoundTripper for forwarding inference requests
// to upstream model-server pods (doRequest and the PD decode path).
//
// It clones http.DefaultTransport so the standard library's robust default
// transport behaviour is preserved, then only widens the per-host idle
// connection pool. The default MaxIdleConnsPerHost of 2 is the bottleneck
// under high concurrency: extra concurrent requests rebuild connections on
// every cycle, which surfaces as EOF/500 once a streaming response has begun
// (streaming responses cannot be retried against another pod).
var proxyTransport = newProxyTransport()

// newProxyTransport builds a transport tuned for high-concurrency upstream
// forwarding. A dedicated constructor (instead of an inline var initializer)
// keeps the configuration testable in isolation.
func newProxyTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Fall back to a fully-specified transport when the default has been
		// replaced with a non-*http.Transport RoundTripper (e.g. in tests or
		// by an embedding binary). Mirrors providers.newProviderTransportBaseline.
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: dialKeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ExpectContinueTimeout: time.Second,
		}
	}

	t := base.Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	t.MaxIdleConns = maxIdleConns
	t.IdleConnTimeout = idleConnTimeout
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: dialKeepAlive,
	}).DialContext
	t.ForceAttemptHTTP2 = true
	return t
}
