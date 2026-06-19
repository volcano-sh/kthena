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

package datastore

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

var (
	metricsName = []string{
		utils.KVCacheUsage,
		utils.RequestWaitingNum,
		utils.RequestRunningNum,
		utils.TPOT,
		utils.TTFT,
	}

	histogramMetricsName = []string{
		utils.TPOT,
		utils.TTFT,
	}
)

const (
	// defaultMetricsScrapeInterval is the default polling interval for pod metrics.
	defaultMetricsScrapeInterval = 1 * time.Second
	metricsScrapeIntervalEnv     = "METRICS_SCRAPE_INTERVAL"

	// onFlightSyncInterval caps Redis read traffic from SyncOnFlightCounts.
	// At most one HMGET is issued per interval regardless of request rate;
	// all other callers use the local atomic values maintained by Incr/Decr.
	onFlightSyncInterval = 50 * time.Millisecond
)

// createTokenTracker creates a token tracker with configuration from environment variables
func createTokenTracker() TokenTracker {
	var opts []TokenTrackerOption

	// Parse window size from environment
	if windowSizeStr := os.Getenv("FAIRNESS_WINDOW_SIZE"); windowSizeStr != "" {
		if windowSize, err := time.ParseDuration(windowSizeStr); err == nil {
			opts = append(opts, WithWindowSize(windowSize))
		} else {
			klog.Warningf("Invalid FAIRNESS_WINDOW_SIZE: %v, using default", err)
		}
	}

	// Parse token weights from environment
	inputWeightStr := os.Getenv("FAIRNESS_INPUT_TOKEN_WEIGHT")
	outputWeightStr := os.Getenv("FAIRNESS_OUTPUT_TOKEN_WEIGHT")

	if inputWeightStr != "" || outputWeightStr != "" {
		inputWeight := defaultInputTokenWeight
		outputWeight := defaultOutputTokenWeight

		if inputWeightStr != "" {
			if w, err := strconv.ParseFloat(inputWeightStr, 64); err == nil {
				inputWeight = w
			} else {
				klog.Warningf("Invalid FAIRNESS_INPUT_TOKEN_WEIGHT: %v, using default", err)
			}
		}

		if outputWeightStr != "" {
			if w, err := strconv.ParseFloat(outputWeightStr, 64); err == nil {
				outputWeight = w
			} else {
				klog.Warningf("Invalid FAIRNESS_OUTPUT_TOKEN_WEIGHT: %v, using default", err)
			}
		}

		opts = append(opts, WithTokenWeights(inputWeight, outputWeight))
	}

	return NewInMemorySlidingWindowTokenTracker(opts...)
}

// EventType represents different types of events that can trigger callbacks
type EventType string

const (
	EventAdd    EventType = "add"
	EventUpdate EventType = "update"
	EventDelete EventType = "delete"
)

// EventData contains information about the event that triggered the callback
type EventData struct {
	EventType EventType
	Pod       types.NamespacedName
	Gateway   types.NamespacedName

	ModelName  string
	ModelRoute *aiv1alpha1.ModelRoute
}

// CallbackFunc is the type of function that can be registered as a callback
type CallbackFunc func(data EventData)

// PodRuntimeInspector fetches runtime metrics and loaded models for a pod.
type PodRuntimeInspector interface {
	GetPodMetrics(engine string, pod *corev1.Pod, port uint32, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram)
	GetPodModels(engine string, pod *corev1.Pod, port uint32) ([]string, error)
}

type realPodRuntimeInspector struct{}

func (realPodRuntimeInspector) GetPodMetrics(engine string, pod *corev1.Pod, port uint32, previousHistogram map[string]*dto.Histogram) (map[string]float64, map[string]*dto.Histogram) {
	return backend.GetPodMetrics(engine, pod, port, previousHistogram)
}

func (realPodRuntimeInspector) GetPodModels(engine string, pod *corev1.Pod, port uint32) ([]string, error) {
	return backend.GetPodModels(engine, pod, port)
}

type Option func(*store)

func WithPodRuntimeInspector(inspector PodRuntimeInspector) Option {
	return func(s *store) {
		if inspector != nil {
			s.podRuntimeInspector = inspector
		}
	}
}

// WithRedisOnFlightCounter configures the store to maintain globally visible
// in-flight request counts via Redis, enabling accurate scheduling across
// multiple router replicas.
func WithRedisOnFlightCounter(counter OnFlightCounter) Option {
	return func(s *store) {
		s.onFlightCounter = counter
	}
}

// Store is an interface for storing and retrieving data
type Store interface {
	// Add modelServer which are selected by modelServer.Spec.WorkloadSelector
	AddOrUpdateModelServer(modelServer *aiv1alpha1.ModelServer, pods sets.Set[types.NamespacedName]) error
	// Delete modelServer
	DeleteModelServer(name types.NamespacedName) error
	// Get modelServer
	GetModelServer(name types.NamespacedName) *aiv1alpha1.ModelServer
	GetPodsByModelServer(name types.NamespacedName) ([]*PodInfo, error)

	// Refresh Store and ModelServer when add a new pod or update a pod
	AddOrUpdatePod(pod *corev1.Pod, modelServer []*aiv1alpha1.ModelServer) error
	// AppendModelServerToPod appends new modelservers to the podInfo without replacing existing ones
	AppendModelServerToPod(pod *corev1.Pod, modelServers []*aiv1alpha1.ModelServer) error
	// Refresh Store and ModelServer when delete a pod
	DeletePod(podName types.NamespacedName) error

	// New methods for routing functionality
	MatchModelServer(modelName string, request *http.Request, gatewayKey string) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error)

	// Model routing methods
	AddOrUpdateModelRoute(mr *aiv1alpha1.ModelRoute) error
	DeleteModelRoute(namespacedName string) error
	GetModelRoute(namespacedName string) *aiv1alpha1.ModelRoute

	// PDGroup methods for efficient PD scheduling
	GetDecodePods(modelServerName types.NamespacedName) ([]*PodInfo, error)
	GetPrefillPods(modelServerName types.NamespacedName) ([]*PodInfo, error)
	GetPrefillPodsForDecodeGroup(modelServerName types.NamespacedName, decodePodName types.NamespacedName) ([]*PodInfo, error)

	// New methods for callback management
	RegisterCallback(kind string, callback CallbackFunc)
	// Run to update pod info periodically
	Run(context.Context)

	// HasSynced checks if the store has been initialized and synced
	HasSynced() bool

	// GetPodInfo returns the pod info for a given pod name (for testing)
	GetPodInfo(podName types.NamespacedName) *PodInfo

	// SyncOnFlightCounts fetches the current on-flight counts for all tracked
	// pods from Redis in a single round-trip and updates their local counters.
	// Call this immediately before scheduling so scores reflect cross-router
	// traffic. Reads are rate-limited to at most one Redis HMGET per
	// onFlightSyncInterval; all other callers use the local atomic values that
	// Incr/Decr keep up to date. No-op when no Redis counter is configured.
	SyncOnFlightCounts()

	// IncrPodOnFlightRequests atomically increments the in-flight request counter for
	// the given pod. Must be called just before dispatching a request to the pod.
	IncrPodOnFlightRequests(podName types.NamespacedName)
	// DecrPodOnFlightRequests atomically decrements the in-flight request counter for
	// the given pod. Must be called once the response is received (or the request fails).
	DecrPodOnFlightRequests(podName types.NamespacedName)

	// GetTokenCount returns the token count for a user and model
	GetTokenCount(userId, modelName string) (float64, error)
	// UpdateTokenCount updates token usage for a user and model
	UpdateTokenCount(userId, modelName string, inputTokens, outputTokens float64) error
	// GetRequestCount returns the request count for a user and model in the current window
	GetRequestCount(userId, modelName string) (int, error)

	// Enqueue adds a request to the fair queue
	Enqueue(*Request) error

	// GetRequestWaitingQueueStats returns per-model queue lengths
	GetRequestWaitingQueueStats() []QueueStat

	// Gateway methods (using standard Gateway API)
	AddOrUpdateGateway(gateway *gatewayv1.Gateway) error
	DeleteGateway(key string) error
	GetGateway(key string) *gatewayv1.Gateway
	GetGatewaysByNamespace(namespace string) []*gatewayv1.Gateway
	GetAllGateways() []*gatewayv1.Gateway

	// InferencePool methods (using Gateway API Inference Extension)
	AddOrUpdateInferencePool(inferencePool *inferencev1.InferencePool) error
	DeleteInferencePool(key string) error
	GetInferencePool(key string) *inferencev1.InferencePool
	GetAllInferencePools() []*inferencev1.InferencePool
	GetPodsByInferencePool(name types.NamespacedName) ([]*PodInfo, error)

	// HTTPRoute methods (using standard Gateway API)
	AddOrUpdateHTTPRoute(httpRoute *gatewayv1.HTTPRoute) error
	DeleteHTTPRoute(key string) error
	GetHTTPRoute(key string) *gatewayv1.HTTPRoute
	GetAllHTTPRoutes() []*gatewayv1.HTTPRoute
	GetHTTPRoutesByGateway(gatewayKey string) []*gatewayv1.HTTPRoute

	// GetModelNames returns all model names registered via ModelRoutes,
	// including both base model names and LoRA adapter names.
	GetModelNames() []string

	// Debug interface methods
	GetAllModelRoutes() map[string]*aiv1alpha1.ModelRoute
	GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer
	GetAllPods() map[types.NamespacedName]*PodInfo
}

