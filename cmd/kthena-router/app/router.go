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

	// Start debug server on localhost
	s.startDebugServer(ctx, store)

	// Gateway API features are optional
	if s.EnableGatewayAPI {
		// Create listener manager for dynamic Gateway listener management
		listenerManager := NewListenerManager(ctx, router, store, s)
		s.listenerManager = listenerManager

		// Default gateway is created in startControllers, so it will be handled by the callback

		// Register callback to handle Gateway events dynamically
		store.RegisterCallback("Gateway", func(data datastore.EventData) {
			key := fmt.Sprintf("%s/%s", data.Gateway.Namespace, data.Gateway.Name)
			switch data.EventType {
			case datastore.EventAdd, datastore.EventUpdate:
				if gw := store.GetGateway(key); gw != nil {
					listenerManager.StartListenersForGateway(gw)
				}
			case datastore.EventDelete:
				listenerManager.StopListenersForGateway(key)
			}
		})

		// Initialize listeners for existing Gateways that were added before callback registration
		// This ensures we don't lose Gateway events that occurred during controller startup
		existingGateways := store.GetAllGateways()
		for _, gw := range existingGateways {
			klog.V(4).Infof("Initializing listeners for existing Gateway %s/%s", gw.Namespace, gw.Name)
			listenerManager.StartListenersForGateway(gw)
		}
	} else {
		// When gateway-api is disabled, start standalone default server
		s.startDefaultServer(ctx, router, store)
		klog.Info("Gateway API features are disabled")
	}
}

// startDebugServer starts a separate debug server on localhost
// This server only handles debug endpoints and is not accessible from outside
func (s *Server) startDebugServer(ctx context.Context, store datastore.Store) {
	engine := gin.New()
	engine.Use(gin.Recovery())

	// Debug endpoints
	debugHandler := debug.NewDebugHandler(store)
	debugGroup := engine.Group("/debug/config_dump")
	{
		// List resources
		debugGroup.GET("/modelroutes", debugHandler.ListModelRoutes)
		debugGroup.GET("/modelservers", debugHandler.ListModelServers)
		debugGroup.GET("/pods", debugHandler.ListPods)
		debugGroup.GET("/gateways", debugHandler.ListGateways)
		debugGroup.GET("/httproutes", debugHandler.ListHTTPRoutes)
		debugGroup.GET("/inferencepools", debugHandler.ListInferencePools)

		// Get specific resources
		debugGroup.GET("/namespaces/:namespace/modelroutes/:name", debugHandler.GetModelRoute)
		debugGroup.GET("/namespaces/:namespace/modelservers/:name", debugHandler.GetModelServer)
		debugGroup.GET("/namespaces/:namespace/pods/:name", debugHandler.GetPod)
		debugGroup.GET("/namespaces/:namespace/gateways/:name", debugHandler.GetGateway)
		debugGroup.GET("/namespaces/:namespace/httproutes/:name", debugHandler.GetHTTPRoute)
		debugGroup.GET("/namespaces/:namespace/inferencepools/:name", debugHandler.GetInferencePool)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf("localhost:%d", s.DebugPort),
		Handler: engine.Handler(),
	}
	go func() {
		klog.Infof("Starting debug server on localhost:%d", s.DebugPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			klog.Fatalf("Debug server listen failed: %v", err)
		}
	}()

	go func() {
		<-ctx.Done()
		// graceful shutdown
		klog.Info("Shutting down debug HTTP server ...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Debug server shutdown failed: %v", err)
		}
		klog.Info("Debug HTTP server exited")
	}()
}

// startDefaultServer starts the default HTTP server on fixed port
// This server handles healthz, readyz, metrics, and /v1/*path
func (s *Server) startDefaultServer(ctx context.Context, router *router.Router, store datastore.Store) {
	engine := gin.New()
	engine.Use(gin.LoggerWithWriter(gin.DefaultWriter, "/healthz", "/readyz", "/metrics"), gin.Recovery())

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

	// Handle /v1/*path with middleware
	v1Group := engine.Group("/v1")
	v1Group.Use(AccessLogMiddleware(router))
	v1Group.Use(AuthMiddleware(router))
	v1Group.Any("/*path", router.HandlerFunc())

	server := &http.Server{
		Addr:    ":" + s.Port,
		Handler: engine.Handler(),
	}
	go func() {
		klog.Infof("Starting default server on port %s", s.Port)
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
		klog.Info("Shutting down default HTTP server ...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			klog.Errorf("Default server shutdown failed: %v", err)
		}
		klog.Info("Default HTTP server exited")
	}()
}

