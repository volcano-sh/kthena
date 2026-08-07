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

package app

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const defaultDrainTimeout = 5 * time.Minute

const (
	// defaultReadHeaderTimeout bounds how long a client may take to send the
	// complete request headers, so a slow-header client cannot hold a
	// connection and its goroutine indefinitely.
	defaultReadHeaderTimeout = 10 * time.Second
	// defaultIdleTimeout bounds how long an idle keep-alive connection is kept
	// open between requests.
	defaultIdleTimeout = 120 * time.Second
	// defaultMaxHeaderBytes matches net/http's own default, so the limit only
	// becomes stricter when an operator asks for it.
	defaultMaxHeaderBytes = http.DefaultMaxHeaderBytes
)

// serverLimits are the connection-level bounds applied to every router HTTP
// listener. WriteTimeout is deliberately absent: streaming inference responses
// are long-lived and a global write deadline would truncate them.
type serverLimits struct {
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	maxHeaderBytes    int
}

type Server struct {
	store                              datastore.Store
	controllers                        Controller
	listenerManager                    *ListenerManager
	EnableTLS                          bool
	TLSCertFile                        string
	TLSKeyFile                         string
	Port                               string
	EnableGatewayAPI                   bool
	EnableGatewayAPIInferenceExtension bool
	DebugPort                          int
	KubeAPIQPS                         float32
	KubeAPIBurst                       int
	// drainTimeout is HTTP server shutdown grace; not datastore state.
	drainTimeout time.Duration
	// limits are the connection-level bounds shared by every HTTP listener.
	limits serverLimits
}

func NewServer(port string, enableTLS bool, cert, key string, enableGatewayAPI bool, enableGatewayAPIInferenceExtension bool, debugPort int, kubeAPIQPS float32, kubeAPIBurst int) *Server {
	return &Server{
		store:                              nil,
		EnableTLS:                          enableTLS,
		TLSCertFile:                        cert,
		TLSKeyFile:                         key,
		Port:                               port,
		EnableGatewayAPI:                   enableGatewayAPI,
		EnableGatewayAPIInferenceExtension: enableGatewayAPIInferenceExtension,
		DebugPort:                          debugPort,
		KubeAPIQPS:                         kubeAPIQPS,
		KubeAPIBurst:                       kubeAPIBurst,
		drainTimeout:                       parseDrainTimeout(),
		limits:                             parseServerLimits(),
	}
}

func parseDrainTimeout() time.Duration {
	return parsePositiveDurationEnv("DRAIN_TIMEOUT", defaultDrainTimeout)
}

// parseServerLimits reads the listener bounds from READ_HEADER_TIMEOUT,
// IDLE_TIMEOUT and MAX_HEADER_BYTES. Invalid or non-positive values fall back
// to the defaults, so a bad value can never leave a listener unbounded.
func parseServerLimits() serverLimits {
	return serverLimits{
		readHeaderTimeout: parsePositiveDurationEnv("READ_HEADER_TIMEOUT", defaultReadHeaderTimeout),
		idleTimeout:       parsePositiveDurationEnv("IDLE_TIMEOUT", defaultIdleTimeout),
		maxHeaderBytes:    parseMaxHeaderBytes(),
	}
}

func parsePositiveDurationEnv(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		klog.Warningf("Invalid %s %q, using default %v", key, v, fallback)
	}
	return fallback
}

func parseMaxHeaderBytes() int {
	if v := os.Getenv("MAX_HEADER_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		klog.Warningf("Invalid MAX_HEADER_BYTES %q, using default %v", v, defaultMaxHeaderBytes)
	}
	return defaultMaxHeaderBytes
}

func (s *Server) Run(ctx context.Context) {
	// Build store options. When REDIS_HOST is set, use a Redis-backed on-flight
	// counter so that multiple router replicas share a globally consistent view
	// of in-flight request counts, enabling better cross-router scheduling.
	var storeOpts []datastore.Option
	if os.Getenv("REDIS_HOST") != "" {
		if redisClient := utils.TryGetRedisClient(); redisClient != nil {
			klog.Infof("Redis on-flight counter enabled: cross-router in-flight tracking active")
			storeOpts = append(storeOpts, datastore.WithRedisOnFlightCounter(datastore.NewRedisOnFlightCounter(redisClient)))
		} else {
			klog.Warningf("REDIS_HOST is set but Redis connection failed; falling back to local on-flight counter")
		}
	}

	// create store
	store := datastore.New(storeOpts...)
	s.store = store

	// must be run before the controller, because it will register callbacks
	r := NewRouter(store)
	// start controller
	s.controllers = startControllers(store, ctx.Done(), s.EnableGatewayAPI, s.Port, s.EnableGatewayAPIInferenceExtension, s.KubeAPIQPS, s.KubeAPIBurst)

	// Start store's periodic update loop after controllers have synced
	if !cache.WaitForCacheSync(ctx.Done(), s.controllers.HasSynced) {
		klog.Fatalf("Failed to sync controllers")
	}
	klog.Infof("Controllers have synced, starting store periodic update loop")
	store.Run(ctx)
	// start router
	s.startRouter(ctx, r, store)

	// Block until context is cancelled to keep the process running
	klog.Info("Router server started, waiting for shutdown signal...")
	<-ctx.Done()
	klog.Info("Router server shutting down...")
}

func (s *Server) HasSynced() bool {
	return s.controllers.HasSynced() && s.store.HasSynced()
}
