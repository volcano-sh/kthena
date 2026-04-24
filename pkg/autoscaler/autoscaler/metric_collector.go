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

package autoscaler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
	"github.com/volcano-sh/kthena/pkg/autoscaler/histogram"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	inferControllerUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type MetricCollector struct {
	PastHistograms        *datastructure.SnapshotSlidingWindow[map[string]HistogramInfo]
	Target                *v1alpha1.Target
	Scope                 Scope
	WatchMetricList       sets.String
	MetricTargets         map[string]float64
	// RouterMetricsEndpoint is the optional endpoint of the router's metrics endpoint for scale-from-zero
	// Format: http://<router-service-name>.<namespace>.svc.cluster.local:<port>/metrics
	RouterMetricsEndpoint string
}

func NewMetricCollector(target *v1alpha1.Target, binding *v1alpha1.AutoscalingPolicyBinding, metricTargets map[string]float64) *MetricCollector {
	// Get router metrics endpoint from environment variable if set
	routerMetricsEndpoint := os.Getenv("ROUTER_METRICS_ENDPOINT")

	return &MetricCollector{
		PastHistograms: datastructure.NewSnapshotSlidingWindow[map[string]HistogramInfo](util.SecondToTimestamp(util.SloQuantileSlidingWindowSeconds), util.SecondToTimestamp(util.SloQuantileDataKeepSeconds)),
		Target:         target,
		Scope: Scope{
			Namespace:      binding.Namespace,
			OwnedBindingId: binding.UID,
		},
		MetricTargets:         metricTargets,
		WatchMetricList:       util.ExtractKeysToSet(metricTargets),
		RouterMetricsEndpoint: routerMetricsEndpoint,
	}
}

type HistogramInfo struct {
	PodStartTime *metav1.Time
	HistogramMap map[string]*histogram.Snapshot
}

type Scope struct {
	Namespace      string
	OwnedBindingId types.UID
}

type InstanceInfo struct {
	IsReady    bool
	IsFailed   bool
	MetricsMap algorithm.Metrics
}

type Generations struct {
	AutoscalePolicyGeneration int64
	BindingGeneration         int64
}

func GetMetricTargets(autoscalePolicy *v1alpha1.AutoscalingPolicy) algorithm.Metrics {
	metricTargets := algorithm.Metrics{}
	if autoscalePolicy == nil {
		klog.Warning("autoscalePolicy is nil, can't get metricTargets")
		return metricTargets
	}

	for _, metric := range autoscalePolicy.Spec.Metrics {
		metricTargets[metric.MetricName] = metric.TargetValue.AsFloat64Slow()
	}
	return metricTargets
}

func (collector *MetricCollector) UpdateMetrics(ctx context.Context, podLister listerv1.PodLister) (unreadyInstancesCount int32, readyInstancesMetric algorithm.Metrics, externalMetrics algorithm.Metrics, err error) {
	// Get pod list which will be invoked api to get metrics
	unreadyInstancesCount = int32(0)
	readyInstancesMetric = make(algorithm.Metrics)
	externalMetrics = make(algorithm.Metrics)

	pods, err := util.GetMetricPods(podLister, collector.Scope.Namespace, collector.Target)
	if err != nil {
		klog.Errorf("list watched pod error: %v in namespace: %s, labels: %v", err, collector.Scope.Namespace, collector.Target.MetricEndpoint)
		return
	}

	if len(pods) > 0 {
		currentHistograms := make(map[string]HistogramInfo)
		instanceInfo := collector.fetchMetricsFromPods(ctx, pods, &currentHistograms)
		klog.V(10).InfoS("finish to processInstance", "instanceInfo.isFailed", instanceInfo.IsFailed)
		klog.V(10).InfoS("finish to processInstance", "instanceInfo.isReady", instanceInfo.IsReady)
		klog.V(10).InfoS("finish to processInstance", "instanceInfo.metricsMap", instanceInfo.MetricsMap)
		if instanceInfo.IsFailed {
			klog.Warningf("some pod of %s are failed in namespace: %s.", collector.Scope, collector.Scope.Namespace)
			return
		}

		if !instanceInfo.IsReady {
			unreadyInstancesCount++
			klog.Warningf("some pod of %s are not ready in namespace: %s.", collector.Scope, collector.Scope.Namespace)
			return
		}
		readyInstancesMetric = instanceInfo.MetricsMap
		collector.PastHistograms.Append(currentHistograms)
	} else {
		klog.V(4).Infof("no pods available for metric collection, will use external metrics if available")
		// Try to fetch external pending request metrics from router if endpoint is configured
		if collector.RouterMetricsEndpoint != "" {
			routerMetrics, err := collector.fetchMetricsFromRouter(ctx)
			if err == nil {
				for k, v := range routerMetrics {
					externalMetrics[k] = v
				}
			} else {
				klog.Warningf("failed to fetch metrics from router endpoint %s: %v", collector.RouterMetricsEndpoint, err)
			}
		}
	}

	return
}

