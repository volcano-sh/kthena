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

package utils

import (
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// DrainMinTimeout is the minimal accepted drain-timeout annotation value.
	DrainMinTimeout = 60 * time.Second
	// DrainMaxKVBuffer is the hard upper bound of the KV buffer.
	DrainMaxKVBuffer = 30 * time.Second
)

// TODO: replace the fixed KV buffer with a precise signal (e.g. a connector
// metric for active KV pulls) when engines expose one.

// DrainHookScript is the in-Pod drain logic: poll engine metrics and exit as
// soon as a poll reads zero running/waiting (a failed scrape keeps polling),
// optionally holding the KV buffer. Its budget is
// min(DRAIN_TIMEOUT, 70% * GRACE_SECONDS), within the grace period.
const DrainHookScript = `
import os, re, sys, time, urllib.request

poll_interval = 5
try:
    drain_timeout = int(os.environ.get("DRAIN_TIMEOUT", "0"))
    grace_seconds = int(os.environ.get("GRACE_SECONDS", "0"))
    buffer_s = int(os.environ.get("DRAIN_BUFFER_SECONDS", "0"))
except ValueError:
    sys.exit(0)
port = os.environ.get("DRAIN_METRICS_PORT", "")
budget = min(drain_timeout, int(grace_seconds * 0.7))
if budget <= 0:
    sys.exit(0)
# Engine metrics carry labels, e.g. vllm:num_requests_running{engine="0"} 0.0;
# the label block is optional so both forms match.
pattern = re.compile(
    r"^(?:vllm|sglang):(?:num_requests_running|num_running_reqs|"
    r"num_requests_waiting|num_queue_reqs)(?:\{[^}]*\})?\s+(\S+)", re.M)
deadline = time.time() + budget
no_metrics_reported = False
while time.time() < deadline:
    counts = None
    try:
        body = urllib.request.urlopen(
            "http://localhost:%s/metrics" % port, timeout=2).read().decode()
        counts = [float(v) for v in pattern.findall(body)]
    except Exception as err:
        print("drain: metrics scrape failed: %s" % err, flush=True)
    if counts is not None and not counts and not no_metrics_reported:
        # Unrecognized metric names must not end the drain: keep polling and
        # leave the residual to SIGTERM + grace.
        no_metrics_reported = True
        print("drain: no drain metrics matched; check the engine metric names", flush=True)
    if counts and all(c == 0 for c in counts):
        if buffer_s > 0:
            print("drain: in-flight drained, holding %ss KV buffer" % buffer_s, flush=True)
            time.sleep(buffer_s)
        else:
            print("drain: in-flight drained", flush=True)
        sys.exit(0)
    time.sleep(poll_interval)
print("drain: timeout before in-flight reached zero", flush=True)
`

// DrainConfig is the parsed drain-timeout annotation of a ModelServing.
type DrainConfig struct {
	Enabled        bool
	TimeoutSeconds int
	Invalid        bool
	Reason         string
}

// ParseDrainConfig parses the drain-timeout annotation. Missing or "0" keeps
// the drain disabled; invalid or too-small values are reported as Invalid.
func ParseDrainConfig(ms *workloadv1alpha1.ModelServing) DrainConfig {
	raw, ok := ms.Annotations[workloadv1alpha1.DrainTimeoutAnnotationKey]
	if !ok {
		return DrainConfig{}
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return DrainConfig{Invalid: true, Reason: fmt.Sprintf("invalid %s value %q: %v", workloadv1alpha1.DrainTimeoutAnnotationKey, raw, err)}
	}
	if d <= 0 {
		return DrainConfig{}
	}
	if d < DrainMinTimeout {
		return DrainConfig{Invalid: true, Reason: fmt.Sprintf("%s value %q is below the minimal %s", workloadv1alpha1.DrainTimeoutAnnotationKey, raw, DrainMinTimeout)}
	}
	return DrainConfig{Enabled: true, TimeoutSeconds: int(d.Seconds())}
}

// ParseRoleBuffer parses the optional drain-kv-buffer annotation of a role
// template, clamped to DrainMaxKVBuffer.
func ParseRoleBuffer(role workloadv1alpha1.Role) int {
	if role.EntryTemplate.Metadata == nil {
		return 0
	}
	raw, ok := role.EntryTemplate.Metadata.Annotations[workloadv1alpha1.DrainKVBufferAnnotationKey]
	if !ok {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	if d > DrainMaxKVBuffer {
		d = DrainMaxKVBuffer
	}
	return int(d.Seconds())
}

// metricsPort picks the drain metrics port: the first port named "metrics",
// otherwise the first declared containerPort.
func metricsPort(pod *corev1.Pod) int {
	fallback := 0
	for i := range pod.Spec.Containers {
		for _, p := range pod.Spec.Containers[i].Ports {
			if p.ContainerPort == 0 {
				continue
			}
			if fallback == 0 {
				fallback = int(p.ContainerPort)
			}
			if p.Name == "metrics" {
				return int(p.ContainerPort)
			}
		}
	}
	return fallback
}

// injectDrainHook adds the drain preStop hook and env vars to every container;
// containers that already declare a preStop hook are left untouched.
func injectDrainHook(pod *corev1.Pod, timeoutSeconds, bufferSeconds int) {
	port := metricsPort(pod)
	if port == 0 {
		return
	}
	grace := 30
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		grace = int(*pod.Spec.TerminationGracePeriodSeconds)
	}
	AddPodEnvVars(pod, []corev1.EnvVar{
		{Name: "DRAIN_TIMEOUT", Value: strconv.Itoa(timeoutSeconds)},
		{Name: "GRACE_SECONDS", Value: strconv.Itoa(grace)},
		{Name: "DRAIN_BUFFER_SECONDS", Value: strconv.Itoa(bufferSeconds)},
		{Name: "DRAIN_METRICS_PORT", Value: strconv.Itoa(port)},
	}...)
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Lifecycle == nil {
			c.Lifecycle = &corev1.Lifecycle{}
		}
		if c.Lifecycle.PreStop != nil {
			continue
		}
		c.Lifecycle.PreStop = &corev1.LifecycleHandler{
			Exec: &corev1.ExecAction{Command: []string{"python3", "-c", DrainHookScript}},
		}
	}
}

// ApplyDrain injects the drain preStop hook when the drain-timeout annotation
// enables it; Pods without injectable metrics ports keep the default behavior.
func ApplyDrain(pod *corev1.Pod, role workloadv1alpha1.Role, ms *workloadv1alpha1.ModelServing) {
	cfg := ParseDrainConfig(ms)
	if !cfg.Enabled {
		return
	}
	injectDrainHook(pod, cfg.TimeoutSeconds, ParseRoleBuffer(role))
}
