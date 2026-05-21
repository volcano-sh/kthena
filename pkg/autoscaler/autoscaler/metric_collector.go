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
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
	"github.com/volcano-sh/kthena/pkg/autoscaler/histogram"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	inferControllerUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
)

type MetricCollector struct {
	PastHistograms *datastructure.SnapshotSlidingWindow[map[string]HistogramInfo]
	Target         *v1alpha1.Target
	Scope          Scope
	MetricTargets  map[string]float64
}

func NewMetricCollector(target *v1alpha1.Target, binding *v1alpha1.AutoscalingPolicyBinding, metricTargets map[string]float64) *MetricCollector {
	namespace := target.TargetRef.Namespace
	if namespace == "" {
		namespace = binding.Namespace
	}
	return &MetricCollector{
		PastHistograms: datastructure.NewSnapshotSlidingWindow[map[string]HistogramInfo](util.SecondToTimestamp(util.SloQuantileSlidingWindowSeconds), util.SecondToTimestamp(util.SloQuantileDataKeepSeconds)),
		Target:         target,
		Scope: Scope{
			Namespace:      namespace,
			OwnedBindingId: binding.UID,
		},
		MetricTargets: metricTargets,
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
		metricTargets[metric.Name] = metric.TargetValue.AsFloat64Slow()
	}
	return metricTargets
}

func (collector *MetricCollector) UpdateMetrics(
	ctx context.Context,
	podLister listerv1.PodLister,
	targetMetricSources map[string]v1alpha1.MetricSource,
) (unreadyInstancesCount int32, readyInstancesMetric algorithm.Metrics, externalMetrics algorithm.Metrics, err error) {
	readyInstancesMetric = make(algorithm.Metrics)
	externalMetrics = make(algorithm.Metrics)

	pastHistograms, ok := collector.PastHistograms.GetLastUnfreshSnapshot()
	if !ok {
		pastHistograms = make(map[string]HistogramInfo)
	}
	currentHistograms := make(map[string]HistogramInfo)

	for metricName := range collector.MetricTargets {
		source, exists := targetMetricSources[metricName]
		if !exists {
			klog.Warningf("metric source missing for metric %s in target %s", metricName, collector.Target.TargetRef.Name)
			continue
		}
		sourceType := source.Type
		if sourceType == "" {
			sourceType = v1alpha1.PodMetricSourceType
		}

		switch sourceType {
		case v1alpha1.PodMetricSourceType:
			podSource := source.Pod
			if podSource == nil {
				podSource = &v1alpha1.PodMetricSource{}
			}
			metricValue, anyUnready, failed, collectErr := collector.collectPodMetric(ctx, podLister, metricName, podSource, pastHistograms, currentHistograms)
			if collectErr != nil {
				klog.Warningf("collect pod metric %s failed: %v", metricName, collectErr)
				continue
			}
			if failed {
				klog.Warningf("collect pod metric %s skipped because pod failed/restarted", metricName)
				continue
			}
			if anyUnready {
				unreadyInstancesCount = 1
				continue
			}
			readyInstancesMetric[metricName] = metricValue
		case v1alpha1.PrometheusMetricSourceType:
			if source.Prometheus == nil {
				klog.Warningf("prometheus source is nil for metric %s", metricName)
				continue
			}
			metricValue, promErr := collector.fetchPrometheusMetric(ctx, source.Prometheus)
			if promErr != nil {
				klog.Warningf("collect prometheus metric %s failed: %v", metricName, promErr)
				continue
			}
			externalMetrics[metricName] = metricValue
		default:
			klog.Warningf("unknown metric source type %q for metric %s", sourceType, metricName)
		}
	}

	collector.PastHistograms.Append(currentHistograms)
	return
}

