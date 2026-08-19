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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUpstreamTransportPoolConfiguration asserts the PD-disaggregated
// forwarding transport widens the per-host idle pool. The stdlib default of 2
// churns connections under high concurrency, which surfaces as EOF/500 on
// streaming decode responses that cannot be retried once begun.
func TestUpstreamTransportPoolConfiguration(t *testing.T) {
	transport := newUpstreamTransport()

	assert.Equal(t, upstreamMaxIdleConnsPerHost, transport.MaxIdleConnsPerHost,
		"MaxIdleConnsPerHost must be widened beyond the stdlib default of 2")
	assert.Equal(t, upstreamMaxIdleConns, transport.MaxIdleConns)
	assert.Equal(t, upstreamIdleConnTimeout, transport.IdleConnTimeout)
	assert.Equal(t, upstreamTLSHandshakeTimeout, transport.TLSHandshakeTimeout)
	assert.NotNil(t, transport.DialContext, "DialContext must be configured")
	assert.True(t, transport.ForceAttemptHTTP2, "ForceAttemptHTTP2 should be enabled")
	assert.Zero(t, transport.ResponseHeaderTimeout,
		"no ResponseHeaderTimeout: streaming decode must not be cut off mid-flight")
}

// TestUpstreamTransportWiderThanStdlibDefault is the behavioural guard: the
// per-host pool must exceed http.DefaultTransport's default of 2.
func TestUpstreamTransportWiderThanStdlibDefault(t *testing.T) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || defaultTransport == nil {
		t.Skip("http.DefaultTransport is not *http.Transport in this environment")
	}
	assert.Greater(t, newUpstreamTransport().MaxIdleConnsPerHost, defaultTransport.MaxIdleConnsPerHost,
		"upstream pool must be wider than the stdlib default of 2")
}

// TestUpstreamTransportSingletonInitialized guards the package-level singleton
// shared by prefillerProxy, decoderProxy and NIXLConnector.prefill.
func TestUpstreamTransportSingletonInitialized(t *testing.T) {
	assert.NotNil(t, upstreamTransport)
	assert.Equal(t, upstreamMaxIdleConnsPerHost, upstreamTransport.MaxIdleConnsPerHost,
		"package-level singleton must carry the widened pool configuration")
}