// ListenerConfig represents a single listener configuration
type ListenerConfig struct {
	GatewayKey   string
	ListenerName string
	Port         int32
	Hostname     *string // nil means match all hostnames
	Protocol     string
}

// PortListenerInfo contains all listeners for a specific port
type PortListenerInfo struct {
	mu           sync.RWMutex
	Server       *http.Server
	ShutdownFunc context.CancelFunc
	Listeners    []ListenerConfig
	LastError    error
}

// ListenerManager manages Gateway listeners dynamically
// Uses port as the key, allowing multiple listeners per port
type ListenerManager struct {
	ctx              context.Context
	router           *router.Router
	store            datastore.Store
	server           *Server
	mu               sync.RWMutex
	portListeners    map[int32]*PortListenerInfo // key: port
	gatewayListeners map[string][]ListenerConfig // key: gatewayKey, tracks listeners per gateway
}

// NewListenerManager creates a new listener manager
func NewListenerManager(ctx context.Context, router *router.Router, store datastore.Store, server *Server) *ListenerManager {
	return &ListenerManager{
		ctx:              ctx,
		router:           router,
		store:            store,
		server:           server,
		portListeners:    make(map[int32]*PortListenerInfo),
		gatewayListeners: make(map[string][]ListenerConfig),
	}
}

// findBestMatchingListener finds the best matching listener for a request
// Returns the listener config and true if found, nil and false otherwise
// NOTE: Caller must hold lm.mu lock
func (lm *ListenerManager) findBestMatchingListener(port int32, hostname string) (*ListenerConfig, bool) {
	lm.mu.RLock()
	portInfo, exists := lm.portListeners[port]
	if !exists {
		lm.mu.RUnlock()
		return nil, false
	}
	lm.mu.RUnlock()

	portInfo.mu.RLock()
	defer portInfo.mu.RUnlock()

	if len(portInfo.Listeners) == 0 {
		return nil, false
	}

	// First, try to find an exact hostname match
	for i := range portInfo.Listeners {
		listener := &portInfo.Listeners[i]
		if listener.Hostname != nil && *listener.Hostname == hostname {
			return listener, true
		}
	}

	// If no exact match, try to find a listener without hostname restriction (wildcard)
	// TODO: support wildcard hostname matching
	for i := range portInfo.Listeners {
		listener := &portInfo.Listeners[i]
		if listener.Hostname == nil {
			return listener, true
		}
	}

	// No match found
	return nil, false
}

// createPortHandler creates a gin handler for a specific port that routes to the best matching listener
func (lm *ListenerManager) createPortHandler(port int32) gin.HandlerFunc {
	return func(c *gin.Context) {
		if strconv.Itoa(int(port)) == lm.server.Port {
			// Handle management endpoints first (healthz, readyz, metrics)
			path := c.Request.URL.Path
			if path == "/healthz" {
				c.JSON(http.StatusOK, gin.H{
					"message": "ok",
				})
				return
			}
			if path == "/readyz" {
				if lm.server.HasSynced() {
					c.JSON(http.StatusOK, gin.H{
						"message": "router is ready",
					})
				} else {
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"message": "router is not ready",
					})
				}
				return
			}
			if path == "/metrics" {
				promhttp.Handler().ServeHTTP(c.Writer, c.Request)
				return
			}
		}

		hostname := c.Request.Host
		// Remove port from hostname if present
		if idx := strings.Index(hostname, ":"); idx != -1 {
			hostname = hostname[:idx]
		}

		listenerConfig, found := lm.findBestMatchingListener(port, hostname)
		if !found {
			c.JSON(http.StatusNotFound, gin.H{
				"message": "No matching listener found",
			})
			return
		}

		// Set gateway key in context so router can filter ModelRoutes by gateway
		c.Set(router.GatewayKey, listenerConfig.GatewayKey)

		// Apply middleware and route
		AccessLogMiddleware(lm.router)(c)
		if c.IsAborted() {
			return
		}

		AuthMiddleware(lm.router)(c)
		if c.IsAborted() {
			return
		}

		// Route handling logic is now in router.HandlerFunc()
		// It will handle both /v1/* paths (ModelRoute with HTTPRoute fallback) and non-/v1/* paths (HTTPRoute)
		lm.router.HandlerFunc()(c)
	}
}

