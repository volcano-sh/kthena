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

// This standalone example registers an HTTP provider without changing the
// default router binary. Run from the repository root; see the provider guide.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/volcano-sh/kthena/cmd/kthena-router/app"
	"github.com/volcano-sh/kthena/pkg/kthena-router/discovery"
)

func main() {
	const base = "examples/kthena-router/endpoint-discovery/"
	config := flag.String("router-config", base+"router.yaml", "Router configuration file")
	resources := flag.String("config-dir", base+"resources", "Resource manifest directory")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := app.NewServer("18080", false, "", "", false, false, 15000, 19090, false, 0, 0)
	server.ConfigSource = app.ConfigSourceFile
	server.ConfigDir = *resources
	server.RouterConfigFile = *config
	server.EndpointDiscoveryRegistry = discovery.Registry{"http-json": newHTTPSource}
	server.Run(ctx)
}
