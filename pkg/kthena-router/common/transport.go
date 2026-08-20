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
	"net"
	"net/http"
	"time"
)

// Fallback values mirroring http.DefaultTransport, used only when Clone() is
// unavailable (DefaultTransport is not *http.Transport).
const (
	pooledMaxIdleConns        = 100
	pooledIdleConnTimeout     = 90 * time.Second
	pooledDialTimeout         = 30 * time.Second
	pooledDialKeepAlive       = 30 * time.Second
	pooledTLSHandshakeTimeout = 10 * time.Second
)

// NewPooledTransport clones http.DefaultTransport and widens only the per-host
// idle pool. The stdlib default (DefaultMaxIdleConnsPerHost=2) churns
// connections under high concurrency against a single upstream pod, surfacing
// as EOF/500 on streaming responses that cannot be retried once begun. Other
// fields are left at the stdlib defaults Clone() already carries.
func NewPooledTransport(maxIdleConnsPerHost int) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   pooledDialTimeout,
				KeepAlive: pooledDialKeepAlive,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          pooledMaxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       pooledIdleConnTimeout,
			TLSHandshakeTimeout:   pooledTLSHandshakeTimeout,
			ExpectContinueTimeout: time.Second,
		}
	}

	t := base.Clone()
	t.MaxIdleConnsPerHost = maxIdleConnsPerHost
	return t
}
