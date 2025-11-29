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

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type Server struct {
	store            datastore.Store
	controllers      Controller
	listenerManager  *ListenerManager
	EnableTLS        bool
	TLSCertFile      string
	TLSKeyFile       string
	Port             string
	EnableGatewayAPI bool
}

func NewServer(port string, enableTLS bool, cert, key string, enableGatewayAPI bool) *Server {
	return &Server{
		store:            nil,
		EnableTLS:        enableTLS,
		TLSCertFile:      cert,
		TLSKeyFile:       key,
		Port:             port,
		EnableGatewayAPI: enableGatewayAPI,
	}
}

func (s *Server) Run(ctx context.Context) {
	// create store
	store := datastore.New()
	s.store = store

	// must be run before the controller, because it will register callbacks
	r := NewRouter(store)
	// start controller
	s.controllers = startControllers(store, ctx.Done(), s.EnableGatewayAPI, s.Port)

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
