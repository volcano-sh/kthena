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
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/scheduler/plugins/conf"
)

const (
	defaultRefreshInterval = time.Hour * 24 * 7 // 7 days
	maxRetryAttempts       = 3
)

// Jwks represents the JWKS data structure
type Jwks struct {
	Jwks      jwk.Set
	Audiences []string
	Issuer    string
	// Used to update jwks
	Uri         string
	ExpiredTime time.Duration
}

// JWKSRotator handles the rotation and caching of JWKS
type JWKSRotator struct {
	config          conf.AuthenticationConfig
	refreshInterval time.Duration
	stopCh          chan struct{}
	mu              sync.RWMutex
	jwks            *Jwks
}

// NewJWKSRotator creates a new JWKS rotator
func NewJWKSRotator(config conf.AuthenticationConfig) *JWKSRotator {
	return &JWKSRotator{
		config:          config,
		refreshInterval: defaultRefreshInterval,
		stopCh:          make(chan struct{}),
	}
}

// Start begins the JWKS rotation process
func (jr *JWKSRotator) Start(ctx context.Context) {
	klog.V(4).Info("Starting JWKS rotator")

	// Perform initial JWKS fetch
	jr.rotateJWKS()

	// Start the rotation goroutine
	go jr.rotationLoop(ctx)
}

// Stop stops the JWKS rotation process
func (jr *JWKSRotator) Stop() {
	klog.V(4).Info("Stopping JWKS rotator")
	close(jr.stopCh)
}

// GetJwks returns the current JWKS cache
func (jr *JWKSRotator) GetJwks() *Jwks {
	jr.mu.RLock()
	defer jr.mu.RUnlock()
	return jr.jwks
}

// rotationLoop runs the periodic JWKS rotation
func (jr *JWKSRotator) rotationLoop(ctx context.Context) {
	ticker := time.NewTicker(jr.refreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(4).Info("Context cancelled, stopping JWKS rotation")
			return
		case <-jr.stopCh:
			klog.V(4).Info("Stop signal received, stopping JWKS rotation")
			return
		case <-ticker.C:
			jr.rotateJWKS()
		}
	}
}

// rotateJWKS performs the actual JWKS rotation
func (jr *JWKSRotator) rotateJWKS() {
	klog.V(4).Infof("Rotating JWKS from URI: %s", jr.config.JwksUri)

	// Fetch new JWKS
	newJwks := rebuildJwks(jr.config)
	if newJwks != nil {
		jr.mu.Lock()
		jr.jwks = newJwks
		jr.mu.Unlock()
		klog.V(4).Info("JWKS rotation completed successfully")
	} else {
		klog.Error("Failed to rotate JWKS")
	}
}

// rebuildJwks creates a new Jwks instance by fetching from the configured URI
func rebuildJwks(config conf.AuthenticationConfig) *Jwks {
	var keySet jwk.Set
	var err error
	for i := 0; i < maxRetryAttempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		keySet, err = jwk.Fetch(ctx, config.JwksUri)
		cancel()
		if err != nil {
			klog.V(4).Infof("failed to fetch JWKS from %s: %v", config.JwksUri, err)
		} else {
			return &Jwks{
				Jwks:      keySet,
				Audiences: config.Audiences,
				Issuer:    config.Issuer,
				Uri:       config.JwksUri,
				// Default expiration time is set to 7 days
				ExpiredTime: time.Hour * 24 * 7, // Default to 7 days
			}
		}
	}

	return nil
}