// listenerConfigKey creates a unique key for a listener config for comparison
func (c *ListenerConfig) listenerConfigKey() string {
	hostnameStr := ""
	if c.Hostname != nil {
		hostnameStr = *c.Hostname
	}
	return fmt.Sprintf("%s:%s:%d:%s:%s", c.GatewayKey, c.ListenerName, c.Port, hostnameStr, c.Protocol)
}

// buildListenerConfigsFromGateway builds listener configs from a Gateway spec
func buildListenerConfigsFromGateway(gateway *gatewayv1.Gateway) []ListenerConfig {
	gatewayKey := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)
	var configs []ListenerConfig

	for _, listener := range gateway.Spec.Listeners {
		protocol := string(listener.Protocol)

		// Only support HTTP for now
		if protocol != string(gatewayv1.HTTPProtocolType) {
			klog.Errorf("Unsupported protocol %s for listener %s/%s, only HTTP is supported", protocol, gatewayKey, listener.Name)
			continue
		}

		var hostname *string
		if listener.Hostname != nil && *listener.Hostname != "" {
			hostnameStr := string(*listener.Hostname)
			hostname = &hostnameStr
		}

		config := ListenerConfig{
			GatewayKey:   gatewayKey,
			ListenerName: string(listener.Name),
			Port:         int32(listener.Port),
			Hostname:     hostname,
			Protocol:     protocol,
		}

		configs = append(configs, config)
	}

	return configs
}

// removeListenerFromPort removes a specific listener config from a port
// NOTE: Caller must hold lm.mu lock
func (lm *ListenerManager) removeListenerFromPort(port int32, configToRemove ListenerConfig) {
	portInfo, exists := lm.portListeners[port]
	if !exists {
		return
	}

	portInfo.mu.Lock()
	filtered := portInfo.Listeners[:0]
	for i := range portInfo.Listeners {
		existing := &portInfo.Listeners[i]
		if existing.GatewayKey != configToRemove.GatewayKey || existing.ListenerName != configToRemove.ListenerName {
			filtered = append(filtered, portInfo.Listeners[i])
		}
	}
	portInfo.Listeners = filtered
	portInfo.mu.Unlock()

	// Remove listener status from store to prevent memory leaks
	lm.store.RemoveListenerStatus(configToRemove.GatewayKey, configToRemove.ListenerName)
}

// addListenerToPort adds a listener config to a port
// NOTE: Caller must hold lm.mu lock
func (lm *ListenerManager) addListenerToPort(port int32, config ListenerConfig, enableTLS bool, tlsCertFile, tlsKeyFile string) {
	portInfo, exists := lm.portListeners[port]
	if !exists {
		// Create new port listener
		engine := gin.New()
		engine.Use(gin.Recovery())
		engine.Any("/*path", lm.createPortHandler(port))

		server := &http.Server{
			Addr:    ":" + strconv.Itoa(int(port)),
			Handler: engine.Handler(),
		}

		portInfo = &PortListenerInfo{
			Server:    server,
			Listeners: []ListenerConfig{config},
		}
		lm.portListeners[port] = portInfo

		// Create context for this port's server
		listenerCtx, cancel := context.WithCancel(lm.ctx)
		portInfo.ShutdownFunc = cancel

		// Start the server
		// Set status to nil to indicate a clean state (no errors detected yet)
		// This will translate to Accepted: True and Programmed: True in the Gateway status
		lm.store.SetListenerStatus(config.GatewayKey, config.ListenerName, nil)

		go func(p int32, srv *http.Server, pi *PortListenerInfo, tls bool, cert, key string) {
			klog.Infof("Starting Gateway listener server on port %d", p)
			var err error
			if tls {
				if cert == "" || key == "" {
					klog.Fatalf("TLS enabled but cert or key file not specified for port %d", p)
				}
				err = srv.ListenAndServeTLS(cert, key)
			} else {
				err = srv.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				klog.Errorf("listen failed for port %d: %v", p, err)

				// Update all listeners on this port with the error
				// pi is a pointer to the PortListenerInfo, so we can lock it directly
				// without needing to lock the top-level ListenerManager
				pi.mu.Lock()
				pi.LastError = err
				for _, l := range pi.Listeners {
					lm.store.SetListenerStatus(l.GatewayKey, l.ListenerName, err)
				}
				pi.mu.Unlock()
			}
		}(port, server, portInfo, enableTLS, tlsCertFile, tlsKeyFile)

		// Start graceful shutdown goroutine
		go func(p int32, srv *http.Server, cancel context.CancelFunc) {
			<-listenerCtx.Done()
			klog.Infof("Shutting down Gateway listener server on port %d ...", p)
			shutdownCtx, cancelTimeout := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
			defer cancelTimeout()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				klog.Errorf("Gateway listener server on port %d shutdown failed: %v", p, err)
			}
		}(port, server, cancel)
	} else {
		// Add listener to existing port
		portInfo.mu.Lock()
		portInfo.Listeners = append(portInfo.Listeners, config)
		currentErr := portInfo.LastError
		portInfo.mu.Unlock()

		lm.store.SetListenerStatus(config.GatewayKey, config.ListenerName, currentErr)
		klog.V(4).Infof("Added listener %s/%s to existing port %d", config.GatewayKey, config.ListenerName, port)
	}
}

