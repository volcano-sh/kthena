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
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	prometheusv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/autoscaler/algorithm"
	"github.com/volcano-sh/kthena/pkg/autoscaler/datastructure"
	"github.com/volcano-sh/kthena/pkg/autoscaler/histogram"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	inferControllerUtils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type MetricCollector struct {
	PastHistograms *datastructure.SnapshotSlidingWindow[map[string]HistogramInfo]
	Target         *v1alpha1.Target
	Scope          Scope
	MetricTargets  map[string]float64
	promClients    map[string]promapi.Client
	promClientsMu  sync.Mutex
}

func NewMetricCollector(target *v1alpha1.Target, policy *v1alpha1.AutoscalingPolicy, metricTargets map[string]float64) *MetricCollector {
	namespace := target.TargetRef.Namespace
	if namespace == "" {
		namespace = policy.Namespace
	}
	return &MetricCollector{
		PastHistograms: datastructure.NewSnapshotSlidingWindow[map[string]HistogramInfo](util.SecondToTimestamp(util.SloQuantileSlidingWindowSeconds), util.SecondToTimestamp(util.SloQuantileDataKeepSeconds)),
		Target:         target,
		Scope: Scope{
			Namespace:     namespace,
			OwnedPolicyId: policy.UID,
		},
		MetricTargets: metricTargets,
		promClients:   make(map[string]promapi.Client),
	}
}

type HistogramInfo struct {
	PodStartTime *metav1.Time
	HistogramMap map[string]*histogram.Snapshot
}

type Scope struct {
	Namespace     string
	OwnedPolicyId types.UID
}

type Generations struct {
	AutoscalePolicyGeneration int64
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
) (unreadyInstancesCountByMetric map[string]int32, readyInstancesMetrics map[string][]algorithm.Metrics, externalMetrics algorithm.Metrics, err error) {
	// Results are keyed by metric. Each metric only sees the pods its own source
	// selects, so a pod that was never asked for a metric is not counted as
	// missing it, and an unready pod only counts for the metrics that select it.
	unreadyInstancesCountByMetric = make(map[string]int32)
	readyInstancesMetrics = make(map[string][]algorithm.Metrics)

	// pastHistograms are the stored snapshots, oldest first. The oldest one is
	// the baseline, just over 60 seconds old. The list is empty until a snapshot
	// is that old, and histograms are then compared with an empty baseline.
	pastHistograms := collector.PastHistograms.GetSnapshotsSinceLastUnfresh()
	currentHistograms := make(map[string]HistogramInfo)

	// Group pod metrics by identical PodMetricSource (uri/port/selector) so we
	// scrape each ready pod endpoint once per reconcile and extract every
	// required metric from the same payload. Prometheus-sourced metrics stay
	// per-metric.
	podGroups, externalMetrics := collector.planMetricSources(ctx, targetMetricSources)

	for _, g := range podGroups {
		values, groupUnreadyPods, collectErr := collector.collectPodMetricsGroup(ctx, podLister, g.podSource, g.specs, pastHistograms, currentHistograms)
		if collectErr != nil {
			klog.Warningf("collect pod metrics for target %s failed: %v", collector.Target.TargetRef.Name, collectErr)
			continue
		}
		groupMetrics := make([]algorithm.Metrics, 0, len(values))
		for _, podKey := range slices.Sorted(maps.Keys(values)) {
			groupMetrics = append(groupMetrics, values[podKey])
		}
		for _, spec := range g.specs {
			readyInstancesMetrics[spec.policyKey] = groupMetrics
			unreadyInstancesCountByMetric[spec.policyKey] = int32(len(groupUnreadyPods))
		}
	}

	collector.PastHistograms.Append(currentHistograms)
	return
}

