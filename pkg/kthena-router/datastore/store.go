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
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dto "github.com/prometheus/client_model/go"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	aiv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/networking/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/kthena-router/backend"
	"github.com/volcano-sh/kthena/pkg/kthena-router/utils"
)

var (
	metricsName = []string{
		utils.GPUCacheUsage,
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
	// Configuration constants for fairness scheduling
	defaultQueueQPS = 100
	uppdateInterval = 1 * time.Second
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

	ModelName  string
	ModelRoute *aiv1alpha1.ModelRoute
}

// CallbackFunc is the type of function that can be registered as a callback
type CallbackFunc func(data EventData)

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
	// Refresh Store and ModelServer when delete a pod
	DeletePod(podName types.NamespacedName) error

	// New methods for routing functionality
	MatchModelServer(modelName string, request *http.Request) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error)

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
	// GetTokenCount returns the token count for a user and model
	GetTokenCount(userId, modelName string) (float64, error)
	// UpdateTokenCount updates token usage for a user and model
	UpdateTokenCount(userId, modelName string, inputTokens, outputTokens float64) error

	// Enqueue adds a request to the fair queue
	Enqueue(*Request) error

	// GetRequestWaitingQueueStats returns per-model queue lengths
	GetRequestWaitingQueueStats() []QueueStat

	// Gateway methods (using standard Gateway API)
	AddOrUpdateGateway(gateway interface{}) error
	DeleteGateway(key string) error
	GetGateway(key string) interface{}
	GetGatewaysByNamespace(namespace string) []interface{}
	GetAllGateways() []interface{}

	// GatewayClass methods (using standard Gateway API)
	AddOrUpdateGatewayClass(gatewayClass interface{}) error
	DeleteGatewayClass(key string) error
	GetGatewayClass(key string) interface{}

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

	mutex sync.RWMutex // Protects concurrent access to Models and modelServer fields
	// Protected fields - use accessor methods for thread-safe access
	models      sets.Set[string]               // running models. Including base model and lora adapters.
	modelServer sets.Set[types.NamespacedName] // The modelservers this pod belongs to
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

	routeMutex sync.RWMutex
	// Model routing fields
	routeInfo  map[string]*modelRouteInfo
	routes     map[string]*aiv1alpha1.ModelRoute
	loraRoutes map[string]*aiv1alpha1.ModelRoute

	// Gateway fields (using standard Gateway API)
	gatewayMutex sync.RWMutex
	gateways     map[string]*gatewayv1.Gateway // key: namespace/name, value: *gatewayv1.Gateway

	// GatewayClass fields (using standard Gateway API)
	gatewayClassMutex sync.RWMutex
	gatewayClasses    map[string]*gatewayv1.GatewayClass // key: name (cluster-scoped), value: *gatewayv1.GatewayClass

	// New fields for callback management
	callbacks map[string][]CallbackFunc

	// initialSynced is used to indicate whether all the resources has been processed and storred into this store.
	initialSynced *atomic.Bool
	// model -> RequestPriorityQueue
	requestWaitingQueue sync.Map
	tokenTracker        TokenTracker
}

func New() Store {
	return &store{
		modelServer:         sync.Map{},
		pods:                sync.Map{},
		routeInfo:           make(map[string]*modelRouteInfo),
		routes:              make(map[string]*aiv1alpha1.ModelRoute),
		loraRoutes:          make(map[string]*aiv1alpha1.ModelRoute),
		gateways:            make(map[string]*gatewayv1.Gateway),
		gatewayClasses:      make(map[string]*gatewayv1.GatewayClass),
		callbacks:           make(map[string][]CallbackFunc),
		initialSynced:       &atomic.Bool{},
		requestWaitingQueue: sync.Map{},
		// Create token tracker with environment-based configuration
		tokenTracker: createTokenTracker(),
	}
}

func (s *store) Run(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				s.pods.Range(func(key, value any) bool {
					if p, ok := value.(*PodInfo); ok {
						s.updatePodMetrics(p)
						s.updatePodModels(p)
					}
					return true
				})
				s.initialSynced.Store(true)
				time.Sleep(uppdateInterval)
			}
		}
	}()
}
func (s *store) GetTokenCount(userID, model string) (float64, error) {
	return s.tokenTracker.GetTokenCount(userID, model)
}

