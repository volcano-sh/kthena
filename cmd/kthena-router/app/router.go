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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
	"github.com/volcano-sh/kthena/pkg/kthena-router/debug"
	"github.com/volcano-sh/kthena/pkg/kthena-router/router"
)

const (
	gracefulShutdownTimeout = 15 * time.Second
	routerConfigFile        = "/etc/config/routerConfiguration.yaml"
)

func NewRouter(store datastore.Store) *router.Router {
	return router.NewRouter(store, routerConfigFile)
}

// Starts router
func (s *Server) startRouter(ctx context.Context, router *router.Router, store datastore.Store) {
	gin.SetMode(gin.ReleaseMode)

	// Create listener manager for dynamic Gateway listener management
	listenerManager := NewListenerManager(ctx, router, store, s)
	s.listenerManager = listenerManager

	// Always start management endpoints on fixed port
	s.startManagementServer(ctx, router, store)

	// Register callback to handle Gateway events dynamically
	store.RegisterCallback("Gateway", func(data datastore.EventData) {
		key := fmt.Sprintf("%s/%s", data.Pod.Namespace, data.Pod.Name)
		switch data.EventType {
		case datastore.EventAdd, datastore.EventUpdate:
			if gatewayObj := store.GetGateway(key); gatewayObj != nil {
				if gw, ok := gatewayObj.(*gatewayv1.Gateway); ok {
					listenerManager.StartListenersForGateway(gw)
				}
			}
		case datastore.EventDelete:
			listenerManager.StopListenersForGateway(key)
		}
	})

	// Initialize listeners for existing Gateways that were added before callback registration
	// This ensures we don't lose Gateway events that occurred during controller startup
	existingGateways := store.GetAllGateways()
	for _, gatewayObj := range existingGateways {
		if gw, ok := gatewayObj.(*gatewayv1.Gateway); ok {
			klog.V(4).Infof("Initializing listeners for existing Gateway %s/%s", gw.Namespace, gw.Name)
			listenerManager.StartListenersForGateway(gw)
		}
	}
}

// startManagementServer starts the management HTTP server on fixed port
// This server handles healthz, readyz, metrics, and debug endpoints only
func (s *Server) startManagementServer(ctx context.Context, router *router.Router, store datastore.Store) {
	engine := gin.New()
	engine.Use(gin.LoggerWithWriter(gin.DefaultWriter, "/healthz", "/readyz", "/metrics"), gin.Recovery())

	// Management endpoints (no auth/access log middleware)
	engine.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"message": "ok",
		})
	})

	engine.GET("/readyz", func(c *gin.Context) {
		if s.HasSynced() {
			c.JSON(http.StatusOK, gin.H{
				"message": "router is ready",
			})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"message": "router is not ready",
			})
		}
	})

	// Prometheus metrics endpoint
	engine.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Debug endpoints
	debugHandler := debug.NewDebugHandler(store)
	debugGroup := engine.Group("/debug/config_dump")
	{
		// List resources
		debugGroup.GET("/modelroutes", debugHandler.ListModelRoutes)
		debugGroup.GET("/modelservers", debugHandler.ListModelServers)
		debugGroup.GET("/pods", debugHandler.ListPods)

		// Get specific resources
		debugGroup.GET("/namespaces/:namespace/modelroutes/:name", debugHandler.GetModelRoute)
		debugGroup.GET("/namespaces/:namespace/modelservers/:name", debugHandler.GetModelServer)
		debugGroup.GET("/namespaces/:namespace/pods/:name", debugHandler.GetPod)
	}

	server := &http.Server{
		Addr:    ":" + s.Port,
		Handler: engine.Handler(),
	}
	go func() {
		klog.Infof("Starting management server on port %s", s.Port)
		var err error
		if s.EnableTLS {
			if s.TLSCertFile == "" || s.TLSKeyFile == "" {
				klog.Fatalf("TLS enabled but cert or key file not specified")
			}
			err = server.ListenAndServeTLS(s.TLSCertFile, s.TLSKeyFile)
		} else {
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			klog.Fatalf("listen failed: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// graceful shutdown
		klog.Info("Shutting down management HTTP server ...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Management server shutdown failed: %v", err)
		}
		klog.Info("Management HTTP server exited")
	}()
}

// ListenerManager manages Gateway listeners dynamically
type ListenerManager struct {
	ctx           context.Context
	router        *router.Router
	store         datastore.Store
	server        *Server
	mu            sync.RWMutex
	listeners     map[string]*http.Server // key: gatewayKey/listenerName
	shutdownFuncs map[string]context.CancelFunc
}