func (collector *MetricCollector) fetchMetricsFromPods(ctx context.Context, pods []*corev1.Pod, currentHistograms *map[string]HistogramInfo) InstanceInfo {
	instanceInfo := InstanceInfo{true, false, make(algorithm.Metrics)}
	pastHistograms, ok := collector.PastHistograms.GetLastUnfreshSnapshot()
	if !ok {
		pastHistograms = make(map[string]HistogramInfo)
	}
	klog.InfoS("fetch metrics from pods start")
	for _, pod := range pods {
		func() {
			instanceInfo.IsReady = instanceInfo.IsReady && inferControllerUtils.IsPodRunningAndReady(pod)
			instanceInfo.IsFailed = instanceInfo.IsFailed || util.IsPodFailed(pod) || inferControllerUtils.ContainerRestarted(pod)

			pastValue, ok := pastHistograms[pod.Name]
			var pastHistogramMap map[string]*histogram.Snapshot
			if !ok || pod.Status.StartTime == nil || pastValue.PodStartTime == nil || !pod.Status.StartTime.Equal(pastValue.PodStartTime) {
				pastHistogramMap = make(map[string]*histogram.Snapshot)
			} else {
				pastHistogramMap = pastValue.HistogramMap
			}

			currentHistogramMap := make(map[string]*histogram.Snapshot)
			ip := pod.Status.PodIP
			podCtx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
			defer cancel()
			url := fmt.Sprintf("http://%s:%d%s", ip, collector.Target.MetricEndpoint.Port, collector.Target.MetricEndpoint.Uri)

			req, _ := http.NewRequestWithContext(podCtx, http.MethodGet, url, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				klog.Errorf("get metric response error: %v", err)
				return
			}
			if resp == nil || !util.IsRequestSuccess(resp.StatusCode) || resp.Body == nil {
				klog.Errorf("get metric response is invalid")
				return
			}
			defer resp.Body.Close()

			bodyStr, err := io.ReadAll(resp.Body)
			if err != nil {
				klog.Errorf("get metrics read response error: %v", err)
				return
			}
			result := string(bodyStr)
			collector.processPrometheusString(result, pastHistogramMap, currentHistogramMap, instanceInfo.MetricsMap)
			(*currentHistograms)[pod.Name] = HistogramInfo{
				PodStartTime: pod.Status.StartTime,
				HistogramMap: currentHistogramMap,
			}
		}()
	}
	return instanceInfo
}