// QueueStat holds per-model queue metrics to aid scheduling decisions
type QueueStat struct {
	Model  string
	Length int
}

type PodInfo struct {
	Pod *corev1.Pod
	// Name of AI inference engine
	engine string
	// TODO: add metrics here
	GPUCacheUsage     float64 // GPU KV-cache usage.
	RequestWaitingNum float64 // Number of requests waiting to be processed.
	RequestRunningNum float64 // Number of requests running.
	// for calculating the average value over the time interval, need to store the results of the last query
	TimeToFirstToken   *dto.Histogram
	TimePerOutputToken *dto.Histogram
	TPOT               float64
	TTFT               float64

	// onFlightRequestNum tracks requests actively in-flight from the router to this pod.
	// Updated atomically with zero delay — not subject to the ~1 s engine-metrics poll lag.
	// When a Redis-backed OnFlightCounter is configured on the store this field is also
	// kept in sync with the global Redis counter so it reflects cross-router traffic.
	onFlightRequestNum atomic.Int64

	mutex sync.RWMutex // Protects concurrent access to Pod, engine, metrics, models and modelServer fields
	// Protected fields - use accessor methods for thread-safe access
	models      sets.Set[string]               // running models. Including base model and lora adapters.
	modelServer sets.Set[types.NamespacedName] // The modelservers this pod belongs to
}

// NewPodInfo constructs a PodInfo with the given pod and inference engine.
func NewPodInfo(pod *corev1.Pod, engine string) *PodInfo {
	return &PodInfo{
		Pod:    pod,
		engine: engine,
	}
}

// modelRouteInfo stores the mapping between a ModelRoute resource and its associated models.
// It maintains both the primary model and any LoRA adapters that are configured for this route.
type modelRouteInfo struct {
	// model is the primary model name that this route serves.
	// If empty, it means this route only serves LoRA adapters.
	model string

	// loras is a list of LoRA adapter names that this route serves.
	// These adapters can be used to modify the behavior of the primary model.
	loras []string
}

type store struct {
	modelServer sync.Map // map[types.NamespacedName]*modelServer
	pods        sync.Map // map[types.NamespacedName]*PodInfo

	// onFlightCounter is optional. When non-nil (Redis-backed), in-flight request
	// counts are shared across all router replicas via Redis. When nil, only the
	// local per-PodInfo atomic counter is used (suitable for single-router setups).
	onFlightCounter OnFlightCounter
	// lastOnFlightSync is the Unix nanosecond timestamp of the last sync attempt.
	// Updated before the Redis call (not after) to prevent concurrent goroutines
	// from all hitting Redis within the same window. Gates SyncOnFlightCounts to
	// at most one Redis HMGET per onFlightSyncInterval.
	lastOnFlightSync atomic.Int64

	routeMutex sync.RWMutex
	// Model routing fields
	routeInfo          map[string]*modelRouteInfo
	routes             map[string][]*aiv1alpha1.ModelRoute // key: model name, value: list of ModelRoutes
	loraRoutes         map[string][]*aiv1alpha1.ModelRoute // key: lora name, value: list of ModelRoutes
	gatewayModelRoutes map[string]sets.Set[string]         // key: gateway key (namespace/name), value: set of ModelRoute keys

	// Gateway fields (using standard Gateway API)
	gatewayMutex sync.RWMutex
	gateways     map[string]*gatewayv1.Gateway // key: namespace/name, value: *gatewayv1.Gateway

	// InferencePool fields (using Gateway API Inference Extension)
	inferencePoolMutex sync.RWMutex
	inferencePools     map[string]*inferencev1.InferencePool // key: namespace/name, value: *inferencev1.InferencePool

	// HTTPRoute fields (using standard Gateway API)
	httpRouteMutex sync.RWMutex
	httpRoutes     map[string]*gatewayv1.HTTPRoute // key: namespace/name, value: *gatewayv1.HTTPRoute
	gatewayRoutes  map[string]sets.Set[string]     // key: gateway key (namespace/name), value: set of HTTPRoute keys
	// New fields for callback management
	callbacks map[string][]CallbackFunc

	// initialSynced is used to indicate whether all the resources has been processed and storred into this store.
	initialSynced *atomic.Bool
	// model -> RequestPriorityQueue
	requestWaitingQueue   sync.Map
	tokenTracker          TokenTracker
	podRuntimeInspector   PodRuntimeInspector
	rootCtx               context.Context // Lifecycle context for queue goroutines, set by Run()
	fairnessQueueConfig   FairnessQueueConfig
	metricsScrapeInterval time.Duration
}

func New(opts ...Option) Store {
	s := &store{
		modelServer:         sync.Map{},
		pods:                sync.Map{},
		routeInfo:           make(map[string]*modelRouteInfo),
		routes:              make(map[string][]*aiv1alpha1.ModelRoute),
		loraRoutes:          make(map[string][]*aiv1alpha1.ModelRoute),
		gatewayModelRoutes:  make(map[string]sets.Set[string]),
		gateways:            make(map[string]*gatewayv1.Gateway),
		inferencePools:      make(map[string]*inferencev1.InferencePool),
		httpRoutes:          make(map[string]*gatewayv1.HTTPRoute),
		gatewayRoutes:       make(map[string]sets.Set[string]),
		callbacks:           make(map[string][]CallbackFunc),
		initialSynced:       &atomic.Bool{},
		requestWaitingQueue: sync.Map{},
		// Create token tracker with environment-based configuration
		tokenTracker:          createTokenTracker(),
		podRuntimeInspector:   realPodRuntimeInspector{},
		fairnessQueueConfig:   createFairnessQueueConfig(),
		metricsScrapeInterval: parseMetricsScrapeInterval(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(s)
		}
	}
	return s
}

func (s *store) getPodRuntimeInspector() PodRuntimeInspector {
	if s.podRuntimeInspector == nil {
		return realPodRuntimeInspector{}
	}
	return s.podRuntimeInspector
}

// createFairnessQueueConfig reads fairness queue configuration from environment variables.
func createFairnessQueueConfig() FairnessQueueConfig {
	cfg := DefaultFairnessQueueConfig()

	if v := os.Getenv("FAIRNESS_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MaxConcurrent = n
		} else {
			klog.Warningf("Invalid FAIRNESS_MAX_CONCURRENT: %q, using default %d", v, cfg.MaxConcurrent)
		}
	}

	if v := os.Getenv("FAIRNESS_MAX_QPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxQPS = n
		} else {
			klog.Warningf("Invalid FAIRNESS_MAX_QPS: %q, using default %d", v, cfg.MaxQPS)
		}
	}

	if v := os.Getenv("FAIRNESS_PRIORITY_REFRESH_RETRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.MaxPriorityRefreshRetries = n
		} else {
			klog.Warningf("Invalid FAIRNESS_PRIORITY_REFRESH_RETRIES: %q, using default %d", v, cfg.MaxPriorityRefreshRetries)
		}
	}

	if v := os.Getenv("FAIRNESS_REBUILD_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.RebuildThreshold = n
		} else {
			klog.Warningf("Invalid FAIRNESS_REBUILD_THRESHOLD: %q, using default %d", v, cfg.RebuildThreshold)
		}
	}

	if v := os.Getenv("FAIRNESS_PRIORITY_TOKEN_WEIGHT"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && isValidFairnessWeight(n) {
			cfg.TokenWeight = n
		} else {
			klog.Warningf("Invalid FAIRNESS_PRIORITY_TOKEN_WEIGHT: %q, using default %v", v, cfg.TokenWeight)
		}
	}

	if v := os.Getenv("FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && isValidFairnessWeight(n) {
			cfg.RequestNumWeight = n
		} else {
			klog.Warningf("Invalid FAIRNESS_PRIORITY_REQUEST_NUM_WEIGHT: %q, using default %v", v, cfg.RequestNumWeight)
		}
	}

	return cfg
}