func (s *store) UpdateTokenCount(userID, model string, inputTokens, outputTokens float64) error {
	return s.tokenTracker.UpdateTokenCount(userID, model, inputTokens, outputTokens)
}

func (s *store) Enqueue(req *Request) error {
	modelName := req.ModelName
	var queue *RequestPriorityQueue
	val, ok := s.requestWaitingQueue.Load(modelName)
	if ok {
		queue, _ = val.(*RequestPriorityQueue)
	} else {
		newQueue := NewRequestPriorityQueue(nil)
		val, ok = s.requestWaitingQueue.LoadOrStore(modelName, newQueue)
		if !ok {
			go newQueue.Run(context.TODO(), defaultQueueQPS)
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

func (s *store) AddOrUpdateModelServer(ms *aiv1alpha1.ModelServer, pods sets.Set[types.NamespacedName]) error {
	name := utils.GetNamespaceName(ms)
	var modelServerObj *modelServer
	if value, ok := s.modelServer.Load(name); !ok {
		modelServerObj = newModelServer(ms)
	} else {
		modelServerObj = value.(*modelServer)
		modelServerObj.modelServer = ms
	}

	if len(pods) != 0 {
		// do not operate s.pods here, which are done within pod handler
		modelServerObj.pods = pods
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
		return value.(*modelServer).modelServer
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
	newPodInfo := &PodInfo{
		Pod:         pod,
		modelServer: sets.Set[types.NamespacedName]{},
		models:      sets.New[string](),
	}

	for _, ms := range modelServers {
		modelServerName := utils.GetNamespaceName(ms)
		newPodInfo.AddModelServer(modelServerName)
		// NOTE: even if a pod belongs to multiple model servers, the backend should be the same
		newPodInfo.engine = string(ms.Spec.InferenceEngine)
		if value, ok := s.modelServer.Load(modelServerName); ok {
			ms := value.(*modelServer)
			ms.addPod(podName)
			// Categorize the pod for PDGroup scheduling
			ms.categorizePodForPDGroup(podName, pod.Labels)
		}
	}

	var oldPodInfo *PodInfo
	if value, ok := s.pods.Load(podName); ok {
		oldPodInfo = value.(*PodInfo)
		oldModelServers := oldPodInfo.GetModelServers()
		// Handle the case where the pod is no longer belong to some model servers
		for msName := range oldModelServers.Difference(newPodInfo.modelServer) {
			if value, ok := s.modelServer.Load(msName); ok {
				ms := value.(*modelServer)
				ms.deletePod(podName)
				// Remove from PDGroup categorizations
				ms.removePodFromPDGroups(podName, oldPodInfo.Pod.Labels)
			}
		}
	}

	s.pods.Store(podName, newPodInfo)

	if oldPodInfo == nil {
		s.updatePodMetrics(newPodInfo)
		s.updatePodModels(newPodInfo)
	}

	return nil
}

func (s *store) DeletePod(podName types.NamespacedName) error {
	if value, ok := s.pods.Load(podName); ok {
		pod := value.(*PodInfo)
		modelServers := pod.GetModelServers()
		for modelServerName := range modelServers {
			if value, ok := s.modelServer.Load(modelServerName); ok {
				ms := value.(*modelServer)
				ms.deletePod(podName)
				// Remove from PDGroup categorizations
				ms.removePodFromPDGroups(podName, pod.Pod.Labels)
			} else {
				klog.V(4).Infof("model server %s not found for pod %s, maybe already deleted", modelServerName, podName)
			}
		}
		s.pods.Delete(podName)
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
		s.routes[mr.Spec.ModelName] = mr
	}

	for _, lora := range mr.Spec.LoraAdapters {
		s.loraRoutes[lora] = mr
	}
	s.routeMutex.Unlock()

	s.triggerCallbacks("ModelRoute", EventData{
		EventType:  EventUpdate,
		ModelName:  mr.Spec.ModelName,
		ModelRoute: mr,
	})
	return nil
}

func (s *store) DeleteModelRoute(namespacedName string) error {
	s.routeMutex.Lock()
	info := s.routeInfo[namespacedName]
	var modelName string
	if info != nil {
		modelName = info.model
		delete(s.routes, info.model)
		for _, lora := range info.loras {
			delete(s.loraRoutes, lora)
		}
	}
	delete(s.routeInfo, namespacedName)
	s.routeMutex.Unlock()
	if modelName != "" {
		// Clean up associated waiting queue if exists
		val, _ := s.requestWaitingQueue.LoadAndDelete(modelName)
		if val != nil {
			queue, _ := val.(*RequestPriorityQueue)
			queue.Close()
			klog.Infof("deleted waiting queue for model %s", modelName)
		}
	}

	// Trigger callbacks outside the lock to avoid potential deadlocks
	s.triggerCallbacks("ModelRoute", EventData{
		EventType:  EventDelete,
		ModelName:  modelName,
		ModelRoute: nil,
	})
	return nil
}

func (s *store) MatchModelServer(model string, req *http.Request) (types.NamespacedName, bool, *aiv1alpha1.ModelRoute, error) {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	var isLora bool
	mr, ok := s.routes[model]
	if !ok {
		mr, ok = s.loraRoutes[model]
		if !ok {
			return types.NamespacedName{}, false, nil, fmt.Errorf("not found route rules for model %s", model)
		}
		isLora = true
	}

	// Check parentRefs if specified
	if len(mr.Spec.ParentRefs) > 0 {
		if !s.matchesParentRefs(mr) {
			return types.NamespacedName{}, false, nil, fmt.Errorf("ModelRoute %s/%s does not match any Gateway", mr.Namespace, mr.Name)
		}
	}

	rule, err := s.selectRule(model, req, mr.Spec.Rules)
	if err != nil {
		return types.NamespacedName{}, false, nil, fmt.Errorf("failed to select route rule: %v", err)
	}

	dst, err := s.selectDestination(rule.TargetModels)
	if err != nil {
		return types.NamespacedName{}, false, nil, fmt.Errorf("failed to select destination: %v", err)
	}

	return types.NamespacedName{Namespace: mr.Namespace, Name: dst.ModelServerName}, isLora, mr, nil
}

// matchesParentRefs checks if the ModelRoute matches any of its parentRefs
func (s *store) matchesParentRefs(mr *aiv1alpha1.ModelRoute) bool {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	for _, parentRef := range mr.Spec.ParentRefs {
		// Get namespace from parentRef, default to ModelRoute's namespace
		namespace := mr.Namespace
		if parentRef.Namespace != nil {
			namespace = string(*parentRef.Namespace)
		}

		// Get name from parentRef
		name := string(parentRef.Name)
		key := fmt.Sprintf("%s/%s", namespace, name)
		gatewayObj := s.gateways[key]
		if gatewayObj != nil {
			// If sectionName is specified, check if the listener exists
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
	weightedSlice, err := toWeightedSlice(targets)
	if err != nil {
		return nil, err
	}

	index := selectFromWeightedSlice(weightedSlice)

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

func selectFromWeightedSlice(weights []uint32) int {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	totalWeight := 0
	for _, weight := range weights {
		totalWeight += int(weight)
	}

	randomNum := rng.Intn(totalWeight)

	for i, weight := range weights {
		randomNum -= int(weight)
		if randomNum < 0 {
			return i
		}
	}

	return 0
}

func (s *store) updatePodMetrics(pod *PodInfo) {
	if pod.engine == "" {
		klog.V(2).Info("failed to find backend in pod")
		return
	}

	previousHistogram := getPreviousHistogram(pod)
	gaugeMetrics, histogramMetrics := backend.GetPodMetrics(pod.engine, pod.Pod, previousHistogram)
	updateGaugeMetricsInfo(pod, gaugeMetrics)
	updateHistogramMetrics(pod, histogramMetrics)
}

func (s *store) updatePodModels(podInfo *PodInfo) {
	if podInfo.engine == "" {
		klog.V(2).Info("failed to find backend in pod")
		return
	}

	models, err := backend.GetPodModels(podInfo.engine, podInfo.Pod)
	if err != nil {
		klog.V(4).Infof("failed to get models of pod %s/%s", podInfo.Pod.GetNamespace(), podInfo.Pod.GetName())
	}

	podInfo.UpdateModels(models)
}

func getPreviousHistogram(podinfo *PodInfo) map[string]*dto.Histogram {
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
	updateFuncs := map[string]func(float64){
		utils.GPUCacheUsage: func(f float64) {
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

// PodInfo methods for thread-safe access to models and modelServer fields

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
	return p.engine
}

// Debug interface implementations

// GetAllModelRoutes returns all ModelRoutes in the store
func (s *store) GetAllModelRoutes() map[string]*aiv1alpha1.ModelRoute {
	s.routeMutex.RLock()
	defer s.routeMutex.RUnlock()

	result := make(map[string]*aiv1alpha1.ModelRoute)
	for key, info := range s.routeInfo {
		if info.model != "" {
			if route, ok := s.routes[info.model]; ok {
				result[key] = route
			}
		}
		// Also check lora routes
		for _, lora := range info.loras {
			if route, ok := s.loraRoutes[lora]; ok {
				result[key] = route
				break // Same route for all loras in a ModelRoute
			}
		}
	}
	return result
}

// GetAllModelServers returns all ModelServers in the store
func (s *store) GetAllModelServers() map[types.NamespacedName]*aiv1alpha1.ModelServer {
	result := make(map[types.NamespacedName]*aiv1alpha1.ModelServer)
	s.modelServer.Range(func(key, value any) bool {
		if namespacedName, ok := key.(types.NamespacedName); ok {
			if ms, ok := value.(*modelServer); ok {
				result[namespacedName] = ms.modelServer
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
		if route, ok := s.routes[info.model]; ok {
			return route
		}
	}

	// Try to find the route from lora adapters
	for _, lora := range info.loras {
		if route, ok := s.loraRoutes[lora]; ok {
			return route
		}
	}

	return nil
}

// Gateway methods (using standard Gateway API)

func (s *store) AddOrUpdateGateway(gateway interface{}) error {
	gw, ok := gateway.(*gatewayv1.Gateway)
	if !ok {
		return fmt.Errorf("invalid gateway type: %T", gateway)
	}

	key := fmt.Sprintf("%s/%s", gw.Namespace, gw.Name)

	s.gatewayMutex.Lock()
	s.gateways[key] = gw
	s.gatewayMutex.Unlock()

	klog.V(4).Infof("Added or updated Gateway: %s", key)

	// Trigger callback outside the lock to avoid potential deadlocks
	s.triggerCallbacks("Gateway", EventData{
		EventType: EventAdd,
		Pod:       types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name},
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
			Pod:       types.NamespacedName{Namespace: namespace, Name: name},
		})
	}

	return nil
}

func (s *store) GetGateway(key string) interface{} {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	return s.gateways[key]
}

func (s *store) GetGatewaysByNamespace(namespace string) []interface{} {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	var result []interface{}
	for key, gateway := range s.gateways {
		if strings.HasPrefix(key, namespace+"/") {
			result = append(result, gateway)
		}
	}
	return result
}

func (s *store) GetAllGateways() []interface{} {
	s.gatewayMutex.RLock()
	defer s.gatewayMutex.RUnlock()

	var result []interface{}
	for _, gateway := range s.gateways {
		result = append(result, gateway)
	}
	return result
}

// GatewayClass methods (using standard Gateway API)

func (s *store) AddOrUpdateGatewayClass(gatewayClass interface{}) error {
	s.gatewayClassMutex.Lock()
	defer s.gatewayClassMutex.Unlock()

	gc, ok := gatewayClass.(*gatewayv1.GatewayClass)
	if !ok {
		return fmt.Errorf("invalid gatewayclass type: %T", gatewayClass)
	}

	key := gc.Name
	s.gatewayClasses[key] = gc
	klog.V(4).Infof("Added or updated GatewayClass: %s", key)
	return nil
}

func (s *store) DeleteGatewayClass(key string) error {
	s.gatewayClassMutex.Lock()
	defer s.gatewayClassMutex.Unlock()

	delete(s.gatewayClasses, key)
	klog.V(4).Infof("Deleted GatewayClass: %s", key)
	return nil
}

func (s *store) GetGatewayClass(key string) interface{} {
	s.gatewayClassMutex.RLock()
	defer s.gatewayClassMutex.RUnlock()

	return s.gatewayClasses[key]
}
