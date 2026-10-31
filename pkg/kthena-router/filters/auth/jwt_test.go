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

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

func TestNewJWTAuthenticator(t *testing.T) {
	tests := []struct {
		name          string
		config        *conf.RouterConfiguration
		expectEnabled bool
		expectRotator bool
	}{
		{
			name:          "nil config",
			config:        nil,
			expectEnabled: false,
			expectRotator: false,
		},
		{
			name: "empty JWKS URI",
			config: &conf.RouterConfiguration{
				Auth: conf.AuthenticationConfig{
					JwksUri: "",
				},
			},
			expectEnabled: false,
			expectRotator: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := NewJWTAuthenticator(tt.config)
			assert.NotNil(t, validator)
			assert.Equal(t, tt.expectEnabled, validator.IsEnabled())

			if tt.expectRotator {
				assert.NotNil(t, validator.rotator)
				validator.rotator.Stop()
			} else {
				assert.Nil(t, validator.rotator)
			}
		})
	}
}

func TestRebuildJwks(t *testing.T) {
	tests := []struct {
		name      string
		config    conf.AuthenticationConfig
		expectNil bool
	}{
		{
			name: "empty URI",
			config: conf.AuthenticationConfig{
				JwksUri: "",
			},
			expectNil: true,
		},
		{
			name: "invalid URI",
			config: conf.AuthenticationConfig{
				JwksUri: "invalid-url",
				Issuer:  "test-issuer",
			},
			expectNil: true,
		},
		{
			name: "valid URI",
			config: conf.AuthenticationConfig{
				JwksUri:   "https://raw.githubusercontent.com/istio/istio/release-1.27/security/tools/jwt/samples/jwks.json",
				Issuer:    "test-issuer",
				Audiences: []string{"test-audience"},
			},
			expectNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jwks := rebuildJwks(t.Context(), tt.config, fetchJWKS)

			if tt.expectNil {
				assert.Nil(t, jwks)
			} else {
				require.NotNil(t, jwks)
				assert.Equal(t, tt.config.Issuer, jwks.Issuer)
				assert.Equal(t, tt.config.Audiences, jwks.Audiences)
				assert.Equal(t, tt.config.JwksUri, jwks.Uri)
				assert.NotNil(t, jwks.Jwks)
				assert.Equal(t, defaultRefreshInterval, jwks.ExpiredTime)
			}
		})
	}
}

func TestJWKSRotatorStop_IsIdempotent(t *testing.T) {
	rotator := NewJWKSRotator(conf.AuthenticationConfig{JwksUri: "invalid-url"})
	rotator.Start(context.Background())
	rotator.Stop()
	require.NotPanics(t, rotator.Stop)
}

func TestJWKSRotatorStop_ConcurrentCallsAreSafe(t *testing.T) {
	// Two callers could both find stopCh open and both close it.
	for i := 0; i < 64; i++ {
		rotator := NewJWKSRotator(conf.AuthenticationConfig{JwksUri: "invalid-url"})
		rotator.Start(context.Background())
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Go(rotator.Stop)
		}
		wg.Wait()
	}
}

// TestJWKSRotatorStop_CancelsHangingRefresh stops the rotator while a periodic
// refresh waits on a JWKS endpoint that never answers; Stop must cancel the
// fetch instead of waiting for it forever.
func TestJWKSRotatorStop_CancelsHangingRefresh(t *testing.T) {
	var hang atomic.Bool
	refreshStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hang.Load() {
			_, _ = w.Write([]byte(`{"keys":[]}`))
			return
		}
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)

	rotator := NewJWKSRotator(conf.AuthenticationConfig{JwksUri: server.URL})
	rotator.refreshInterval = 10 * time.Millisecond
	rotator.Start(context.Background())
	hang.Store(true)
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		rotator.Stop()
		t.Fatal("periodic JWKS refresh did not start")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		rotator.Stop()
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while a JWKS refresh was hanging")
	}
}

// TestJWKSRotatorStop_WaitsForRotationLoop holds a periodic refresh open after
// Stop has cancelled it and checks that Stop waits for the rotation loop to exit.
func TestJWKSRotatorStop_WaitsForRotationLoop(t *testing.T) {
	var calls atomic.Int32
	refreshStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	rotator := NewJWKSRotator(conf.AuthenticationConfig{JwksUri: "jwks-uri"})
	rotator.refreshInterval = 10 * time.Millisecond
	rotator.fetch = func(ctx context.Context, uri string) (jwk.Set, error) {
		if calls.Add(1) == 1 {
			return jwk.NewSet(), nil // initial fetch in Start
		}
		select {
		case refreshStarted <- struct{}{}:
		default:
		}
		// A refresh that is still finishing after Stop cancelled ctx.
		<-release
		return nil, ctx.Err()
	}
	rotator.Start(context.Background())
	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		close(release)
		rotator.Stop()
		t.Fatal("periodic JWKS refresh did not start")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		rotator.Stop()
	}()
	select {
	case <-stopped:
		close(release)
		t.Fatal("Stop returned while the rotation loop was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the rotation loop finished")
	}
}