func isValidFairnessWeight(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func parseMetricsScrapeInterval() time.Duration {
	if v := os.Getenv(metricsScrapeIntervalEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		} else {
			klog.Warningf("Invalid %s: %q, using default %v", metricsScrapeIntervalEnv, v, defaultMetricsScrapeInterval)
		}
	}
	return defaultMetricsScrapeInterval
}

func (s *store) Run(ctx context.Context) {
	s.rootCtx = ctx
	go func() {
		ticker := time.NewTicker(s.metricsScrapeInterval)
		defer ticker.Stop()
		for {
			s.pods.Range(func(key, value any) bool {
				if p, ok := value.(*PodInfo); ok {
					s.updatePodMetrics(p)
					s.updatePodModels(p)
				}
				return true
			})
			s.initialSynced.Store(true)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// SyncOnFlightCounts fetches current on-flight counts for all tracked pods from
// Redis in one HMGET and updates their local atomic counters. Reads are
// rate-limited by onFlightSyncInterval: at most one goroutine per interval
// actually hits Redis (via a CAS on lastOnFlightSync); all other callers return
// immediately and use the local values maintained by IncrPodOnFlightRequests /
// DecrPodOnFlightRequests.
func (s *store) SyncOnFlightCounts() {
	if s.onFlightCounter == nil {
		return
	}

	// Rate-gate: skip Redis if we synced recently.
	now := time.Now().UnixNano()
	lastSync := s.lastOnFlightSync.Load()
	if now-lastSync < int64(onFlightSyncInterval) {
		return
	}
	// CAS ensures exactly one goroutine wins the sync slot per interval.
	// Using the previously loaded lastSync as the expected value prevents multiple
	// goroutines from all winning the CAS when they observe the same stale timestamp.
	if !s.lastOnFlightSync.CompareAndSwap(lastSync, now) {
		return
	}

	var podNames []types.NamespacedName
	s.pods.Range(func(k, v any) bool {
		if nn, ok := k.(types.NamespacedName); ok {
			podNames = append(podNames, nn)
		}
		return true
	})
	if len(podNames) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	counts, err := s.onFlightCounter.BatchGet(ctx, podNames)
	if err != nil {
		klog.V(4).Infof("SyncOnFlightCounts: Redis batch get failed: %v", err)
		return
	}
	for podName, count := range counts {
		if value, ok := s.pods.Load(podName); ok {
			value.(*PodInfo).SetOnFlightRequestNum(count)
		}
	}
}
func (s *store) GetTokenCount(userID, model string) (float64, error) {
	return s.tokenTracker.GetTokenCount(userID, model)
}

func (s *store) UpdateTokenCount(userID, model string, inputTokens, outputTokens float64) error {
	return s.tokenTracker.UpdateTokenCount(userID, model, inputTokens, outputTokens)
}

func (s *store) GetRequestCount(userID, model string) (int, error) {
	return s.tokenTracker.GetRequestCount(userID, model)
}

func (s *store) Enqueue(req *Request) error {
	modelName := req.ModelName
	var queue *RequestPriorityQueue
	val, ok := s.requestWaitingQueue.Load(modelName)
	if ok {
		queue, _ = val.(*RequestPriorityQueue)
	} else {
		newQueue := NewRequestPriorityQueueWithConfig(nil, s.fairnessQueueConfig, s.tokenTracker)
		val, ok = s.requestWaitingQueue.LoadOrStore(modelName, newQueue)
		if !ok {
			queueCtx := s.rootCtx
			if queueCtx == nil {
				klog.Warning("store.Enqueue called before Run(); using background context for queue")
				queueCtx = context.Background()
			}
			go newQueue.Run(queueCtx, s.fairnessQueueConfig.MaxQPS)
		}
		queue, _ = val.(*RequestPriorityQueue)
	}
	err := queue.PushRequest(req)
	if err != nil {
		klog.Errorf("failed to push request to waiting queue: %v", err)
		return err
	}
	return nil
}

func (s *store) GetRequestWaitingQueueStats() []QueueStat {
	stats := make([]QueueStat, 0)
	s.requestWaitingQueue.Range(func(modelName, queueVal interface{}) bool {
		name, _ := modelName.(string)
		queue, _ := queueVal.(*RequestPriorityQueue)
		length := 0
		if queue != nil {
			length = queue.Len()
		}
		if length > 0 {
			stats = append(stats, QueueStat{Model: name, Length: length})
		}
		return true
	})
	return stats
}

func (s *store) HasSynced() bool {
	return s.initialSynced.Load()
}

func (s *store) GetPodInfo(podName types.NamespacedName) *PodInfo {
	if value, ok := s.pods.Load(podName); ok {
		return value.(*PodInfo)
	}
	return nil
}

// IncrPodOnFlightRequests increments the in-flight counter for the given pod.
// When a Redis counter is configured the increment is performed atomically in
// Redis and the returned global value is stored locally; otherwise the local
// atomic counter is incremented directly.
func (s *store) IncrPodOnFlightRequests(podName types.NamespacedName) {
	value, ok := s.pods.Load(podName)
	if !ok {
		klog.V(4).Infof("IncrPodOnFlightRequests: pod %s not found in store", podName)
		return
	}
	podInfo := value.(*PodInfo)
	if s.onFlightCounter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if count, err := s.onFlightCounter.Incr(ctx, podName); err == nil {
			podInfo.SetOnFlightRequestNum(count)
			return
		} else {
			klog.V(4).Infof("Redis on-flight incr failed for pod %s: %v, falling back to local counter", podName, err)
		}
	}
	podInfo.IncrOnFlightRequests()
}

// DecrPodOnFlightRequests decrements the in-flight counter for the given pod.
func (s *store) DecrPodOnFlightRequests(podName types.NamespacedName) {
	value, ok := s.pods.Load(podName)
	if !ok {
		klog.V(4).Infof("DecrPodOnFlightRequests: pod %s not found in store", podName)
		return
	}
	podInfo := value.(*PodInfo)
	if s.onFlightCounter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		if count, err := s.onFlightCounter.Decr(ctx, podName); err == nil {
			podInfo.SetOnFlightRequestNum(count)
			return
		} else {
			klog.V(4).Infof("Redis on-flight decr failed for pod %s: %v, falling back to local counter", podName, err)
		}
	}
	podInfo.DecrOnFlightRequests()
}

func (s *store) AddOrUpdateModelServer(ms *aiv1alpha1.ModelServer, pods sets.Set[types.NamespacedName]) error {
	name := utils.GetNamespaceName(ms)
	var modelServerObj *modelServer
	if value, ok := s.modelServer.Load(name); !ok {
		modelServerObj = newModelServer(ms)
		// New object — no concurrent access yet, safe to write without lock
		if len(pods) != 0 {
			modelServerObj.pods = pods
		}
	} else {
		modelServerObj = value.(*modelServer)
		// Existing object — concurrent readers may access modelServer and pods,
		// so we must hold the lock to prevent data races.
		modelServerObj.mutex.Lock()
		modelServerObj.modelServer = ms
		if len(pods) != 0 {
			// do not operate s.pods here, which are done within pod handler
			modelServerObj.pods = pods
		}
		modelServerObj.mutex.Unlock()
	}
	s.modelServer.Store(name, modelServerObj)
	return nil
}

func (s *store) DeleteModelServer(ms types.NamespacedName) error {
	value, ok := s.modelServer.LoadAndDelete(ms)
	if !ok {
		return nil
	}
	modelServerObj := value.(*modelServer)
	podNames := modelServerObj.getPods()
	// then delete the model server from all pod info
	for _, podName := range podNames {
		if value, ok := s.pods.Load(podName); ok {
			podInfo := value.(*PodInfo)
			podInfo.RemoveModelServer(ms)
			if podInfo.GetModelServerCount() == 0 {
				s.pods.Delete(podName)
			}
		} else {
			klog.Warningf("pod %s not found", podName)
		}
	}

	return nil
}

func (s *store) GetModelServer(name types.NamespacedName) *aiv1alpha1.ModelServer {
	if value, ok := s.modelServer.Load(name); ok {
		return value.(*modelServer).getModelServer()
	}
	return nil
}

func (s *store) GetPodsByModelServer(name types.NamespacedName) ([]*PodInfo, error) {
	value, ok := s.modelServer.Load(name)
	if !ok {
		return nil, fmt.Errorf("model server not found: %v", name)
	}
	ms := value.(*modelServer)

	podNames := ms.getPods()
	pods := make([]*PodInfo, 0, len(podNames))

	for _, podName := range podNames {
		if value, ok := s.pods.Load(podName); ok {
			pods = append(pods, value.(*PodInfo))
		}
	}

	return pods, nil
}

// GetDecodePods returns all decode pods for a given model server
func (s *store) GetDecodePods(modelServerName types.NamespacedName) ([]*PodInfo, error) {
	value, ok := s.modelServer.Load(modelServerName)
	if !ok {
		return nil, fmt.Errorf("model server not found: %v", modelServerName)
	}
	ms := value.(*modelServer)

	decodePodNames := ms.getAllDecodePods()
	decodePods := make([]*PodInfo, 0, len(decodePodNames))

	for _, podName := range decodePodNames {
		if value, ok := s.pods.Load(podName); ok {
			decodePods = append(decodePods, value.(*PodInfo))
		}
	}

	return decodePods, nil
}

// GetPrefillPods returns all prefill pods for a given model server
func (s *store) GetPrefillPods(modelServerName types.NamespacedName) ([]*PodInfo, error) {
	value, ok := s.modelServer.Load(modelServerName)
	if !ok {
		return nil, fmt.Errorf("model server not found: %v", modelServerName)
	}
	ms := value.(*modelServer)

	prefillPodNames := ms.getAllPrefillPods()
	prefillPods := make([]*PodInfo, 0, len(prefillPodNames))

	for _, podName := range prefillPodNames {
		if value, ok := s.pods.Load(podName); ok {
			prefillPods = append(prefillPods, value.(*PodInfo))
		}
	}

	return prefillPods, nil
}

// GetPrefillPodsForDecodeGroup returns prefill pods that match the same PD group as the decode pod
func (s *store) GetPrefillPodsForDecodeGroup(modelServerName types.NamespacedName, decodePodName types.NamespacedName) ([]*PodInfo, error) {
	value, ok := s.modelServer.Load(modelServerName)
	if !ok {
		return nil, fmt.Errorf("model server not found: %v", modelServerName)
	}
	ms := value.(*modelServer)

	pod, ok := s.pods.Load(decodePodName)
	if !ok {
		return nil, fmt.Errorf("pod not found: %v", decodePodName)
	}
	podInfo := pod.(*PodInfo)

	prefillPodNames := ms.getPrefillPodsForDecodeGroup(podInfo)
	prefillPods := make([]*PodInfo, 0, len(prefillPodNames))
	for _, podName := range prefillPodNames {
		if value, ok := s.pods.Load(podName); ok {
			prefillPods = append(prefillPods, value.(*PodInfo))
		}
	}

	return prefillPods, nil
}

func (s *store) AddOrUpdatePod(pod *corev1.Pod, modelServers []*aiv1alpha1.ModelServer) error {
	podName := utils.GetNamespaceName(pod)

	newModelServers := sets.New[types.NamespacedName]()
	var engine string
	for _, ms := range modelServers {
		modelServerName := utils.GetNamespaceName(ms)
		newModelServers.Insert(modelServerName)
		// NOTE: even if a pod belongs to multiple model servers, the backend should be the same
		engine = string(ms.Spec.InferenceEngine)
		if value, ok := s.modelServer.Load(modelServerName); ok {
			ms := value.(*modelServer)
			ms.addPod(podName)
			// Categorize the pod for PDGroup scheduling
			klog.V(4).Infof("Categorizing pod %s for PDGroup scheduling, model server %s", podName, modelServerName)
			ms.categorizePodForPDGroup(podName, pod.Labels)
		}
	}

	if value, ok := s.pods.Load(podName); ok {
		// Update existing pod in place — preserve runtime metrics and models.
		oldPodInfo := value.(*PodInfo)
		oldModelServers := oldPodInfo.GetModelServers()
		// Handle the case where the pod no longer belongs to some model servers
		oldPodLabels := oldPodInfo.GetPodLabels()
		for msName := range oldModelServers.Difference(newModelServers) {
			if value, ok := s.modelServer.Load(msName); ok {
				ms := value.(*modelServer)
				ms.deletePod(podName)
				// Remove from PDGroup categorizations
				ms.removePodFromPDGroups(podName, oldPodLabels)
			}
		}

		oldPodInfo.UpdatePod(pod, engine, newModelServers)
		return nil
	}

	// New pod — create PodInfo and fetch initial metrics.
	newPodInfo := &PodInfo{
		Pod:         pod,
		engine:      engine,
		modelServer: newModelServers,
		models:      sets.New[string](),
	}
	s.pods.Store(podName, newPodInfo)
	s.updatePodMetrics(newPodInfo)
	s.updatePodModels(newPodInfo)

	return nil
}

func (s *store) AppendModelServerToPod(pod *corev1.Pod, modelServers []*aiv1alpha1.ModelServer) error {
	podName := utils.GetNamespaceName(pod)

	// Get existing podInfo, return error if pod doesn't exist
	value, ok := s.pods.Load(podName)
	if !ok {
		return fmt.Errorf("pod %s not found in store, cannot append modelserver", podName)
	}

	podInfo := value.(*PodInfo)

	// Append new modelservers only
	for _, ms := range modelServers {
		modelServerName := utils.GetNamespaceName(ms)

		// Only add if not already present
		if !podInfo.HasModelServer(modelServerName) {
			podInfo.AddModelServer(modelServerName)
			// NOTE: even if a pod belongs to multiple model servers, the backend should be the same
			if podInfo.GetEngine() == "" {
				podInfo.SetEngine(string(ms.Spec.InferenceEngine))
			}

			// Update modelServer object to include this pod
			if value, ok := s.modelServer.Load(modelServerName); ok {
				msObj := value.(*modelServer)
				msObj.addPod(podName)
				// Categorize the pod for PDGroup scheduling
				klog.V(4).Infof("Categorizing pod %s for PDGroup scheduling, model server %s", podName, modelServerName)
				msObj.categorizePodForPDGroup(podName, pod.Labels)
			}
		}
	}

	return nil
}

func (s *store) DeletePod(podName types.NamespacedName) error {
	if value, ok := s.pods.Load(podName); ok {
		pod := value.(*PodInfo)
		modelServers := pod.GetModelServers()
		podLabels := pod.GetPodLabels()
		for modelServerName := range modelServers {
			if value, ok := s.modelServer.Load(modelServerName); ok {
				ms := value.(*modelServer)
				ms.deletePod(podName)
				// Remove from PDGroup categorizations
				ms.removePodFromPDGroups(podName, podLabels)
			} else {
				klog.V(4).Infof("model server %s not found for pod %s, maybe already deleted", modelServerName, podName)
			}
		}
		s.pods.Delete(podName)
		// Remove the pod's Redis counter so stale keys do not accumulate.
		if s.onFlightCounter != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := s.onFlightCounter.Delete(ctx, podName); err != nil {
				klog.V(4).Infof("failed to delete Redis on-flight counter for pod %s: %v", podName, err)
			}
		}
	}

	s.triggerCallbacks("Pod", EventData{
		EventType: EventDelete,
		Pod:       podName,
	})

	return nil
}

