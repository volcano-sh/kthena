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

package tokenizer

import (
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	kthenainformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	networkinglisters "github.com/volcano-sh/kthena/client-go/listers/networking/v1alpha1"
)

// ModelServerWatcher keeps the renderer manager's model set in sync with the
// ModelServer objects in the cluster: creating a ModelServer pre-warms the
// tokenizer for its spec.model, deleting the last ModelServer that references
// a model unloads it.
type ModelServerWatcher struct {
	factory kthenainformers.SharedInformerFactory
	lister  networkinglisters.ModelServerLister
	synced  cache.InformerSynced
	manager *RendererManager
}

func NewModelServerWatcher(client clientset.Interface, cfg Config, manager *RendererManager) (*ModelServerWatcher, error) {
	opts := []kthenainformers.SharedInformerOption{}
	if cfg.WatchNamespace != "" {
		opts = append(opts, kthenainformers.WithNamespace(cfg.WatchNamespace))
	}
	factory := kthenainformers.NewSharedInformerFactoryWithOptions(client, cfg.ResyncPeriod, opts...)
	informer := factory.Networking().V1alpha1().ModelServers()

	w := &ModelServerWatcher{
		factory: factory,
		lister:  informer.Lister(),
		synced:  informer.Informer().HasSynced,
		manager: manager,
	}
	// Any change may add or remove a served model, so recompute the full
	// desired set from the lister; the manager reconciles idempotently.
	_, err := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { w.sync() },
		UpdateFunc: func(interface{}, interface{}) { w.sync() },
		DeleteFunc: func(interface{}) { w.sync() },
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}

// Start runs the informer until stopCh is closed and waits for the initial
// sync.
func (w *ModelServerWatcher) Start(stopCh <-chan struct{}) error {
	w.factory.Start(stopCh)
	if !cache.WaitForCacheSync(stopCh, w.synced) {
		klog.Error("Failed to sync ModelServer informer cache")
		return nil
	}
	w.sync()
	return nil
}

// HasSynced reports whether the initial ModelServer list has completed.
func (w *ModelServerWatcher) HasSynced() bool {
	return w.synced()
}

func (w *ModelServerWatcher) sync() {
	servers, err := w.lister.List(labels.Everything())
	if err != nil {
		klog.Errorf("Failed to list ModelServers: %v", err)
		return
	}
	desired := make(map[string]struct{})
	for _, ms := range servers {
		if ms.Spec.Model != nil && *ms.Spec.Model != "" {
			desired[*ms.Spec.Model] = struct{}{}
		}
	}
	w.manager.SetModels(desired)
}
