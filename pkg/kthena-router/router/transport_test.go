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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestProxyTransportPoolConfiguration asserts the upstream forwarding
// transport widens the per-host idle pool so high-concurrency requests
// against a single pod reuse pooled connections instead of churning. This
// guards against regressions silently dropping the override back to the
// stdlib default of 2.
func TestProxyTransportPoolConfiguration(t *testing.T) {
	transport := newProxyTransport()

	assert.Equal(t, maxIdleConnsPerHost, transport.MaxIdleConnsPerHost,
		"MaxIdleConnsPerHost must be widened beyond the stdlib default of 2")
	assert.Equal(t, maxIdleConns, transport.MaxIdleConns)
	assert.Equal(t, idleConnTimeout, transport.IdleConnTimeout)
	assert.Equal(t, tlsHandshakeTimeout, transport.TLSHandshakeTimeout)
	assert.NotNil(t, transport.DialContext, "DialContext must be configured")
	assert.True(t, transport.ForceAttemptHTTP2, "ForceAttemptHTTP2 should be enabled")
	assert.Zero(t, transport.ResponseHeaderTimeout,
		"no ResponseHeaderTimeout: streaming prefill must not be cut off mid-flight")
}

// TestProxyTransportWiderThanStdlibDefault is the behavioural guard: the
// entire point of this PR is that the per-host pool exceeds
// http.DefaultTransport's default of 2.
func TestProxyTransportWiderThanStdlibDefault(t *testing.T) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		t.Skip("http.DefaultTransport is not *http.Transport in this environment")
	}
	assert.Greater(t, newProxyTransport().MaxIdleConnsPerHost, defaultTransport.MaxIdleConnsPerHost,
		"upstream pool must be wider than the stdlib default of 2")
}

// TestProxyTransportSingletonInitialized guards the package-level singleton:
// if it is ever left as a nil/round-tripper stub, doRequest would panic on
// the first request.
func TestProxyTransportSingletonInitialized(t *testing.T) {
	assert.NotNil(t, proxyTransport)
	assert.Equal(t, maxIdleConnsPerHost, proxyTransport.MaxIdleConnsPerHost,
		"package-level singleton must carry the widened pool configuration")
}