// Model routing methods
func (s *store) AddOrUpdateModelRoute(mr *aiv1alpha1.ModelRoute) error {
	s.routeMutex.Lock()
	key := mr.Namespace + "/" + mr.Name
	s.routeInfo[key] = &modelRouteInfo{
		model: mr.Spec.ModelName,
		loras: mr.Spec.LoraAdapters,
	}

	if mr.Spec.ModelName != "" {
		// Check if this ModelRoute already exists in the slice
		routes := s.routes[mr.Spec.ModelName]
		found := false
		for i, route := range routes {
			if route.Namespace == mr.Namespace && route.Name == mr.Name {
				routes[i] = mr // Update existing
				sortModelRoutesInPlace(routes)
				s.routes[mr.Spec.ModelName] = routes // Update the map
				found = true
				break
			}
		}
		if !found {
			routes = append(routes, mr)
			sortModelRoutesInPlace(routes)
			s.routes[mr.Spec.ModelName] = routes
		}
	}

	for _, lora := range mr.Spec.LoraAdapters {
		// Check if this ModelRoute already exists in the slice
		loraRoutes := s.loraRoutes[lora]
		found := false
		for i, route := range loraRoutes {
			if route.Namespace == mr.Namespace && route.Name == mr.Name {
				loraRoutes[i] = mr // Update existing
				sortModelRoutesInPlace(loraRoutes)
				s.loraRoutes[lora] = loraRoutes // Update the map
				found = true
				break
			}
		}
		if !found {
			loraRoutes = append(loraRoutes, mr)
			sortModelRoutesInPlace(loraRoutes)
			s.loraRoutes[lora] = loraRoutes
		}
	}

	// Update gateway model routes mapping
	for _, parentRef := range mr.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
			gatewayName := string(parentRef.Name)
			gatewayNamespace := mr.Namespace
			if parentRef.Namespace != nil {
				gatewayNamespace = string(*parentRef.Namespace)
			}
			gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName)

			if s.gatewayModelRoutes[gatewayKey] == nil {
				s.gatewayModelRoutes[gatewayKey] = sets.New[string]()
			}
			s.gatewayModelRoutes[gatewayKey].Insert(key)
		}
	}

	s.routeMutex.Unlock()

	s.triggerCallbacks("ModelRoute", EventData{
		EventType:  EventUpdate,
		ModelName:  mr.Spec.ModelName,
		ModelRoute: mr,
	})
	return nil
}

