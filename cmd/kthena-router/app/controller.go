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
	"os"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
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

func startControllers(store datastore.Store, stop <-chan struct{}, enableGatewayAPI bool, defaultPort string, enableGatewayAPIInferenceExtension bool, kubeAPIQPS float32, kubeAPIBurst int) Controller {
	cfg, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}
	// Set QPS and Burst if provided
	if kubeAPIQPS > 0 {
		cfg.QPS = kubeAPIQPS
	}
	if kubeAPIBurst > 0 {
		cfg.Burst = kubeAPIBurst
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	kthenaClient, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Error building kthena clientset: %s", err.Error())
	}

	kubeInformerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	kthenaInformerFactory := kthenaInformers.NewSharedInformerFactory(kthenaClient, 0)

	modelRouteController := controller.NewModelRouteController(kthenaInformerFactory, store)
	modelServerController := controller.NewModelServerController(kthenaInformerFactory, kubeInformerFactory, store)

	kubeInformerFactory.Start(stop)
	kthenaInformerFactory.Start(stop)

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

	controllers := []Controller{
		modelRouteController,
		modelServerController,
	}

	// Gateway API controllers are optional
	if enableGatewayAPI {
		gatewayClient, err := gatewayclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building gateway clientset: %s", err.Error())
		}

		// Ensure default GatewayClass exists before starting controllers
		if err := ensureDefaultGatewayClass(gatewayClient); err != nil {
			klog.Fatalf("Failed to ensure default GatewayClass: %s", err.Error())
		}

		// Ensure default Gateway exists before starting controllers
		if err := ensureDefaultGateway(gatewayClient, defaultPort); err != nil {
			klog.Fatalf("Failed to ensure default Gateway: %s", err.Error())
		}

		gatewayInformerFactory := gatewayinformers.NewSharedInformerFactory(gatewayClient, 0)
		gatewayController := controller.NewGatewayController(gatewayClient, gatewayInformerFactory, store)

		// Gateway API Inference Extension controllers are optional
		var httpRouteController *controller.HTTPRouteController
		if enableGatewayAPIInferenceExtension {
			httpRouteController = controller.NewHTTPRouteController(gatewayClient, gatewayInformerFactory, store)
		}

		// Start informer factory after all controllers that use it are created
		gatewayInformerFactory.Start(stop)

		go func() {
			if err := gatewayController.Run(stop); err != nil {
				klog.Fatalf("Error running gateway controller: %s", err.Error())
			}
		}()

		controllers = append(controllers, gatewayController)

		// Gateway API Inference Extension controllers are optional
		if enableGatewayAPIInferenceExtension {
			dynamicClient, err := dynamic.NewForConfig(cfg)
			if err != nil {
				klog.Fatalf("Error building dynamic client: %s", err.Error())
			}
			dynamicInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, 0)
			inferencePoolController := controller.NewInferencePoolController(dynamicClient, dynamicInformerFactory, store)

			dynamicInformerFactory.Start(stop)

			go func() {
				if err := httpRouteController.Run(stop); err != nil {
					klog.Fatalf("Error running httproute controller: %s", err.Error())
				}
			}()

			go func() {
				if err := inferencePoolController.Run(stop); err != nil {
					klog.Fatalf("Error running inferencepool controller: %s", err.Error())
				}
			}()

			controllers = append(controllers, httpRouteController, inferencePoolController)
		} else {
			klog.Info("Gateway API Inference Extension controllers are disabled")
		}
	} else {
		klog.Info("Gateway API controllers are disabled")
	}

	return &aggregatedController{
		controllers: controllers,
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

// ensureDefaultGatewayClass creates the default GatewayClass if it doesn't exist
func ensureDefaultGatewayClass(gatewayClient gatewayclientset.Interface) error {
	ctx := context.Background()

	// Check if GatewayClass already exists
	_, err := gatewayClient.GatewayV1().GatewayClasses().Get(ctx, controller.DefaultGatewayClassName, metav1.GetOptions{})
	if err == nil {
		klog.V(2).Infof("Default GatewayClass %s already exists", controller.DefaultGatewayClassName)
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check GatewayClass %s: %w", controller.DefaultGatewayClassName, err)
	}

	// Create the default GatewayClass
	gatewayClass := &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: controller.DefaultGatewayClassName,
		},
		Spec: gatewayv1.GatewayClassSpec{
			ControllerName: gatewayv1.GatewayController(controller.ControllerName),
		},
	}

	_, err = gatewayClient.GatewayV1().GatewayClasses().Create(ctx, gatewayClass, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(2).Infof("GatewayClass %s was created by another process", controller.DefaultGatewayClassName)
			return nil
		}
		return fmt.Errorf("failed to create GatewayClass %s: %w", controller.DefaultGatewayClassName, err)
	}

	klog.Infof("Created default GatewayClass %s", controller.DefaultGatewayClassName)
	return nil
}

// ensureDefaultGateway creates the default Gateway if it doesn't exist
func ensureDefaultGateway(gatewayClient gatewayclientset.Interface, defaultPort string) error {
	ctx := context.Background()
	namespace := "default"
	name := "default"

	// Get namespace from environment variable if available, otherwise use "default"
	if podNamespace := os.Getenv("POD_NAMESPACE"); podNamespace != "" {
		namespace = podNamespace
	}

	// Parse port
	port, err := strconv.Atoi(defaultPort)
	if err != nil {
		return fmt.Errorf("invalid default port %s: %w", defaultPort, err)
	}

	// Check if Gateway already exists
	_, err = gatewayClient.GatewayV1().Gateways(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		klog.V(2).Infof("Default Gateway %s/%s already exists", namespace, name)
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check Gateway %s/%s: %w", namespace, name, err)
	}

	// Create the default Gateway
	gateway := &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: gatewayv1.ObjectName(controller.DefaultGatewayClassName),
			Listeners: []gatewayv1.Listener{
				{
					Name:     gatewayv1.SectionName("default"),
					Port:     gatewayv1.PortNumber(port),
					Protocol: gatewayv1.HTTPProtocolType,
					// Hostname is nil, meaning match all hostnames
				},
			},
		},
	}

	_, err = gatewayClient.GatewayV1().Gateways(namespace).Create(ctx, gateway, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			klog.V(2).Infof("Gateway %s/%s was created by another process", namespace, name)
			return nil
		}
		return fmt.Errorf("failed to create Gateway %s/%s: %w", namespace, name, err)
	}

	klog.Infof("Created default Gateway %s/%s", namespace, name)
	return nil
}
