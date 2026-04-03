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

func writeHealthzJSON(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "ok",
	})
}

func writeReadyzJSON(c *gin.Context, synced bool) {
	if synced {
		c.JSON(http.StatusOK, gin.H{
			"message": "router is ready",
		})
	} else {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"message": "router is not ready",
		})
	}
}

// listenerGatewayConfig registers Recovery, gateway routing (management + hostname match), AccessLog, Auth, and catch-all.
// When non-nil, startListener applies this stack instead of listenerConfig.setup.
type listenerGatewayConfig struct {
	lm   *ListenerManager
	port int32
}

// listenerConfig configures startListener (shared by default server and Gateway port listeners).
type listenerConfig struct {
	addr          string
	enableTLS     bool
	tlsCertFile   string
	tlsKeyFile    string
	tlsMissingMsg string
	// Used when gateway is nil (standalone default server).
	setup func(*gin.Engine)
	// When non-nil, registers Gateway listener middleware and routes; setup is ignored.
	gateway  *listenerGatewayConfig
	startLog string
	// Shutdown
	shutdownStartLog string
	shutdownDoneLog  string
	logShutdownErr   func(err error)
	// Listen errors (after ErrServerClosed filtered out)
	logListenErr func(err error)
}

// startListener builds the Gin engine via setup, starts the HTTP server, and shuts down when ctx is done.
func startListener(ctx context.Context, cfg listenerConfig) *http.Server {
	engine := gin.New()
	if g := cfg.gateway; g != nil {
		lm := g.lm
		port := g.port
		engine.Use(gin.Recovery())
		engine.Use(func(c *gin.Context) {
			if strconv.Itoa(int(port)) == lm.server.Port {
				path := c.Request.URL.Path
				if path == "/healthz" {
					writeHealthzJSON(c)
					c.Abort()
					return
				}
				if path == "/readyz" {
					writeReadyzJSON(c, lm.server.HasSynced())
					c.Abort()
					return
				}
				if path == "/metrics" {
					promhttp.Handler().ServeHTTP(c.Writer, c.Request)
					c.Abort()
					return
				}
			}

			hostname := c.Request.Host
			if idx := strings.Index(hostname, ":"); idx != -1 {
				hostname = hostname[:idx]
			}

			matched, found := lm.findBestMatchingListener(port, hostname)
			if !found {
				c.JSON(http.StatusNotFound, gin.H{
					"message": "No matching listener found",
				})
				c.Abort()
				return
			}

			c.Set(router.GatewayKey, matched.GatewayKey)
			c.Next()
		})
		engine.Use(AccessLogMiddleware(lm.router))
		engine.Use(AuthMiddleware(lm.router))
		engine.Any("/*path", lm.router.HandlerFunc())
	} else {
		cfg.setup(engine)
	}
	srv := &http.Server{
		Addr:    cfg.addr,
		Handler: engine.Handler(),
	}

	go func() {
		klog.Info(cfg.startLog)
		if cfg.enableTLS {
			if cfg.tlsCertFile == "" || cfg.tlsKeyFile == "" {
				msg := cfg.tlsMissingMsg
				if msg == "" {
					msg = "TLS enabled but cert or key file not specified"
				}
				klog.Fatal(msg)
			}
			if err := srv.ListenAndServeTLS(cfg.tlsCertFile, cfg.tlsKeyFile); err != nil && err != http.ErrServerClosed {
				cfg.logListenErr(err)
			}
			return
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			cfg.logListenErr(err)
		}
	}()

	go func() {
		<-ctx.Done()
		klog.Info(cfg.shutdownStartLog)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			cfg.logShutdownErr(err)
		}
		if cfg.shutdownDoneLog != "" {
			klog.Info(cfg.shutdownDoneLog)
		}
	}()

	return srv
}

