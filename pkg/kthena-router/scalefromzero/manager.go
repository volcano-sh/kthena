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

package scalefromzero

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	listerv1alpha1 "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	networkingv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/datastore"
)

var (
	ErrNotConfigured = errors.New("scale from zero not configured for this model server")
	ErrNotEnabled    = errors.New("scale from zero is disabled")
	ErrTimeout       = errors.New("scale from zero timeout")
	ErrTriggerFailed = errors.New("failed to trigger scale from zero")
)

const (
	defaultScaleFromZeroTimeout = 5 * time.Minute
	defaultBindingCacheTTL      = 2 * time.Minute
)

// Manager handles scale-from-zero for ModelServers that have zero pods.
type Manager struct {
	enabled bool

	kubeClient    clientset.Interface
	bindingLister listerv1alpha1.AutoscalingPolicyBindingLister

	// Runtime mapping: ModelServer key ("namespace/name") -> ModelServing key ("namespace/name")
	mu           sync.RWMutex
	servingForMS map[string]string

	// Binding existence cache: ModelServing key ("namespace/name") -> has binding
	bindingMu       sync.RWMutex
	bindingCache    map[string]bool
	bindingCacheTTL time.Duration
	bindingRefresh  time.Time

	// Pending waits for scale-from-zero
	pendingMu sync.Mutex
	pending   map[string]*pendingWait

	timeout time.Duration
	stopCh  chan struct{}
}

type pendingWait struct {
	triggered  bool
	resultChan chan *waitResult
}

type waitResult struct {
	pods        []*datastore.PodInfo
	modelServer *networkingv1alpha1.ModelServer
	err         error
}

// NewManager creates a new scale-from-zero manager.
func NewManager(
	kubeClient clientset.Interface,
	bindingLister listerv1alpha1.AutoscalingPolicyBindingLister,
) *Manager {
	enabled := getEnvBool("ENABLE_SCALE_FROM_ZERO", false)
	timeout := parseDurationEnv("SCALE_FROM_ZERO_TIMEOUT", defaultScaleFromZeroTimeout)

	m := &Manager{
		enabled:         enabled,
		kubeClient:      kubeClient,
		bindingLister:   bindingLister,
		servingForMS:    make(map[string]string),
		bindingCache:    make(map[string]bool),
		bindingCacheTTL: defaultBindingCacheTTL,
		pending:         make(map[string]*pendingWait),
		timeout:         timeout,
		stopCh:          make(chan struct{}),
	}

	if enabled && bindingLister != nil {
		// Initial cache load
		_ = m.refreshBindingCache()
		// Start periodic refresher
		go m.bindingCacheRefresher()
	}

	return m
}

// Enabled returns whether scale-from-zero is enabled.
func (m *Manager) Enabled() bool {
	return m.enabled
}

// Handle triggers scale-from-zero for the given ModelServer and waits for pods to become available.
func (m *Manager) Handle(ctx context.Context, namespace, modelServerName string) ([]*datastore.PodInfo, *networkingv1alpha1.ModelServer, error) {
	if !m.enabled {
		return nil, nil, ErrNotEnabled
	}

	msKey := fmt.Sprintf("%s/%s", namespace, modelServerName)

	m.mu.RLock()
	servingKey, exists := m.servingForMS[msKey]
	m.mu.RUnlock()
	if !exists {
		return nil, nil, ErrNotConfigured
	}

	if !m.hasAutoscalingPolicy(servingKey) {
		return nil, nil, ErrNotConfigured
	}

	m.pendingMu.Lock()
	pw, exists := m.pending[msKey]
	if !exists {
		pw = &pendingWait{resultChan: make(chan *waitResult, 1)}
		m.pending[msKey] = pw
	}
	if !pw.triggered {
		pw.triggered = true
		go m.triggerScaleUp(servingKey, msKey)
	}
	m.pendingMu.Unlock()

	waitCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	select {
	case result := <-pw.resultChan:
		if result.err != nil {
			return nil, nil, result.err
		}
		return result.pods, result.modelServer, nil
	case <-waitCtx.Done():
		m.pendingMu.Lock()
		delete(m.pending, msKey)
		m.pendingMu.Unlock()
		return nil, nil, ErrTimeout
	}
}

// SetModelServerMapping maps a ModelServer to its parent ModelServing.
func (m *Manager) SetModelServerMapping(namespace, modelServerName, modelServingName string) {
	if !m.enabled {
		return
	}
	msKey := fmt.Sprintf("%s/%s", namespace, modelServerName)
	servingKey := fmt.Sprintf("%s/%s", namespace, modelServingName)

	m.mu.Lock()
	m.servingForMS[msKey] = servingKey
	m.mu.Unlock()
}