// planMetricSources groups pod-sourced metrics by identical scrape configuration
// (so each pod endpoint is scraped once) and resolves prometheus-sourced metrics
// immediately.
func (collector *MetricCollector) planMetricSources(
	ctx context.Context,
	targetMetricSources map[string]v1alpha1.MetricSource,
) (podGroups map[string]*podMetricGroup, externalMetrics algorithm.Metrics) {
	podGroups = make(map[string]*podMetricGroup)
	externalMetrics = make(algorithm.Metrics)

	for metricName := range collector.MetricTargets {
		source, exists := targetMetricSources[metricName]
		if !exists {
			klog.Warningf("metric source missing for metric %s in target %s", metricName, collector.Target.TargetRef.Name)
			continue
		}
		switch {
		case source.Pod != nil:
			addPodMetricToGroups(podGroups, metricName, source.Pod)
		case source.Prometheus != nil:
			collector.resolvePrometheusMetric(ctx, metricName, source.Prometheus, externalMetrics)
		default:
			klog.Warningf("metric source backend config missing for metric %s", metricName)
		}
	}
	return
}

// addPodMetricToGroups registers a pod-sourced metric under the scrape group that
// shares its pod endpoint configuration.
func addPodMetricToGroups(podGroups map[string]*podMetricGroup, metricName string, podSource *v1alpha1.PodMetricSource) {
	if podSource == nil {
		podSource = &v1alpha1.PodMetricSource{}
	}
	scrapeName := podSource.Name
	if scrapeName == "" {
		scrapeName = metricName
	}
	key := podMetricGroupKey(podSource)
	g, ok := podGroups[key]
	if !ok {
		g = &podMetricGroup{podSource: podSource}
		podGroups[key] = g
	}
	g.specs = append(g.specs, podMetricSpec{policyKey: metricName, scrapeName: scrapeName})
}

// resolvePrometheusMetric queries a prometheus-sourced metric and records its
// value in externalMetrics.
func (collector *MetricCollector) resolvePrometheusMetric(
	ctx context.Context,
	metricName string,
	src *v1alpha1.PrometheusMetricSource,
	externalMetrics algorithm.Metrics,
) {
	if src == nil {
		klog.Warningf("prometheus source is nil for metric %s", metricName)
		return
	}
	metricValue, promErr := collector.fetchPrometheusMetric(ctx, src)
	if promErr != nil {
		klog.Warningf("collect prometheus metric %s failed: %v", metricName, promErr)
		return
	}
	externalMetrics[metricName] = metricValue
}

// podMetricSpec ties a policy metric key to the metric name exposed by the pod.
type podMetricSpec struct {
	policyKey  string
	scrapeName string
}

// podMetricGroup is the set of metrics sharing one pod scrape configuration.
type podMetricGroup struct {
	podSource *v1alpha1.PodMetricSource
	specs     []podMetricSpec
}

// podMetricGroupKey returns a stable key identifying a pod scrape configuration.
func podMetricGroupKey(s *v1alpha1.PodMetricSource) string {
	return fmt.Sprintf("%s|%d|%s", s.Uri, s.Port, metav1.FormatLabelSelector(s.LabelSelector))
}

// collectPodMetricsGroup scrapes each ready pod in the target group exactly
// once and extracts every metric required by specs from the same payload.
func (collector *MetricCollector) collectPodMetricsGroup(
	ctx context.Context,
	podLister listerv1.PodLister,
	podSource *v1alpha1.PodMetricSource,
	specs []podMetricSpec,
	pastHistograms []map[string]HistogramInfo,
	currentHistograms map[string]HistogramInfo,
) (values map[string]algorithm.Metrics, unreadyPods sets.Set[string], err error) {
	pods, err := util.GetMetricPods(podLister, collector.Scope.Namespace, collector.Target, podSource)
	if err != nil {
		return nil, nil, err
	}
	if len(pods) == 0 {
		return nil, nil, fmt.Errorf("pod list is empty")
	}

	toScrape, unreadyPods := groupPodsByReadiness(pods)

	// Multiple policy keys may target the same scrape metric name.
	wanted := groupPolicyKeysByScrapeName(specs)

	values = make(map[string]algorithm.Metrics, len(toScrape))
	for _, pod := range toScrape {
		podValues := make(algorithm.Metrics, len(specs))
		if scrapeErr := collector.collectPodMetrics(ctx, pod, podSource, wanted, podValues, pastHistograms, currentHistograms); scrapeErr != nil {
			// Keep the ready pod as an entry with no values so the algorithm counts
			// its metrics as missing, instead of dropping it or treating it as zero load.
			klog.Warningf("collect metrics from pod %s failed: %v", metricPodKey(pod), scrapeErr)
			podValues = make(algorithm.Metrics)
		}
		values[metricPodKey(pod)] = podValues
	}
	return
}