// NewListenerManager creates a new listener manager
func NewListenerManager(ctx context.Context, router *router.Router, store datastore.Store, server *Server) *ListenerManager {
	return &ListenerManager{
		ctx:           ctx,
		router:        router,
		store:         store,
		server:        server,
		listeners:     make(map[string]*http.Server),
		shutdownFuncs: make(map[string]context.CancelFunc),
	}
}

// StartListenersForGateway starts listeners for a Gateway
func (lm *ListenerManager) StartListenersForGateway(gateway *gatewayv1.Gateway) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	gatewayKey := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)

	// Stop existing listeners for this gateway first
	lm.stopListenersForGatewayLocked(gatewayKey)

	// Start listeners for each listener in the Gateway
	for _, listener := range gateway.Spec.Listeners {
		listenerKey := fmt.Sprintf("%s/%s", gatewayKey, string(listener.Name))

		// Skip if already running
		if _, exists := lm.listeners[listenerKey]; exists {
			continue
		}

		engine := gin.New()
		engine.Use(gin.Recovery())

		// Add middleware for /v1/*path only
		engine.Use(AccessLogMiddleware(lm.router))
		engine.Use(AuthMiddleware(lm.router))

		// Add hostname matching middleware if specified
		if listener.Hostname != nil && *listener.Hostname != "" {
			hostname := string(*listener.Hostname)
			engine.Use(func(c *gin.Context) {
				if c.Request.Host != hostname {
					c.AbortWithStatus(http.StatusNotFound)
					return
				}
				c.Next()
			})
		}

		// Only handle /v1/*path on Gateway listeners
		engine.Any("/v1/*path", lm.router.HandlerFunc())

		// Return 404 for all other paths (management endpoints are on fixed port)
		engine.NoRoute(func(c *gin.Context) {
			c.JSON(http.StatusNotFound, gin.H{
				"message": "Not found. Management endpoints are available on the management port.",
			})
		})

		port := int32(listener.Port)
		server := &http.Server{
			Addr:    ":" + strconv.Itoa(int(port)),
			Handler: engine.Handler(),
		}

		lm.listeners[listenerKey] = server

		listenerName := string(listener.Name)
		protocol := string(listener.Protocol)

		// Create a context for this listener's goroutine
		listenerCtx, cancel := context.WithCancel(lm.ctx)
		lm.shutdownFuncs[listenerKey] = cancel

		go func(name string, p int32, proto string, srv *http.Server, ctx context.Context) {
			klog.Infof("Starting Gateway listener %s on port %d with protocol %s for /v1/*path", name, p, proto)
			var err error
			if proto == string(gatewayv1.HTTPSProtocolType) {
				if lm.server.EnableTLS && lm.server.TLSCertFile != "" && lm.server.TLSKeyFile != "" {
					err = srv.ListenAndServeTLS(lm.server.TLSCertFile, lm.server.TLSKeyFile)
				} else {
					klog.Errorf("HTTPS listener %s requires TLS configuration", name)
					return
				}
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				klog.Errorf("listen failed for listener %s: %v", name, err)
			}
		}(listenerName, port, protocol, server, listenerCtx)

		// Start graceful shutdown goroutine for this listener
		go func(key string, srv *http.Server, cancel context.CancelFunc) {
			<-listenerCtx.Done()
			klog.Infof("Shutting down Gateway listener %s ...", key)
			shutdownCtx, cancelTimeout := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
			defer cancelTimeout()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				klog.Errorf("Gateway listener %s shutdown failed: %v", key, err)
			}
			cancel()
		}(listenerKey, server, cancel)
	}
}

// StopListenersForGateway stops all listeners for a Gateway
func (lm *ListenerManager) StopListenersForGateway(gatewayKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	lm.stopListenersForGatewayLocked(gatewayKey)
}

func (lm *ListenerManager) stopListenersForGatewayLocked(gatewayKey string) {
	// Find and stop all listeners for this gateway
	for key, cancel := range lm.shutdownFuncs {
		if strings.HasPrefix(key, gatewayKey+"/") {
			cancel()
			delete(lm.listeners, key)
			delete(lm.shutdownFuncs, key)
		}
	}
}

func AccessLogMiddleware(gwRouter *router.Router) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Access log for "/v1/" only
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			c.Next()
			return
		}

		// Calling Middleware
		gwRouter.AccessLog()(c)
	}
}

func AuthMiddleware(gwRouter *router.Router) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Auth for "/v1/" only
		if !strings.HasPrefix(c.Request.URL.Path, "/v1/") {
			c.Next()
			return
		}

		// Calling Middleware
		gwRouter.Auth()(c)
		if c.IsAborted() {
			return
		}

		c.Next()
	}
}