func sortModelRoutesInPlace(routes []*aiv1alpha1.ModelRoute) {
	sort.Slice(routes, func(i, j int) bool {
		ti, tj := routes[i].CreationTimestamp.Time, routes[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		ri, rj := routes[i].ResourceVersion, routes[j].ResourceVersion
		if ri != rj {
			return ri < rj
		}
		return routes[i].Namespace+"/"+routes[i].Name < routes[j].Namespace+"/"+routes[j].Name
	})
}

func (s *store) DeleteModelRoute(namespacedName string) error {
	s.routeMutex.Lock()
	info := s.routeInfo[namespacedName]
	var modelName string
	var deletedRoute *aiv1alpha1.ModelRoute
	// Collect all model/lora names that may have associated queues (for cleanup after unlock)
	var namesToCleanQueue []string
	if info != nil {
		modelName = info.model
		// Remove from routes map
		if modelName != "" {
			routes := s.routes[modelName]
			newRoutes := make([]*aiv1alpha1.ModelRoute, 0, len(routes))
			for _, route := range routes {
				routeKey := route.Namespace + "/" + route.Name
				if routeKey != namespacedName {
					newRoutes = append(newRoutes, route)
				} else {
					deletedRoute = route
				}
			}
			if len(newRoutes) == 0 {
				delete(s.routes, modelName)
				namesToCleanQueue = append(namesToCleanQueue, modelName)
			} else {
				s.routes[modelName] = newRoutes
			}
		}
		// Remove from loraRoutes map
		for _, lora := range info.loras {
			loraRoutes := s.loraRoutes[lora]
			newLoraRoutes := make([]*aiv1alpha1.ModelRoute, 0, len(loraRoutes))
			for _, route := range loraRoutes {
				routeKey := route.Namespace + "/" + route.Name
				if routeKey != namespacedName {
					newLoraRoutes = append(newLoraRoutes, route)
				} else if deletedRoute == nil {
					deletedRoute = route
				}
			}
			if len(newLoraRoutes) == 0 {
				delete(s.loraRoutes, lora)
				namesToCleanQueue = append(namesToCleanQueue, lora)
			} else {
				s.loraRoutes[lora] = newLoraRoutes
			}
		}
	}

	// Remove from gateway model routes mapping
	if deletedRoute != nil {
		for _, parentRef := range deletedRoute.Spec.ParentRefs {
			if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
				gatewayName := string(parentRef.Name)
				gatewayNamespace := deletedRoute.Namespace
				if parentRef.Namespace != nil {
					gatewayNamespace = string(*parentRef.Namespace)
				}
				gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName)

				if routeSet, exists := s.gatewayModelRoutes[gatewayKey]; exists {
					routeSet.Delete(namespacedName)
					if routeSet.IsEmpty() {
						delete(s.gatewayModelRoutes, gatewayKey)
					}
				}
			}
		}
	}

	delete(s.routeInfo, namespacedName)
	s.routeMutex.Unlock()

	// Clean up associated waiting queues for both base model and all lora adapters
	for _, name := range namesToCleanQueue {
		val, _ := s.requestWaitingQueue.LoadAndDelete(name)
		if val != nil {
			queue, _ := val.(*RequestPriorityQueue)
			queue.Close()
			klog.Infof("deleted waiting queue for model %s", name)
		}
	}

	// Trigger callbacks outside the lock to avoid potential deadlocks
	s.triggerCallbacks("ModelRoute", EventData{
		EventType:  EventDelete,
		ModelName:  modelName,
		ModelRoute: deletedRoute,
	})
	return nil
}

func (s *store) MatchModelServer(model string, req *http.Request, gatewayKey string) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error) {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	var isLora bool
	var candidateRoutes []*aiv1alpha1.ModelRoute

	// Try to find routes by model name first
	routes, ok := s.routes[model]
	if ok {
		candidateRoutes = routes
		isLora = false
	} else {
		// Try to find routes by lora name
		loraRoutes, ok := s.loraRoutes[model]
		if !ok {
			return types.NamespacedName{}, false, nil, fmt.Errorf("not found route rules for model %s", model)
		}
		candidateRoutes = loraRoutes
		isLora = true
	}

	// candidateRoutes are kept sorted oldest-first by AddOrUpdateModelRoute
	for _, mr := range candidateRoutes {
		// Check parentRefs if specified
		if len(mr.Spec.ParentRefs) > 0 {
			// If gatewayKey is provided (not empty), check if ModelRoute matches the specific gateway
			if gatewayKey != "" {
				if !s.matchesSpecificGateway(mr, gatewayKey) {
					continue // Try next ModelRoute
				}
			} else {
				// If ModelRoute has parentRefs but gatewayKey is empty, skip it
				continue // Skip ModelRoute with parentRefs when gatewayKey is not specified
			}
		} else {
			// If gatewayKey is specified, we only match ModelRoute with parentRefs
			// ModelRoute without parentRefs should not match when gatewayKey is provided
			if gatewayKey != "" {
				continue // Skip ModelRoute without parentRefs when gatewayKey is specified
			}
			// If gatewayKey is empty, ModelRoute without parentRefs can match
			// (ModelRoute without parentRefs attaches to all Gateways in the same namespace)
		}

		// Try to match rules
		rule, err := s.selectRule(model, req, mr.Spec.Rules)
		if err != nil {
			continue // Try next ModelRoute
		}

		dst, err := s.selectDestination(rule.TargetModels)
		if err != nil {
			continue // Try next ModelRoute
		}

		// Found a matching ModelRoute
		return types.NamespacedName{Namespace: mr.Namespace, Name: dst.ModelServerName}, isLora, mr, nil
	}

	// No matching ModelRoute found
	return types.NamespacedName{}, false, nil, fmt.Errorf("no matching ModelRoute found for model %s", model)
}