func (collector *MetricCollector) processPrometheusString(metricStr string, pastHistograms map[string]*histogram.Snapshot, currentHistograms map[string]*histogram.Snapshot, instanceMetricMap algorithm.Metrics) {
	reader := strings.NewReader(metricStr)
	decoder := expfmt.NewDecoder(reader, expfmt.NewFormat(expfmt.TypeTextPlain))
	for {
		mf := &io_prometheus_client.MetricFamily{}
		err := decoder.Decode(mf)
		if err == io.EOF {
			break
		}
		if err != nil {
			klog.Errorf("error decoding metric: %v", err)
			continue
		}
		if len(mf.Metric) < 1 {
			klog.Errorf("metric is invalid")
			continue
		}

		if _, ok := collector.WatchMetricList[mf.GetName()]; !ok {
			klog.V(4).Infof("metric name: %s is not matched with metricTargets", mf.GetName())
			continue
		}

		metric := mf.Metric[0]
		switch mf.GetType() {
		case io_prometheus_client.MetricType_COUNTER:
			addMetric(instanceMetricMap, mf.GetName(), metric.GetCounter().GetValue())
		case io_prometheus_client.MetricType_GAUGE:
			addMetric(instanceMetricMap, mf.GetName(), metric.GetGauge().GetValue())
		case io_prometheus_client.MetricType_HISTOGRAM:
			hist := metric.GetHistogram()
			snapshot := histogram.NewSnapshotOfHistogram(hist)
			currentHistograms[mf.GetName()] = snapshot

			if pastHistograms == nil {
				klog.Warning("pastHistograms is nil")
				continue
			}
			past, ok := pastHistograms[mf.GetName()]
			if !ok {
				past = histogram.NewDefaultSnapshot()
			}
			quantileInDiffMetric, err := histogram.QuantileInDiff(util.SloQuantilePercentile, snapshot, past)
			if err == nil {
				addMetric(instanceMetricMap, mf.GetName(), quantileInDiffMetric)
			}
		default:
			klog.InfoS("metric type is out of range", "type", mf.GetType())
		}
	}

	for key := range collector.WatchMetricList {
		if _, ok := instanceMetricMap[key]; !ok {
			instanceMetricMap[key] = 0
		}
	}
}

func addMetric(instanceMetricMap algorithm.Metrics, metricName string, metricValue float64) {
	if oldValue, ok := instanceMetricMap[metricName]; ok {
		instanceMetricMap[metricName] = oldValue + metricValue
	} else {
		instanceMetricMap[metricName] = metricValue
	}
}

// fetchMetricsFromRouter fetches pending request metrics from the router's metrics endpoint
func (collector *MetricCollector) fetchMetricsFromRouter(ctx context.Context) (algorithm.Metrics, error) {
	metrics := make(algorithm.Metrics)

	// Create HTTP request to router metrics endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, collector.RouterMetricsEndpoint, nil)
	if err != nil {
		return metrics, err
	}

	// Send request with timeout
	client := &http.Client{
		Timeout: util.AutoscaleCtxTimeoutSeconds * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return metrics, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return metrics, fmt.Errorf("router metrics endpoint returned non-2xx status: %d", resp.StatusCode)
	}

	// Read response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return metrics, err
	}

	// Parse Prometheus metrics
	decoder := expfmt.NewDecoder(strings.NewReader(string(body)), expfmt.NewFormat(expfmt.TypeTextPlain))
	modelName := collector.Target.TargetRef.Name

	for {
		mf := &io_prometheus_client.MetricFamily{}
		err := decoder.Decode(mf)
		if err == io.EOF {
			break
		}
		if err != nil {
			klog.Warningf("error decoding router metric: %v", err)
			continue
		}

		// We only care about the fairness queue size metric for scale-from-zero
		if mf.GetName() == "kthena_router_fairness_queue_size" {
			for _, metric := range mf.GetMetric() {
				// Check if this metric is for the current model
				for _, label := range metric.GetLabel() {
					if label.GetName() == "model" && label.GetValue() == modelName {
						// Add the queue size as pending request metric
						addMetric(metrics, "pending_requests", metric.GetGauge().GetValue())
						break
					}
				}
			}
		}
	}

	return metrics, nil
}
