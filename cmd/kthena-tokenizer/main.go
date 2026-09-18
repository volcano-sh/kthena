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

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	tokenizer "github.com/volcano-sh/kthena/pkg/kthena-tokenizer"
	"github.com/volcano-sh/kthena/pkg/kube"
)

func main() {
	klog.InitFlags(nil)
	defer klog.Flush()

	cfg := tokenizer.ConfigFromEnv()

	restConfig, err := kube.BuildConfig("", "")
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %v", err)
	}
	kthenaClient, err := clientset.NewForConfig(restConfig)
	if err != nil {
		klog.Fatalf("Error building kthena client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		klog.Info("Shutting down tokenizer service")
		cancel()
	}()

	manager := tokenizer.NewRendererManager(cfg)
	go manager.Start(ctx)

	watcher, err := tokenizer.NewModelServerWatcher(kthenaClient, cfg, manager)
	if err != nil {
		klog.Fatalf("Error creating ModelServer watcher: %v", err)
	}
	go func() {
		if err := watcher.Start(ctx.Done()); err != nil {
			klog.Fatalf("Error starting ModelServer watcher: %v", err)
		}
	}()

	klog.Infof("Tokenizer service listening on %s:%d", cfg.Host, cfg.Port)
	if err := tokenizer.NewServer(cfg, manager, watcher).Run(ctx); err != nil {
		klog.Fatalf("Tokenizer service stopped: %v", err)
	}
	// Give renderer subprocesses a moment to terminate gracefully.
	cancel()
	time.Sleep(time.Second)
}