// matchesSpecificGateway checks if the ModelRoute matches a specific gateway
func (s *store) matchesSpecificGateway(mr *aiv1alpha1.ModelRoute, gatewayKey string) bool {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	gatewayObj := s.gateways[gatewayKey]
	if gatewayObj == nil {
		return false
	}

	for _, parentRef := range mr.Spec.ParentRefs {
		// Get namespace from parentRef, default to ModelRoute's namespace
		namespace := mr.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		// Get name from parentRef
		name := string(parentRef.Name)
		key := fmt.Sprintf("%s/%s", namespace, name)

		// Check if this parentRef matches the specified gateway
		if key == gatewayKey {
			// If sectionName is specified, check if the listener exists in the gateway
			if parentRef.SectionName != nil {
				sectionName := string(*parentRef.SectionName)
				for _, listener := range gatewayObj.Spec.Listeners {
					if string(listener.Name) == sectionName {
						return true
					}
				}
			} else {
				// No sectionName specified, match any listener
				return true
			}
		}
	}

	return false
}

func (s *store) selectRule(modelName string, req *http.Request, rules []*aiv1alpha1.Rule) (*aiv1alpha1.Rule, error) {
	for _, rule := range rules {
		if rule.ModelMatch == nil {
			return rule, nil
		}

		// Check Model match if specified
		if rule.ModelMatch.Body != nil && rule.ModelMatch.Body.Model != nil {
			// Perform exact match on Model
			if modelName != *rule.ModelMatch.Body.Model {
				continue // Skip this rule if model name doesn't match
			}
		}

		headersMatched := true
		for key, sm := range rule.ModelMatch.Headers {
			reqValue := req.Header.Get(key)
			if !matchString(sm, reqValue) {
				headersMatched = false
				break
			}
		}
		if !headersMatched {
			continue
		}

		uriMatched := true
		if uriMatch := rule.ModelMatch.Uri; uriMatch != nil {
			if !matchString(uriMatch, req.URL.Path) {
				uriMatched = false
			}
		}

		if !uriMatched {
			continue
		}

		return rule, nil
	}

	return nil, fmt.Errorf("failed to find a matching rule")
}

func matchString(sm *aiv1alpha1.StringMatch, value string) bool {
	switch {
	case sm.Exact != nil:
		return value == *sm.Exact
	case sm.Prefix != nil:
		return strings.HasPrefix(value, *sm.Prefix)
	case sm.Regex != nil:
		matched, _ := regexp.MatchString(*sm.Regex, value)
		return matched
	default:
		return true
	}
}

func (s *store) selectDestination(targets []*aiv1alpha1.TargetModel) (*aiv1alpha1.TargetModel, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("no target models specified in rule")
	}

	weightedSlice, err := toWeightedSlice(targets)
	if err != nil {
		return nil, err
	}

	index, err := selectFromWeightedSlice(weightedSlice)
	if err != nil {
		return nil, err
	}

	return targets[index], nil
}

func toWeightedSlice(targets []*aiv1alpha1.TargetModel) ([]uint32, error) {
	var isWeighted bool
	if targets[0].Weight != nil {
		isWeighted = true
	}

	res := make([]uint32, len(targets))

	for i, target := range targets {
		if (isWeighted && target.Weight == nil) || (!isWeighted && target.Weight != nil) {
			return nil, fmt.Errorf("the weight field in targetModel must be either fully specified or not specified")
		}

		if isWeighted {
			res[i] = *target.Weight
		} else {
			// If weight is not specified, set to 1.
			res[i] = 1
		}
	}

	return res, nil
}

func selectFromWeightedSlice(weights []uint32) (int, error) {
	if len(weights) == 0 {
		return 0, fmt.Errorf("no weights provided")
	}

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	totalWeight := 0
	for _, weight := range weights {
		totalWeight += int(weight)
	}

	if totalWeight == 0 {
		return 0, fmt.Errorf("total weight is zero")
	}

	randomNum := rng.Intn(totalWeight)

	for i, weight := range weights {
		randomNum -= int(weight)
		if randomNum < 0 {
			return i, nil
		}
	}

	return 0, nil
}

func (s *store) updatePodMetrics(pod *PodInfo) {
	engine := pod.GetEngine()
	if engine == "" {
		klog.V(2).Info("failed to find backend in pod")
		return
	}
	podObj := pod.GetPod()
	if podObj == nil {
		klog.V(2).Info("failed to find pod")
		return
	}

	if podObj.Status.PodIP == "" {
		return
	}
	port := s.getPodWorkloadPort(pod)
	previousHistogram := getPreviousHistogram(pod)
	gaugeMetrics, histogramMetrics := s.getPodRuntimeInspector().GetPodMetrics(engine, podObj, port, previousHistogram)
	if gaugeMetrics != nil {
		updateGaugeMetricsInfo(pod, gaugeMetrics)
	}
	if histogramMetrics != nil {
		updateHistogramMetrics(pod, histogramMetrics)
	}
}

func (s *store) updatePodModels(podInfo *PodInfo) {
	engine := podInfo.GetEngine()
	if engine == "" {
		klog.V(2).Info("failed to find backend in pod")
		return
	}
	podObj := podInfo.GetPod()
	if podObj == nil {
		klog.V(2).Info("failed to find pod")
		return
	}

	if podObj.Status.PodIP == "" {
		return
	}
	port := s.getPodWorkloadPort(podInfo)
	models, err := s.getPodRuntimeInspector().GetPodModels(engine, podObj, port)
	if err != nil {
		klog.V(4).Infof("failed to get models of pod %s/%s: %v", podObj.GetNamespace(), podObj.GetName(), err)
		return
	}

	podInfo.UpdateModels(models)
}

func (s *store) getPodWorkloadPort(podInfo *PodInfo) uint32 {
	modelServers := podInfo.GetModelServers()
	for msName := range modelServers {
		if msValue, ok := s.modelServer.Load(msName); ok {
			ms := msValue.(*modelServer).getModelServer()
			if ms != nil && ms.Spec.WorkloadPort.Port > 0 {
				return uint32(ms.Spec.WorkloadPort.Port)
			}
		}
	}
	return 0
}

func getPreviousHistogram(podinfo *PodInfo) map[string]*dto.Histogram {
	podinfo.mutex.RLock()
	defer podinfo.mutex.RUnlock()

	previousHistogram := make(map[string]*dto.Histogram)
	if podinfo.TimePerOutputToken != nil {
		previousHistogram[utils.TPOT] = podinfo.TimePerOutputToken
	}
	if podinfo.TimeToFirstToken != nil {
		previousHistogram[utils.TTFT] = podinfo.TimeToFirstToken
	}
	return previousHistogram
}

func updateGaugeMetricsInfo(podinfo *PodInfo, metricsInfo map[string]float64) {
	podinfo.mutex.Lock()
	defer podinfo.mutex.Unlock()
	updateFuncs := map[string]func(float64){
		utils.KVCacheUsage: func(f float64) {
			podinfo.GPUCacheUsage = f
		},
		utils.RequestWaitingNum: func(f float64) {
			podinfo.RequestWaitingNum = f
		},
		utils.RequestRunningNum: func(f float64) {
			podinfo.RequestRunningNum = f
		},
		utils.TPOT: func(f float64) {
			if f == float64(0.0) {
				return
			}
			podinfo.TPOT = f
		},
		utils.TTFT: func(f float64) {
			if f == float64(0.0) {
				return
			}
			podinfo.TTFT = f
		},
	}

	for _, name := range metricsName {
		if updateFunc, exist := updateFuncs[name]; exist {
			updateFunc(metricsInfo[name])
		} else {
			klog.V(4).Infof("Unknown metric: %s", name)
		}
	}
}

func updateHistogramMetrics(podinfo *PodInfo, histogramMetrics map[string]*dto.Histogram) {
	podinfo.mutex.Lock()
	defer podinfo.mutex.Unlock()
	updateFuncs := map[string]func(*dto.Histogram){
		utils.TPOT: func(h *dto.Histogram) {
			podinfo.TimePerOutputToken = h
		},
		utils.TTFT: func(h *dto.Histogram) {
			podinfo.TimeToFirstToken = h
		},
	}

	for _, name := range histogramMetricsName {
		if updateFunc, exist := updateFuncs[name]; exist {
			updateFunc(histogramMetrics[name])
		} else {
			klog.V(4).Infof("Unknown histogram metric: %s", name)
		}
	}
}

