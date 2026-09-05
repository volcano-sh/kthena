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
	"os"
	"time"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/filesource"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

const defaultDrainTimeout = 5 * time.Minute

// Resource sources the router can read ModelRoute, ModelServer and
// ExternalModelProvider objects from.
const (
	// ResourceSourceKubernetes watches the Kubernetes API server.
	ResourceSourceKubernetes = "kubernetes"
	// ResourceSourceFile reads manifests from a local directory, allowing the
	// router to run without an API server.
	ResourceSourceFile = "file"
)

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
	MetricsPort                        int
	ExposeMetricsOnRouterPort          bool
	KubeAPIQPS                         float32
	KubeAPIBurst                       int
	// ResourceSource selects where ModelRoute, ModelServer and
	// ExternalModelProvider objects are read from. Defaults to
	// ResourceSourceKubernetes.
	ResourceSource string
	// ResourceDir holds the manifests when ResourceSource is ResourceSourceFile.
	ResourceDir string
	// ResourceSyncPeriod is how often ResourceDir is re-read.
	ResourceSyncPeriod time.Duration
	// RouterConfigFile is the scheduler and authentication configuration. It
	// defaults to DefaultRouterConfigFile.
	RouterConfigFile string
	// drainTimeout is HTTP server shutdown grace; not datastore state.
	drainTimeout time.Duration
}

func NewServer(port string, enableTLS bool, cert, key string, enableGatewayAPI bool, enableGatewayAPIInferenceExtension bool, debugPort, metricsPort int, exposeMetricsOnRouterPort bool, kubeAPIQPS float32, kubeAPIBurst int) *Server {
	return &Server{
		store:                              nil,
		EnableTLS:                          enableTLS,
		TLSCertFile:                        cert,
		TLSKeyFile:                         key,
		Port:                               port,
		EnableGatewayAPI:                   enableGatewayAPI,
		EnableGatewayAPIInferenceExtension: enableGatewayAPIInferenceExtension,
		DebugPort:                          debugPort,
		MetricsPort:                        metricsPort,
		ExposeMetricsOnRouterPort:          exposeMetricsOnRouterPort,
		KubeAPIQPS:                         kubeAPIQPS,
		KubeAPIBurst:                       kubeAPIBurst,
		ResourceSource:                     ResourceSourceKubernetes,
		drainTimeout:                       parseDrainTimeout(),
	}
}

func parseDrainTimeout() time.Duration {
	if v := os.Getenv("DRAIN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		klog.Warningf("Invalid DRAIN_TIMEOUT %q, using default %v", v, defaultDrainTimeout)
	}
	return defaultDrainTimeout
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
	r := NewRouter(store, s.RouterConfigFile)
	// start the configured resource source
	if s.ResourceSource == ResourceSourceFile {
		s.controllers = s.startFileSource(store, ctx.Done())
	} else {
		s.controllers = startControllers(store, ctx.Done(), s.EnableGatewayAPI, s.Port, s.EnableGatewayAPIInferenceExtension, s.KubeAPIQPS, s.KubeAPIBurst)
	}

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

// startFileSource loads resources from ResourceDir and keeps the store in sync
// with it, replacing the API server backed controllers.
func (s *Server) startFileSource(store datastore.Store, stop <-chan struct{}) Controller {
	source, err := filesource.New(s.ResourceDir, s.ResourceSyncPeriod, store)
	if err != nil {
		klog.Fatalf("Failed to create file resource source: %v", err)
	}
	klog.Infof("Reading resources from directory %s", s.ResourceDir)
	go func() {
		if err := source.Run(stop); err != nil {
			klog.Fatalf("Error running file resource source: %v", err)
		}
	}()
	if !cache.WaitForCacheSync(stop, source.HasSynced) {
		klog.Fatalf("Failed to load resources from %s", s.ResourceDir)
	}
	return source
}