// groupPodsByReadiness splits pods into the ones to scrape (running and ready)
// and the unready ones. Failed or terminating pods are skipped and counted
// nowhere, as in the Kubernetes HPA. A pod whose container restarted and is
// ready again is scraped like any other ready pod.
func groupPodsByReadiness(pods []*corev1.Pod) (toScrape []*corev1.Pod, unreadyPods sets.Set[string]) {
	unreadyPods = sets.New[string]()
	toScrape = make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if util.IsPodFailed(pod) {
			klog.V(4).Infof("skip pod %s for metrics: failed or terminating", metricPodKey(pod))
			continue
		}
		if !inferControllerUtils.IsPodRunningAndReady(pod) {
			unreadyPods.Insert(metricPodKey(pod))
			continue
		}
		toScrape = append(toScrape, pod)
	}
	return
}

// metricPodKey identifies a pod across metric groups. Metric collectors are
// namespace-scoped, but include the namespace so the key remains unambiguous.
func metricPodKey(pod *corev1.Pod) string {
	return pod.Namespace + "/" + pod.Name
}

// groupPolicyKeysByScrapeName maps each scrape metric name to the policy keys
// that consume it.
func groupPolicyKeysByScrapeName(specs []podMetricSpec) map[string][]string {
	wanted := make(map[string][]string, len(specs))
	for _, s := range specs {
		wanted[s.scrapeName] = append(wanted[s.scrapeName], s.policyKey)
	}
	return wanted
}

// collectPodMetrics scrapes a single pod and accumulates its metric values into
// values, refreshing the pod's histogram snapshots in currentHistograms.
func (collector *MetricCollector) collectPodMetrics(
	ctx context.Context,
	pod *corev1.Pod,
	podSource *v1alpha1.PodMetricSource,
	wanted map[string][]string,
	values map[string]float64,
	pastHistograms []map[string]HistogramInfo,
	currentHistograms map[string]HistogramInfo,
) error {
	pastHistogramChains := pastHistogramChainsForPod(pod, pastHistograms)

	currentHistogramMap := currentHistograms[pod.Name].HistogramMap
	if currentHistogramMap == nil {
		currentHistogramMap = make(map[string]*histogram.Snapshot)
	}

	body, err := collector.scrapePod(ctx, pod, podSource)
	if err != nil {
		return err
	}

	families, err := parsePrometheusFamilies(body, wanted)
	if err != nil {
		return err
	}

	for scrapeName, policyKeys := range wanted {
		mf, found := families[scrapeName]
		if !found {
			continue
		}
		for _, policyKey := range policyKeys {
			v, snapshot, gotValue, extractErr := extractMetricFromFamily(mf, pastHistogramChains[policyKey])
			if extractErr != nil {
				return extractErr
			}
			if gotValue {
				values[policyKey] += v
			}
			if snapshot != nil {
				currentHistogramMap[policyKey] = snapshot
			}
		}
	}

	currentHistograms[pod.Name] = HistogramInfo{
		PodStartTime: pod.Status.StartTime,
		HistogramMap: currentHistogramMap,
	}
	return nil
}

