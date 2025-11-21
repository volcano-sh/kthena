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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	kthenaInformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	"github.com/volcano-sh/kthena/pkg/kthena-router/controller"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

type Controller interface {
	HasSynced() bool
}

type aggregatedController struct {
	controllers []Controller
}

var _ Controller = &aggregatedController{}

func startControllers(store datastore.Store, stop <-chan struct{}) Controller {
	cfg, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	kthenaClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kthena clientset: %s", err.Error())
	}

	gatewayClient, err := gatewayclientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building gateway clientset: %s", err.Error())
	}

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := kthenaInformers.NewSharedInformerFactory(kthenaClient, 0)
	gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)

	modelRouteController := controller.NewModelRouteController(kthenaInformerFactory, store)
	modelServerController := controller.NewModelServerController(kthenaInformerFactory, kubeInformerFactory, store)
	gatewayController := controller.NewGatewayController(gatewayInformerFactory, kubeClient, store)
	gatewayClassController := controller.NewGatewayClassController(gatewayClient, store)

	kubeInformerFactory.Start(stop)
	kthenaInformerFactory.Start(stop)
	gatewayInformerFactory.Start(stop)

	go func() {
		if err := modelRouteController.Run(stop); err != nil {
			klog.Fatalf("Error running model route controller: %s", err.Error())
		}
	}()

	go func() {
		if err := modelServerController.Run(stop); err != nil {
			klog.Fatalf("Error running model server controller: %s", err.Error())
		}
	}()

	go func() {
		if err := gatewayController.Run(stop); err != nil {
			klog.Fatalf("Error running gateway controller: %s", err.Error())
		}
	}()

	go func() {
		if err := gatewayClassController.Run(stop); err != nil {
			klog.Fatalf("Error running gateway class controller: %s", err.Error())
		}
	}()

	return &aggregatedController{
		controllers: []Controller{
			modelRouteController,
			modelServerController,
			gatewayController,
			gatewayClassController,
		},
	}
}

func (c *aggregatedController) HasSynced() bool {
	for _, controller := range c.controllers {
		if !controller.HasSynced() {
			return false
		}
	}
	return true
}