// startDefaultServer starts the default HTTP server on fixed port
// This server handles healthz, readyz, metrics, and /v1/*path
func (s *Server) startDefaultServer(ctx context.Context, router *router.Router, store datastore.Store) {
	_ = store
	startListener(ctx, listenerConfig{
		addr:          ":" + s.Port,
		enableTLS:     s.EnableTLS,
		tlsCertFile:   s.TLSCertFile,
		tlsKeyFile:    s.TLSKeyFile,
		tlsMissingMsg: "",
		setup: func(engine *gin.Engine) {
			engine.Use(gin.LoggerWithWriter(gin.DefaultWriter, "/healthz", "/readyz", "/metrics"), gin.Recovery())
			engine.GET("/healthz", func(c *gin.Context) {
				writeHealthzJSON(c)
			})
			engine.GET("/readyz", func(c *gin.Context) {
				writeReadyzJSON(c, s.HasSynced())
			})
			engine.GET("/metrics", gin.WrapH(promhttp.Handler()))
			v1Group := engine.Group("/v1")
			v1Group.Use(AccessLogMiddleware(router))
			v1Group.Use(AuthMiddleware(router))
			v1Group.Any("/*path", router.HandlerFunc())
		},
		startLog:         fmt.Sprintf("Starting default server on port %s", s.Port),
		shutdownStartLog: "Shutting down default HTTP server ...",
		shutdownDoneLog:  "Default HTTP server exited",
		logShutdownErr: func(err error) {
			klog.Errorf("Default server shutdown failed: %v", err)
		},
		logListenErr: func(err error) {
			klog.Fatalf("listen failed: %v", err)
		},
	})
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
		if listener.Hostname != nil && strings.EqualFold(*listener.Hostname, hostname) {
			return listener, true
		}
	}

	// Then, try wildcard hostname matches and prefer the most specific suffix.
	var wildcardMatch *ListenerConfig
	longestPattern := -1
	for i := range portInfo.Listeners {
		listener := &portInfo.Listeners[i]
		if listener.Hostname != nil && wildcardHostnameMatch(*listener.Hostname, hostname) {
			if l := len(*listener.Hostname); l > longestPattern {
				wildcardMatch = listener
				longestPattern = l
			}
		}
	}
	if wildcardMatch != nil {
		return wildcardMatch, true
	}

	// If no exact/wildcard match, try a listener without hostname restriction.
	for i := range portInfo.Listeners {
		listener := &portInfo.Listeners[i]
		if listener.Hostname == nil {
			return listener, true
		}
	}

	// No match found
	return nil, false
}

func wildcardHostnameMatch(pattern, hostname string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}

	patternLower := strings.ToLower(pattern)
	hostnameLower := strings.ToLower(hostname)
	suffix := patternLower[1:] // ".example.com"
	if !strings.HasSuffix(hostnameLower, suffix) {
		return false
	}

	prefix := hostnameLower[:len(hostnameLower)-len(suffix)]
	// Gateway wildcard hostnames represent exactly one leading label.
	return prefix != "" && !strings.Contains(prefix, ".")
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
}

// addListenerToPort adds a listener config to a port
// NOTE: Caller must hold lm.mu lock
func (lm *ListenerManager) addListenerToPort(port int32, config ListenerConfig, enableTLS bool, tlsCertFile, tlsKeyFile string) {
	portInfo, exists := lm.portListeners[port]
	if !exists {
		// Create new port listener — middleware order must match startDefaultServer so AccessLog's
		// c.Next() runs Auth and HandlerFunc before the access log line is written.
		listenerCtx, cancel := context.WithCancel(lm.ctx)

		server := startListener(listenerCtx, listenerConfig{
			addr:             ":" + strconv.Itoa(int(port)),
			enableTLS:        enableTLS,
			tlsCertFile:      tlsCertFile,
			tlsKeyFile:       tlsKeyFile,
			tlsMissingMsg:    fmt.Sprintf("TLS enabled but cert or key file not specified for port %d", port),
			gateway:          &listenerGatewayConfig{lm: lm, port: port},
			startLog:         fmt.Sprintf("Starting Gateway listener server on port %d", port),
			shutdownStartLog: fmt.Sprintf("Shutting down Gateway listener server on port %d ...", port),
			shutdownDoneLog:  "",
			logShutdownErr: func(err error) {
				klog.Errorf("Gateway listener server on port %d shutdown failed: %v", port, err)
			},
			logListenErr: func(err error) {
				klog.Errorf("listen failed for port %d: %v", port, err)
			},
		})

		portInfo = &PortListenerInfo{
			Server:    server,
			Listeners: []ListenerConfig{config},
		}
		lm.portListeners[port] = portInfo
		portInfo.ShutdownFunc = cancel
	} else {
		// Add listener to existing port
		portInfo.mu.Lock()
		portInfo.Listeners = append(portInfo.Listeners, config)
		portInfo.mu.Unlock()
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