// samePod reports whether past was recorded for this same pod. A pod that was
// deleted and created again with the same name has a different start time.
func samePod(pod *corev1.Pod, past HistogramInfo) bool {
	return pod.Status.StartTime != nil && past.PodStartTime != nil &&
		pod.Status.StartTime.Equal(past.PodStartTime)
}

// pastHistogramChainsForPod returns this pod's earlier histograms for each
// metric, oldest first. The oldest one is the baseline that the current
// histogram is compared with. A snapshot is skipped when this pod is not in
// it (for example, because the pod's scrape failed) or when it was taken from
// an earlier pod with the same name, which had a different start time. If a
// snapshot is missing only one metric, it is skipped for that metric only.
func pastHistogramChainsForPod(pod *corev1.Pod, pastHistograms []map[string]HistogramInfo) map[string][]*histogram.Snapshot {
	chains := map[string][]*histogram.Snapshot{}
	for _, histograms := range pastHistograms {
		info, ok := histograms[pod.Name]
		if !ok || !samePod(pod, info) {
			continue
		}
		for policyKey, snapshot := range info.HistogramMap {
			chains[policyKey] = append(chains[policyKey], snapshot)
		}
	}
	return chains
}

func (collector *MetricCollector) scrapePod(ctx context.Context, pod *corev1.Pod, podSource *v1alpha1.PodMetricSource) (string, error) {
	url := collector.buildPodMetricURL(pod, podSource)
	podCtx, cancel := context.WithTimeout(ctx, util.AutoscaleCtxTimeoutSeconds*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(podCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if !util.IsRequestSuccess(resp.StatusCode) || resp.Body == nil {
		return "", fmt.Errorf("invalid metric response for pod %s", pod.Name)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
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
	hostPort := net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port)))
	return fmt.Sprintf("http://%s%s", hostPort, uri)
}