// RegisterCallback registers a callback function for a specific resource
// Note this can only be called during bootstrapping.
func (s *store) RegisterCallback(kind string, callback CallbackFunc) {
	if _, exists := s.callbacks[kind]; !exists {
		s.callbacks[kind] = make([]CallbackFunc, 0)
	}
	s.callbacks[kind] = append(s.callbacks[kind], callback)
}

// triggerCallbacks executes all registered callbacks for a specific event type
func (s *store) triggerCallbacks(kind string, data EventData) {
	if callbacks, exists := s.callbacks[kind]; exists {
		for _, callback := range callbacks {
			go callback(data)
		}
	}
}

// PodInfo methods for thread-safe access to mutable fields

// GetPod returns the current pod pointer. The returned object must be treated as read-only.
func (p *PodInfo) GetPod() *corev1.Pod {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.Pod
}

// GetPodLabels returns the current pod labels. The returned map must be treated as read-only.
func (p *PodInfo) GetPodLabels() map[string]string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	if p.Pod == nil || len(p.Pod.Labels) == 0 {
		return nil
	}
	return p.Pod.Labels
}

// GetPodNamespacedName returns the current pod namespace/name.
func (p *PodInfo) GetPodNamespacedName() types.NamespacedName {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	if p.Pod == nil {
		return types.NamespacedName{}
	}
	return types.NamespacedName{Namespace: p.Pod.Namespace, Name: p.Pod.Name}
}

// UpdatePod replaces pod metadata tracked by PodInfo while preserving runtime metrics and models.
func (p *PodInfo) UpdatePod(pod *corev1.Pod, engine string, modelServers sets.Set[types.NamespacedName]) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.Pod = pod
	p.engine = engine
	p.modelServer = modelServers
}

// GetModels returns a copy of the models set
func (p *PodInfo) GetModels() sets.Set[string] {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	result := sets.New[string]()
	for model := range p.models {
		result.Insert(model)
	}
	return result
}

// Contains checks if a model exists in the models set
func (p *PodInfo) Contains(model string) bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.models != nil && p.models.Contains(model)
}

// UpdateModels updates the models set with a new list of models
func (p *PodInfo) UpdateModels(models []string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.models = sets.New[string](models...)
}

// RemoveModel removes a model from the models set
func (p *PodInfo) RemoveModel(model string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.models != nil {
		p.models.Delete(model)
	}
}

// GetModelServers returns a copy of the modelServer set
func (p *PodInfo) GetModelServers() sets.Set[types.NamespacedName] {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	result := sets.New[types.NamespacedName]()
	for ms := range p.modelServer {
		result.Insert(ms)
	}
	return result
}

// AddModelServer adds a model server to the modelServer set
func (p *PodInfo) AddModelServer(ms types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.modelServer == nil {
		p.modelServer = sets.New[types.NamespacedName]()
	}
	p.modelServer.Insert(ms)
}

// RemoveModelServer removes a model server from the modelServer set
func (p *PodInfo) RemoveModelServer(ms types.NamespacedName) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.modelServer != nil {
		p.modelServer.Delete(ms)
	}
}

// HasModelServer checks if a model server exists in the modelServer set
func (p *PodInfo) HasModelServer(ms types.NamespacedName) bool {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	return p.modelServer != nil && p.modelServer.Contains(ms)
}

// GetModelServerCount returns the number of model servers
func (p *PodInfo) GetModelServerCount() int {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if p.modelServer == nil {
		return 0
	}
	return p.modelServer.Len()
}

// GetModelsList returns all models as a slice
func (p *PodInfo) GetModelsList() []string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if p.models == nil {
		return nil
	}
	return p.models.UnsortedList()
}

// GetModelServersList returns all model servers as a slice
func (p *PodInfo) GetModelServersList() []types.NamespacedName {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if p.modelServer == nil {
		return nil
	}
	return p.modelServer.UnsortedList()
}

// GetEngine returns the inference engine name
func (p *PodInfo) GetEngine() string {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.engine
}

// SetEngine updates the inference engine name.
func (p *PodInfo) SetEngine(engine string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.engine = engine
}

// GetGPUCacheUsage returns the GPU cache usage
func (p *PodInfo) GetGPUCacheUsage() float64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.GPUCacheUsage
}

// GetRequestWaitingNum returns the number of waiting requests
func (p *PodInfo) GetRequestWaitingNum() float64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.RequestWaitingNum
}

// GetRequestRunningNum returns the number of running requests
func (p *PodInfo) GetRequestRunningNum() float64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.RequestRunningNum
}

// IncrOnFlightRequests atomically increments the local in-flight counter and
// returns the new value.
func (p *PodInfo) IncrOnFlightRequests() int64 {
	return p.onFlightRequestNum.Add(1)
}

// DecrOnFlightRequests atomically decrements the local in-flight counter and
// returns the new value.
func (p *PodInfo) DecrOnFlightRequests() int64 {
	return p.onFlightRequestNum.Add(-1)
}

// SetOnFlightRequestNum atomically stores a new value for the in-flight counter
// (used to sync the global Redis value into the local field).
func (p *PodInfo) SetOnFlightRequestNum(v int64) {
	p.onFlightRequestNum.Store(v)
}

// GetOnFlightRequestNum returns the current in-flight request count as tracked
// by this router instance (or globally, if a Redis counter is configured).
func (p *PodInfo) GetOnFlightRequestNum() int64 {
	return p.onFlightRequestNum.Load()
}

// GetTPOT returns the time per output token
func (p *PodInfo) GetTPOT() float64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.TPOT
}

// GetTTFT returns the time to first token
func (p *PodInfo) GetTTFT() float64 {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	return p.TTFT
}

// Debug interface implementations

// GetAllModelRoutes returns all ModelRoutes in the store
func (s *store) GetAllModelRoutes() map[string]*aiv1alpha1.ModelRoute {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	result := make(map[string]*aiv1alpha1.ModelRoute)
	// Use routeInfo to get all ModelRoutes by their namespaced name
	for key, info := range s.routeInfo {
		// Find the ModelRoute by checking routes or loraRoutes
		var foundRoute *aiv1alpha1.ModelRoute
		if info.model != "" {
			if routes, ok := s.routes[info.model]; ok {
				// Find the route matching this key
				for _, route := range routes {
					routeKey := route.Namespace + "/" + route.Name
					if routeKey == key {
						foundRoute = route
						break
					}
				}
			}
		}
		// If not found in routes, check loraRoutes
		if foundRoute == nil {
			for _, lora := range info.loras {
				if loraRoutes, ok := s.loraRoutes[lora]; ok {
					// Find the route matching this key
					for _, route := range loraRoutes {
						routeKey := route.Namespace + "/" + route.Name
						if routeKey == key {
							foundRoute = route
							break
						}
					}
					if foundRoute != nil {
						break
					}
				}
			}
		}
		if foundRoute != nil {
			result[key] = foundRoute
		}
	}
	return result
}