func (collector *MetricCollector) collectPodMetric(
	ctx context.Context,
	podLister listerv1.PodLister,
	metricName string,
	podSource *v1alpha1.PodMetricSource,
	pastHistograms map[string]HistogramInfo,
	currentHistograms map[string]HistogramInfo,
) (metricValue float64, anyUnready bool, failed bool, err error) {
	pods, err := util.GetMetricPods(podLister, collector.Scope.Namespace, collector.Target, podSource)
	if err != nil {
		return 0, false, false, err
	}
	if len(pods) == 0 {
		return 0, false, false, fmt.Errorf("pod list is empty")
	}

	for _, pod := range pods {
		if !inferControllerUtils.IsPodRunningAndReady(pod) {
			anyUnready = true
		}
		if util.IsPodFailed(pod) || inferControllerUtils.ContainerRestarted(pod) {
			failed = true
		}
	}
	if failed || anyUnready {
		return
	}

	for _, pod := range pods {
		pastValue, ok := pastHistograms[pod.Name]
		pastHistogramMap := make(map[string]*histogram.Snapshot)
		if ok && pod.Status.StartTime != nil && pastValue.PodStartTime != nil && pod.Status.StartTime.Equal(pastValue.PodStartTime) {
			pastHistogramMap = pastValue.HistogramMap
		}

		currentValue := currentHistograms[pod.Name]
		currentHistogramMap := currentValue.HistogramMap
		if currentHistogramMap == nil {
			currentHistogramMap = make(map[string]*histogram.Snapshot)
		}

		url := collector.buildPodMetricURL(pod, podSource)
		podCtx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
		req, reqErr := http.NewRequestWithContext(podCtx, http.MethodGet, url, nil)
		if reqErr != nil {
			cancel()
			return 0, false, false, reqErr
		}
		resp, reqErr := http.DefaultClient.Do(req)
		if reqErr != nil {
			cancel()
			return 0, false, false, reqErr
		}
		if resp == nil || !util.IsRequestSuccess(resp.StatusCode) || resp.Body == nil {
			cancel()
			return 0, false, false, fmt.Errorf("invalid metric response for pod %s", pod.Name)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if readErr != nil {
			return 0, false, false, readErr
		}

		v, snapshot, found, parseErr := parseSinglePrometheusMetric(string(body), metricName, pastHistogramMap)
		if parseErr != nil {
			return 0, false, false, parseErr
		}
		if found {
			metricValue += v
		}
		if snapshot != nil {
			currentHistogramMap[metricName] = snapshot
		}
		currentHistograms[pod.Name] = HistogramInfo{
			PodStartTime: pod.Status.StartTime,
			HistogramMap: currentHistogramMap,
		}
	}
	return
}

func (collector *MetricCollector) buildPodMetricURL(pod *corev1.Pod, podSource *v1alpha1.PodMetricSource) string {
	uri := podSource.Uri
	if uri == "" {
		uri = "/metrics"
	}
	port := podSource.Port
	if port == 0 {
		port = 8100
	}
	return fmt.Sprintf("http://%s:%d%s", pod.Status.PodIP, port, uri)
}

func parseSinglePrometheusMetric(metricStr string, metricName string, pastHistograms map[string]*histogram.Snapshot) (value float64, histSnapshot *histogram.Snapshot, found bool, err error) {
	reader := strings.NewReader(metricStr)
	decoder := expfmt.NewDecoder(reader, expfmt.NewFormat(expfmt.TypeTextPlain))
	for {
		mf := &io_prometheus_client.MetricFamily{}
		decodeErr := decoder.Decode(mf)
		if decodeErr == io.EOF {
			break
		}
		if decodeErr != nil {
			return 0, nil, false, decodeErr
		}
		if mf.GetName() != metricName || len(mf.Metric) < 1 {
			continue
		}

		metric := mf.Metric[0]
		switch mf.GetType() {
		case io_prometheus_client.MetricType_COUNTER:
			return metric.GetCounter().GetValue(), nil, true, nil
		case io_prometheus_client.MetricType_GAUGE:
			return metric.GetGauge().GetValue(), nil, true, nil
		case io_prometheus_client.MetricType_HISTOGRAM:
			hist := metric.GetHistogram()
			snapshot := histogram.NewSnapshotOfHistogram(hist)
			past := histogram.NewDefaultSnapshot()
			if pastHistograms != nil {
				if old, ok := pastHistograms[metricName]; ok {
					past = old
				}
			}
			quantile, qErr := histogram.QuantileInDiff(util.SloQuantilePercentile, snapshot, past)
			if qErr != nil {
				return 0, snapshot, false, qErr
			}
			return quantile, snapshot, true, nil
		default:
			return 0, nil, false, fmt.Errorf("unsupported metric type %v", mf.GetType())
		}
	}
	return 0, nil, false, nil
}

func (collector *MetricCollector) fetchPrometheusMetric(ctx context.Context, src *v1alpha1.PrometheusMetricSource) (float64, error) {
	transport := &http.Transport{}
	if src.Auth != nil && src.Auth.TLSConfig != nil {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: src.Auth.TLSConfig.InsecureSkipVerify}
		if src.Auth.TLSConfig.CASecret != nil {
			klog.Warningf("prometheus CASecret is configured for metric target %s but CA secret loading is not implemented yet", collector.Target.TargetRef.Name)
		}
	}
	if src.Auth != nil && src.Auth.BearerTokenSecret != nil {
		klog.Warningf("prometheus bearer token secret is configured for metric target %s but secret loading is not implemented yet", collector.Target.TargetRef.Name)
	}

	client, err := promapi.NewClient(promapi.Config{
		Address:      src.ServerURL,
		RoundTripper: transport,
	})
	if err != nil {
		return 0, err
	}
	api := prometheusv1.NewAPI(client)
	result, warnings, err := api.Query(ctx, src.Query, time.Now())
	if err != nil {
		return 0, err
	}
	for _, warning := range warnings {
		klog.Warningf("prometheus query warning for %s: %s", collector.Target.TargetRef.Name, warning)
	}

	switch v := result.(type) {
	case *model.Scalar:
		return float64(v.Value), nil
	case model.Vector:
		if len(v) != 1 {
			return 0, fmt.Errorf("prometheus query must return a single sample vector, got %d", len(v))
		}
		return float64(v[0].Value), nil
	default:
		return 0, fmt.Errorf("unsupported prometheus query result type %T", result)
	}
}

func addMetric(instanceMetricMap algorithm.Metrics, metricName string, metricValue float64) {
	if oldValue, ok := instanceMetricMap[metricName]; ok {
		instanceMetricMap[metricName] = oldValue + metricValue
	} else {
		instanceMetricMap[metricName] = metricValue
	}
}