// parsePrometheusFamilies decodes a Prometheus text exposition payload once and
// returns the MetricFamily entries whose names are present in wanted.
func parsePrometheusFamilies(body string, wanted map[string][]string) (map[string]*io_prometheus_client.MetricFamily, error) {
	families := make(map[string]*io_prometheus_client.MetricFamily, len(wanted))
	reader := strings.NewReader(body)
	decoder := expfmt.NewDecoder(reader, expfmt.NewFormat(expfmt.TypeTextPlain))
	for {
		mf := &io_prometheus_client.MetricFamily{}
		err := decoder.Decode(mf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if _, ok := wanted[mf.GetName()]; !ok {
			continue
		}
		families[mf.GetName()] = mf
	}
	return families, nil
}

// extractMetricFromFamily returns the scalar value (and histogram snapshot, when
// applicable) for mf. Counter and gauge families may contain multiple labeled
// samples, so their values are summed. pastHistograms are this pod's earlier
// snapshots of the metric, oldest first. The first one is the baseline.
func extractMetricFromFamily(mf *io_prometheus_client.MetricFamily, pastHistograms []*histogram.Snapshot) (value float64, histSnapshot *histogram.Snapshot, found bool, err error) {
	if len(mf.Metric) < 1 {
		return 0, nil, false, nil
	}

	switch mf.GetType() {
	case io_prometheus_client.MetricType_COUNTER:
		validSamples := 0
		for _, metric := range mf.Metric {
			counter := metric.GetCounter()
			if counter == nil {
				continue
			}
			value += counter.GetValue()
			validSamples++
		}
		return value, nil, validSamples > 0, nil
	case io_prometheus_client.MetricType_GAUGE:
		validSamples := 0
		for _, metric := range mf.Metric {
			gauge := metric.GetGauge()
			if gauge == nil {
				continue
			}
			value += gauge.GetValue()
			validSamples++
		}
		return value, nil, validSamples > 0, nil
	case io_prometheus_client.MetricType_HISTOGRAM:
		// Histogram samples are cumulative bucket sets. Unlike counters/gauges,
		// labeled histogram series cannot be safely summed after decoding here.
		metric := mf.Metric[0]
		hist := metric.GetHistogram()
		if hist == nil {
			return 0, nil, false, nil
		}
		snapshot := histogram.NewSnapshotOfHistogram(hist)
		past := histogram.NewDefaultSnapshot()
		// If the engine restarted since the baseline, its histogram started again
		// from zero and the baseline cannot be subtracted from it. Compare against
		// an empty baseline instead of returning an error or a wrong quantile.
		if len(pastHistograms) > 0 {
			if resetInChain(pastHistograms, snapshot) {
				klog.V(4).Infof("histogram %s was reset since the baseline, comparing against an empty baseline", mf.GetName())
			} else {
				past = pastHistograms[0]
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

// resetInChain reports whether the histogram was reset anywhere between the
// first snapshot in past and current. It checks every pair of neighbouring
// snapshots, so it finds a reset even if the counts have since grown back
// above the first snapshot.
func resetInChain(past []*histogram.Snapshot, current *histogram.Snapshot) bool {
	// Copy past so that appending current cannot change the caller's slice.
	chain := append(slices.Clone(past), current)
	for i := 1; i < len(chain); i++ {
		if chain[i].ResetSince(chain[i-1]) {
			return true
		}
	}
	return false
}

func (collector *MetricCollector) fetchPrometheusMetric(ctx context.Context, src *v1alpha1.PrometheusMetricSource) (float64, error) {
	if src == nil {
		return 0, fmt.Errorf("prometheus metric source is nil")
	}
	if !strings.HasPrefix(src.ServerURL, "http://") && !strings.HasPrefix(src.ServerURL, "https://") {
		return 0, fmt.Errorf("unsupported prometheus serverURL scheme (must be http/https): %q", src.ServerURL)
	}

	timeout := util.AutoscaleCtxTimeoutSeconds * time.Second
	api, err := collector.getPrometheusAPI(src.ServerURL, timeout)
	if err != nil {
		return 0, err
	}

	// Bound the query so a stuck Prometheus cannot hang the reconcile, even if
	// the caller did not attach a deadline.
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, warnings, err := api.Query(queryCtx, src.Query, time.Now())
	if err != nil {
		return 0, err
	}
	for _, warning := range warnings {
		klog.Warningf("prometheus query warning for %s: %s", collector.Target.TargetRef.Name, warning)
	}

	switch v := result.(type) {
	case *model.Scalar:
		val := float64(v.Value)
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, fmt.Errorf("prometheus returned non-finite value %v for target %s", val, collector.Target.TargetRef.Name)
		}
		return val, nil
	case model.Vector:
		if len(v) != 1 {
			return 0, fmt.Errorf("prometheus query must return a single sample vector, got %d", len(v))
		}
		val := float64(v[0].Value)
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return 0, fmt.Errorf("prometheus returned non-finite value %v for target %s", val, collector.Target.TargetRef.Name)
		}
		return val, nil
	default:
		return 0, fmt.Errorf("unsupported prometheus query result type %T", result)
	}
}

func (collector *MetricCollector) getPrometheusAPI(serverURL string, timeout time.Duration) (prometheusv1.API, error) {
	collector.promClientsMu.Lock()
	defer collector.promClientsMu.Unlock()
	if collector.promClients == nil {
		collector.promClients = make(map[string]promapi.Client)
	}

	if client, ok := collector.promClients[serverURL]; ok {
		return prometheusv1.NewAPI(client), nil
	}

	transport := &http.Transport{
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: timeout,
	}
	// NOTE: PrometheusMetricSource.Auth (TLS / bearer token) is intentionally
	// not honored yet. The field semantics are documented on the API types and
	// will be implemented in a follow-up change.
	client, err := promapi.NewClient(promapi.Config{
		Address:      serverURL,
		RoundTripper: transport,
	})
	if err != nil {
		return nil, err
	}

	collector.promClients[serverURL] = client
	return prometheusv1.NewAPI(client), nil
}