// GetModelNames returns all model names registered via ModelRoutes,
// including both base model names and LoRA adapter names.
func (s *store) GetModelNames() []string {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	names := make([]string, 0, len(s.routes)+len(s.loraRoutes))
	for name := range s.routes {
		names = append(names, name)
	}
	for name := range s.loraRoutes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetAllModelServers returns all ModelServers in the store
func (s *store) GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer {
	result := make(map[types.NamespacedName]*aiv1alpha1.ModelServer)
	s.modelServer.Range(func(key, value any) bool {
		if namespacedName, ok := key.(types.NamespacedName); ok {
			if ms, ok := value.(*modelServer); ok {
				result[namespacedName] = ms.getModelServer()
			}
		}
		return true
	})
	return result
}

// GetAllPods returns all Pods in the store
func (s *store) GetAllPods() map[types.NamespacedName]*PodInfo {
	result := make(map[types.NamespacedName]*PodInfo)
	s.pods.Range(func(key, value any) bool {
		if namespacedName, ok := key.(types.NamespacedName); ok {
			if podInfo, ok := value.(*PodInfo); ok {
				result[namespacedName] = podInfo
			}
		}
		return true
	})
	return result
}

// GetModelRoute returns a specific ModelRoute by namespacedName
func (s *store) GetModelRoute(namespacedName string) *aiv1alpha1.ModelRoute {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	info, exists := s.routeInfo[namespacedName]
	if !exists {
		return nil
	}

	// Try to find the route from the primary model
	if info.model != "" {
		if routes, ok := s.routes[info.model]; ok {
			// Find the route matching this namespacedName
			for _, route := range routes {
				routeKey := route.Namespace + "/" + route.Name
				if routeKey == namespacedName {
					return route
				}
			}
		}
	}

	// Try to find the route from lora adapters
	for _, lora := range info.loras {
		if loraRoutes, ok := s.loraRoutes[lora]; ok {
			// Find the route matching this namespacedName
			for _, route := range loraRoutes {
				routeKey := route.Namespace + "/" + route.Name
				if routeKey == namespacedName {
					return route
				}
			}
		}
	}

	return nil
}

// Gateway methods (using standard Gateway API)

func (s *store) AddOrUpdateGateway(gateway *gatewayv1.Gateway) error {
	key := fmt.Sprintf("%s/%s", gateway.Namespace, gateway.Name)

	s.gatewayMutex.Lock()
	s.gateways[key] = gateway
	s.gatewayMutex.Unlock()

	klog.V(4).Infof("Added or updated Gateway: %s", key)

	// Trigger callback outside the lock to avoid potential deadlocks
	s.triggerCallbacks("Gateway", EventData{
		EventType: EventAdd,
		Gateway:   types.NamespacedName{Namespace: gateway.Namespace, Name: gateway.Name},
	})

	return nil
}

func (s *store) DeleteGateway(key string) error {
	// Extract namespace and name before deletion
	parts := strings.Split(key, "/")
	var namespace, name string
	if len(parts) == 2 {
		namespace, name = parts[0], parts[1]
	}

	s.gatewayMutex.Lock()
	delete(s.gateways, key)
	s.gatewayMutex.Unlock()

	klog.V(4).Infof("Deleted Gateway: %s", key)

	// Trigger callback outside the lock to avoid potential deadlocks
	if namespace != "" && name != "" {
		s.triggerCallbacks("Gateway", EventData{
			EventType: EventDelete,
			Gateway:   types.NamespacedName{Namespace: namespace, Name: name},
		})
	}

	return nil
}

func (s *store) GetGateway(key string) *gatewayv1.Gateway {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	return s.gateways[key]
}

func (s *store) GetGatewaysByNamespace(namespace string) []*gatewayv1.Gateway {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	var result []*gatewayv1.Gateway
	for key, gateway := range s.gateways {
		if strings.HasPrefix(key, namespace+"/") {
			result = append(result, gateway)
		}
	}
	return result
}

func (s *store) GetAllGateways() []*gatewayv1.Gateway {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	var result []*gatewayv1.Gateway
	for _, gateway := range s.gateways {
		result = append(result, gateway)
	}
	return result
}

// InferencePool methods (using Gateway API Inference Extension)

func (s *store) AddOrUpdateInferencePool(inferencePool *inferencev1.InferencePool) error {
	key := fmt.Sprintf("%s/%s", inferencePool.ObjectMeta.Namespace, inferencePool.ObjectMeta.Name)

	s.inferencePoolMutex.Lock()
	s.inferencePools[key] = inferencePool
	s.inferencePoolMutex.Unlock()

	klog.V(4).Infof("Added or updated InferencePool: %s", key)
	return nil
}

func (s *store) DeleteInferencePool(key string) error {
	s.inferencePoolMutex.Lock()
	delete(s.inferencePools, key)
	s.inferencePoolMutex.Unlock()

	klog.V(4).Infof("Deleted InferencePool: %s", key)
	return nil
}

func (s *store) GetInferencePool(key string) *inferencev1.InferencePool {
	s.inferencePoolMutex.RLock()
	defer s.inferencePoolMutex.RUnlock()

	return s.inferencePools[key]
}

func (s *store) GetAllInferencePools() []*inferencev1.InferencePool {
	s.inferencePoolMutex.RLock()
	defer s.inferencePoolMutex.RUnlock()

	var result []*inferencev1.InferencePool
	for _, inferencePool := range s.inferencePools {
		result = append(result, inferencePool)
	}
	return result
}

func (s *store) GetPodsByInferencePool(name types.NamespacedName) ([]*PodInfo, error) {
	key := fmt.Sprintf("%s/%s", name.Namespace, name.Name)

	s.inferencePoolMutex.RLock()
	ip, exists := s.inferencePools[key]
	s.inferencePoolMutex.RUnlock()

	if !exists {
		return nil, fmt.Errorf("inferencepool not found: %v", name)
	}

	// Convert LabelSelector to metav1.LabelSelector for compatibility
	matchLabels := make(map[string]string)
	for k, v := range ip.Spec.Selector.MatchLabels {
		matchLabels[string(k)] = string(v)
	}
	labelSelector := &metav1.LabelSelector{
		MatchLabels: matchLabels,
	}
	selector, err := metav1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector: %w", err)
	}

	var pods []*PodInfo
	s.pods.Range(func(key, value interface{}) bool {
		podInfo := value.(*PodInfo)
		pod := podInfo.GetPod()
		if pod != nil && pod.Namespace == name.Namespace && selector.Matches(labels.Set(podInfo.GetPodLabels())) {
			pods = append(pods, podInfo)
		}
		return true
	})

	return pods, nil
}

// HTTPRoute methods (using standard Gateway API)

func (s *store) AddOrUpdateHTTPRoute(httpRoute *gatewayv1.HTTPRoute) error {
	key := fmt.Sprintf("%s/%s", httpRoute.Namespace, httpRoute.Name)

	s.httpRouteMutex.Lock()
	s.httpRoutes[key] = httpRoute

	// Update gateway routes mapping
	for _, parentRef := range httpRoute.Spec.ParentRefs {
		if parentRef.Kind != nil && *parentRef.Kind == "Gateway" {
			gatewayName := string(parentRef.Name)
			gatewayNamespace := httpRoute.Namespace
			if parentRef.Namespace != nil {
				gatewayNamespace = string(*parentRef.Namespace)
			}
			gatewayKey := fmt.Sprintf("%s/%s", gatewayNamespace, gatewayName)

			if s.gatewayRoutes[gatewayKey] == nil {
				s.gatewayRoutes[gatewayKey] = sets.New[string]()
			}
			s.gatewayRoutes[gatewayKey].Insert(key)
		}
	}

	s.httpRouteMutex.Unlock()

	klog.V(4).Infof("Added or updated HTTPRoute: %s", key)
	return nil
}

func (s *store) DeleteHTTPRoute(key string) error {
	s.httpRouteMutex.Lock()
	_, exists := s.httpRoutes[key]
	if exists {
		// Remove from gateway routes mapping
		for gatewayKey, routeSet := range s.gatewayRoutes {
			routeSet.Delete(key)
			if routeSet.IsEmpty() {
				delete(s.gatewayRoutes, gatewayKey)
			}
		}
		delete(s.httpRoutes, key)
	}
	s.httpRouteMutex.Unlock()

	if exists {
		klog.V(4).Infof("Deleted HTTPRoute: %s", key)
	}
	return nil
}

func (s *store) GetHTTPRoute(key string) *gatewayv1.HTTPRoute {
	s.httpRouteMutex.RLock()
	defer s.httpRouteMutex.RUnlock()

	return s.httpRoutes[key]
}

func (s *store) GetAllHTTPRoutes() []*gatewayv1.HTTPRoute {
	s.httpRouteMutex.RLock()
	defer s.httpRouteMutex.RUnlock()

	var result []*gatewayv1.HTTPRoute
	for _, httpRoute := range s.httpRoutes {
		result = append(result, httpRoute)
	}
	return result
}

func (s *store) GetHTTPRoutesByGateway(gatewayKey string) []*gatewayv1.HTTPRoute {
	s.httpRouteMutex.RLock()
	defer s.httpRouteMutex.RUnlock()

	var result []*gatewayv1.HTTPRoute
	if routeSet, exists := s.gatewayRoutes[gatewayKey]; exists {
		for routeKey := range routeSet {
			if hr, ok := s.httpRoutes[routeKey]; ok {
				result = append(result, hr)
			}
		}
	}
	return result
}