// DeleteModelServerMapping removes the mapping for a deleted ModelServer.
func (m *Manager) DeleteModelServerMapping(namespace, modelServerName string) {
	if !m.enabled {
		return
	}
	msKey := fmt.Sprintf("%s/%s", namespace, modelServerName)

	m.mu.Lock()
	delete(m.servingForMS, msKey)
	m.mu.Unlock()

	m.pendingMu.Lock()
	delete(m.pending, msKey)
	m.pendingMu.Unlock()
}

// OnPodsAvailable wakes pending waiters when pods become available.
func (m *Manager) OnPodsAvailable(namespace, modelServerName string, pods []*datastore.PodInfo, modelServer *networkingv1alpha1.ModelServer) {
	if !m.enabled {
		return
	}
	msKey := fmt.Sprintf("%s/%s", namespace, modelServerName)

	m.pendingMu.Lock()
	pw, exists := m.pending[msKey]
	if exists {
		delete(m.pending, msKey)
	}
	m.pendingMu.Unlock()

	if exists && pw != nil {
		select {
		case pw.resultChan <- &waitResult{pods: pods, modelServer: modelServer}:
		default:
		}
	}
}

// RefreshBindingCache forces a refresh of the binding cache.
func (m *Manager) RefreshBindingCache() {
	_ = m.refreshBindingCache()
}

// Stop stops the manager's background goroutines.
func (m *Manager) Stop() {
	close(m.stopCh)
}

func (m *Manager) triggerScaleUp(servingKey, msKey string) {
	servingNamespace, servingName, ok := strings.Cut(servingKey, "/")
	if !ok {
		klog.Errorf("invalid serving key: %s", servingKey)
		m.notifyError(msKey, ErrTriggerFailed)
		return
	}

	patch := map[string]any{
		"spec": map[string]any{
			"replicas": 1,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		klog.Errorf("failed to marshal scale-up patch for %s: %v", servingKey, err)
		m.notifyError(msKey, ErrTriggerFailed)
		return
	}

	klog.Infof("scale-from-zero: patching %s/%s to replicas=1 (triggered by %s)", servingNamespace, servingName, msKey)

	_, err = m.kubeClient.WorkloadV1alpha1().ModelServings(servingNamespace).Patch(
		context.Background(), servingName, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.Errorf("scale-from-zero: patch failed %s/%s: %v", servingNamespace, servingName, err)
		m.notifyError(msKey, ErrTriggerFailed)
		return
	}

	klog.Infof("scale-from-zero: patched %s/%s to replicas=1", servingNamespace, servingName)
}

func (m *Manager) notifyError(msKey string, err error) {
	m.pendingMu.Lock()
	pw, exists := m.pending[msKey]
	if exists {
		delete(m.pending, msKey)
	}
	m.pendingMu.Unlock()

	if exists && pw != nil {
		select {
		case pw.resultChan <- &waitResult{err: err}:
		default:
		}
	}
}

func (m *Manager) hasAutoscalingPolicy(servingKey string) bool {
	m.bindingMu.RLock()
	cached, exists := m.bindingCache[servingKey]
	cacheValid := exists && time.Since(m.bindingRefresh) < m.bindingCacheTTL
	m.bindingMu.RUnlock()

	if cacheValid {
		return cached
	}

	_ = m.refreshBindingCache()

	m.bindingMu.RLock()
	cached, exists = m.bindingCache[servingKey]
	m.bindingMu.RUnlock()

	return exists && cached
}

func (m *Manager) refreshBindingCache() error {
	if m.bindingLister == nil {
		return nil
	}

	bindings, err := m.bindingLister.List(labels.Everything())
	if err != nil {
		klog.Warningf("failed to list AutoscalingPolicyBindings: %v", err)
		return err
	}

	newCache := make(map[string]bool)
	for _, binding := range bindings {
		if binding.Spec.HomogeneousTarget != nil {
			ref := binding.Spec.HomogeneousTarget.Target.TargetRef
			ns := ref.Namespace
			if ns == "" {
				ns = binding.Namespace
			}
			key := fmt.Sprintf("%s/%s", ns, ref.Name)
			newCache[key] = true
		}
		if binding.Spec.HeterogeneousTarget != nil {
			for _, param := range binding.Spec.HeterogeneousTarget.Params {
				ref := param.Target.TargetRef
				ns := ref.Namespace
				if ns == "" {
					ns = binding.Namespace
				}
				key := fmt.Sprintf("%s/%s", ns, ref.Name)
				newCache[key] = true
			}
		}
	}

	m.bindingMu.Lock()
	m.bindingCache = newCache
	m.bindingRefresh = time.Now()
	m.bindingMu.Unlock()

	return nil
}

func (m *Manager) bindingCacheRefresher() {
	ticker := time.NewTicker(m.bindingCacheTTL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = m.refreshBindingCache()
		case <-m.stopCh:
			return
		}
	}
}

func getEnvBool(key string, fallback bool) bool {
	if value, ok := os.LookupEnv(key); ok {
		if boolValue, err := strconv.ParseBool(value); err == nil {
			return boolValue
		}
	}
	return fallback
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	if s, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