// StartListenersForGateway starts listeners for a Gateway, only processing delta changes
func (lm *ListenerManager) StartListenersForGateway(gateway *gatewayv1.Gateway) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	gatewayKey := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)

	// Get old listener configs
	oldConfigs := lm.gatewayListeners[gatewayKey]

	newConfigs := buildListenerConfigsFromGateway(gateway)
	// Build maps for efficient comparison
	oldConfigMap := make(map[string]ListenerConfig)
	for _, config := range oldConfigs {
		key := config.listenerConfigKey()
		oldConfigMap[key] = config
	}
	newConfigMap := make(map[string]ListenerConfig)
	for _, config := range newConfigs {
		key := config.listenerConfigKey()
		newConfigMap[key] = config
	}

	// Find listeners to remove (in old but not in new)
	for key, config := range oldConfigMap {
		if _, exists := newConfigMap[key]; !exists {
			lm.removeListenerFromPort(config.Port, config)
			lm.checkAndClosePortIfEmpty(config.Port)
		}
	}

	// Find listeners to add (in new but not in old)
	for key, config := range newConfigMap {
		if _, exists := oldConfigMap[key]; !exists {
			// Check if this is the default port to determine TLS settings
			defaultPort, _ := strconv.Atoi(lm.server.Port)
			enableTLS := false
			tlsCertFile := ""
			tlsKeyFile := ""
			if int32(defaultPort) == config.Port {
				enableTLS = lm.server.EnableTLS
				tlsCertFile = lm.server.TLSCertFile
				tlsKeyFile = lm.server.TLSKeyFile
			}
			lm.addListenerToPort(config.Port, config, enableTLS, tlsCertFile, tlsKeyFile)
		}
	}

	// Update gateway listeners map
	lm.gatewayListeners[gatewayKey] = newConfigs
}

// StopListenersForGateway stops all listeners for a Gateway
// Only closes the port server if no listeners remain on that port
func (lm *ListenerManager) StopListenersForGateway(gatewayKey string) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	_, exists := lm.gatewayListeners[gatewayKey]
	if !exists {
		return
	}
	delete(lm.gatewayListeners, gatewayKey)

	// Build map of ports that might need checking
	portsToCheck := make(map[int32]bool)

	// Remove listeners for this gateway from all ports
	portInfos := make(map[int32]*PortListenerInfo)
	for port, portInfo := range lm.portListeners {
		portInfos[port] = portInfo
	}

	for port, portInfo := range portInfos {
		portInfo.mu.Lock()
		originalCount := len(portInfo.Listeners)
		// Filter out listeners belonging to this gateway
		filtered := portInfo.Listeners[:0]
		for i := range portInfo.Listeners {
			if portInfo.Listeners[i].GatewayKey != gatewayKey {
				filtered = append(filtered, portInfo.Listeners[i])
			}
		}
		portInfo.Listeners = filtered
		newCount := len(portInfo.Listeners)
		portInfo.mu.Unlock()

		// If listeners were removed, mark port for checking
		if newCount < originalCount {
			portsToCheck[port] = true
		}
	}

	// Check if any ports need to be closed (no listeners remaining)
	for port := range portsToCheck {
		lm.checkAndClosePortIfEmpty(port)
	}
}

// checkAndClosePortIfEmpty checks if a port has no listeners and closes it if empty
// NOTE: Caller must hold lm.mu lock
func (lm *ListenerManager) checkAndClosePortIfEmpty(port int32) {
	portInfo, exists := lm.portListeners[port]
	if !exists {
		return
	}

	portInfo.mu.RLock()
	defer portInfo.mu.RUnlock()

	if len(portInfo.Listeners) == 0 {
		// No listeners left on this port, close the server
		delete(lm.portListeners, port)
		klog.Infof("No listeners remaining on port %d, shutting down server", port)
		portInfo.ShutdownFunc()
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
