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

package common

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

const testMaxIdleConnsPerHost = 64

func TestNewPooledTransportOverridesPerHostPool(t *testing.T) {
	transport := NewPooledTransport(testMaxIdleConnsPerHost)
	assert.Equal(t, testMaxIdleConnsPerHost, transport.MaxIdleConnsPerHost)
	assert.NotNil(t, transport.DialContext)
}

// DefaultTransport stores 0 in MaxIdleConnsPerHost and applies
// DefaultMaxIdleConnsPerHost(=2) internally, so compare against the constant.
func TestNewPooledTransportWiderThanStdlibDefault(t *testing.T) {
	assert.Greater(t, NewPooledTransport(testMaxIdleConnsPerHost).MaxIdleConnsPerHost,
		http.DefaultMaxIdleConnsPerHost)
}

// Streaming prefill/decode must not be cut off mid-flight.
func TestNewPooledTransportNoResponseHeaderTimeout(t *testing.T) {
	assert.Zero(t, NewPooledTransport(testMaxIdleConnsPerHost).ResponseHeaderTimeout)
}
