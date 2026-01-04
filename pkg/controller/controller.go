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

package controller

import (
	"context"
	"os"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	autoscaler "github.com/volcano-sh/kthena/pkg/autoscaler/controller"
	modelbooster "github.com/volcano-sh/kthena/pkg/model-booster-controller/controller"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	modelserving "github.com/volcano-sh/kthena/pkg/model-serving-controller/controller"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	volcanoClientSet "volcano.sh/apis/pkg/client/clientset/versioned"
)

const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
	leaderElectionId     = "kthena.controller-manager"
	leaseName            = "lease.kthena.controller-manager"

	ModelServingController = "modelserving"
	ModelBoosterController = "modelbooster"
	AutoscalerController   = "autoscaler"
)

func SetupController(ctx context.Context, cc Config) {
	config, err := clientcmd.BuildConfigFromFlags(cc.MasterURL, cc.Kubeconfig)
	if err != nil {
		klog.Fatalf("build client config: %v", err)
	}
	kubeClient := kubernetes.NewForConfigOrDie(config)
	client := clientset.NewForConfigOrDie(config)
	volcanoClient, err := volcanoClientSet.NewForConfig(config)
	if err != nil {
		klog.Fatalf("failed to create volcano client: %v", err)
	}
	apiextClient, err := apiextclient.NewForConfig(config)
	if err != nil {
		klog.Fatalf("failed to create apiext client: %v", err)
	}

	var mc *modelbooster.ModelBoosterController
	var msc *modelserving.ModelServingController
	var ac *autoscaler.AutoscaleController

	for ctrl, enable := range cc.Controllers {
		if enable {
			switch ctrl {
			case ModelBoosterController:
				mc = modelbooster.NewModelBoosterController(kubeClient, client)
			case ModelServingController:
				msc, err = modelserving.NewModelServingController(kubeClient, client, volcanoClient, apiextClient)
				if err != nil {
					klog.Fatalf("failed to create ModelServing controller: %v", err)
				}
			case AutoscalerController:
				namespace, err := utils.GetInClusterNameSpace()
				if err != nil {
					klog.Fatalf("failed to get in-cluster namespace: %v", err)
				}
				ac = autoscaler.NewAutoscaleController(kubeClient, client, namespace)
			}
		}
	}

	startControllers := func(ctx context.Context) {
		if mc != nil {
			go mc.Run(ctx, cc.Workers)
			klog.Info("ModelBooster controller started")
		}
		if msc != nil {
			go msc.Run(ctx, cc.Workers)
			klog.Info("ModelServing controller started")
		}
		if ac != nil {
			go ac.Run(ctx)
			klog.Info("Autoscaler controller started")
		}
	}

	if cc.EnableLeaderElection {
		startedLeading := func(ctx context.Context) {
			startControllers(ctx)
			klog.Info("Start as leader")
		}
		leaderElector, err := initLeaderElector(kubeClient, startedLeading)
		if err != nil {
			panic(err)
		}
		leaderElector.Run(ctx)
	} else {
		startControllers(ctx)
		klog.Info("Started controllers without leader election")
	}
	<-ctx.Done()
}

// initLeaderElector inits a leader elector for leader election
func initLeaderElector(kubeClient kubernetes.Interface, startedLeading func(ctx context.Context)) (*leaderelection.LeaderElector, error) {
	resourceLock, err := newResourceLock(kubeClient)
	if err != nil {
		return nil, err
	}
	leaderElector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          resourceLock,
		LeaseDuration: defaultLeaseDuration,
		RenewDeadline: defaultRenewDeadline,
		RetryPeriod:   defaultRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: startedLeading,
			OnStoppedLeading: func() {
				klog.Error("leader election lost")
			},
		},
		ReleaseOnCancel: false,
		Name:            leaderElectionId,
	})
	if err != nil {
		return nil, err
	}
	return leaderElector, nil
}

// newResourceLock returns a lease lock which is used to elect leader
func newResourceLock(client kubernetes.Interface) (*resourcelock.LeaseLock, error) {
	namespace, err := utils.GetInClusterNameSpace()
	if err != nil {
		return nil, err
	}
	// Leader id, should be unique
	id, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	id = id + "_" + string(uuid.NewUUID())
	return &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}, nil
}
