# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
from __future__ import annotations

import argparse
import gzip
import json
import math
import re
from collections import defaultdict
from datetime import datetime
from pathlib import Path
from typing import Any

from google.protobuf.message import DecodeError

from router_ab_test.models import BenchmarkResult, VERDICT_VALID
from router_ab_test.profile_pb2 import Profile

_METRIC_SPECS: dict[str, dict[str, Any]] = {
    "ttft_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
    "latency_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
    "throughput_rps": {"higher_is_better": True, "regression_threshold": 10},
}

# Router-side (Prometheus) comparison specs. Keys that are per-plugin are
# built dynamically in ``compare_router``; these cover the fixed ones.
_ROUTER_METRIC_SPECS: dict[str, dict[str, Any]] = {
    "request_duration_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
    "prefix_cache_match_ratio_avg": {"higher_is_better": True, "regression_threshold": 5},
    "kvcache_aware_match_ratio_avg": {"higher_is_better": True, "regression_threshold": 5},
}
# Success rate is compared in absolute percentage points, not relative %.
_SUCCESS_RATE_REGRESSION_PP = 1.0
_PLUGIN_DURATION_REGRESSION_THRESHOLD = 10

_DEFAULT_PPROF_FOCUS = r"kthena-router/scheduler"
_DEFAULT_PPROF_LIMIT = 10


_PROM_LINE_RE = re.compile(
    r"^(?P<metric>[a-zA-Z_:][a-zA-Z0-9_:]*)"
    r"(?:\{(?P<labels>[^}]*)\})?"
    r"\s+(?P<value>[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?)$"
)
_PROM_LABEL_RE = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')

# kthena-router metric families (pkg/kthena-router/metrics/metrics.go).
_M_REQUESTS_TOTAL = "kthena_router_requests_total"
_M_REQUEST_DURATION = "kthena_router_request_duration_seconds"
_M_TOKENS_TOTAL = "kthena_router_tokens_total"
_M_PLUGIN_DURATION = "kthena_router_scheduler_plugin_duration_seconds"
_M_RATE_LIMIT = "kthena_router_rate_limit_exceeded_total"
_M_PREFIX_CACHE_MATCH = "kthena_router_prefix_cache_match_ratio"
_M_PREFIX_CACHE_EVICTIONS = "kthena_router_prefix_cache_evictions_total"
_M_PREFIX_CACHE_ENTRIES = "kthena_router_prefix_cache_entries"
_M_KVCACHE_MATCH = "kthena_router_kvcache_aware_match_ratio"
_M_KVCACHE_REDIS = "kthena_router_kvcache_aware_redis_duration_seconds"
_M_KVCACHE_TOKENIZE = "kthena_router_kvcache_aware_tokenize_duration_seconds"
_M_KVCACHE_ERRORS = "kthena_router_kvcache_aware_errors_total"
_M_ACTIVE_REQUESTS = "kthena_router_active_requests"
_M_GOROUTINES = "go_goroutines"
_M_RSS_BYTES = "process_resident_memory_bytes"
_M_CPU_SECONDS = "process_cpu_seconds_total"


def _parse_samples(text: str) -> list[tuple[str, dict[str, str], float]]:
    """Parse Prometheus text exposition into (metric, labels, value) samples."""
    samples: list[tuple[str, dict[str, str], float]] = []
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        match = _PROM_LINE_RE.match(line)
        if not match:
            continue
        labels = {
            key: value.replace('\\"', '"').replace("\\\\", "\\")
            for key, value in _PROM_LABEL_RE.findall(match.group("labels") or "")
        }
        samples.append((match.group("metric"), labels, float(match.group("value"))))
    return samples


def _histogram_quantile(buckets: list[tuple[float, float]], quantile: float) -> float | None:
    """Estimate a quantile from (le, cumulative_count) buckets, Prometheus-style.

    Buckets must be sorted by ``le`` ascending and include the ``+Inf`` bucket
    as the last entry (its cumulative count is the total observation count).
    Returns None when the histogram has no observations.
    """
    if not buckets:
        return None
    total = buckets[-1][1]
    if total <= 0:
        return None
    rank = quantile * total
    prev_le, prev_cum = 0.0, 0.0
    for le, cum in buckets:
        if cum >= rank:
            if math.isinf(le):
                # Observations above the last finite bucket: return the
                # lower bound, mirroring histogram_quantile().
                return prev_le
            if cum == prev_cum:
                return le
            return prev_le + (le - prev_le) * (rank - prev_cum) / (cum - prev_cum)
        prev_le, prev_cum = le, cum
    return None


def _histogram_stats(
    samples: list[tuple[str, dict[str, str], float]],
    metric: str,
    group_label: str | None = None,
) -> dict[str, dict[str, Any]]:
    """Aggregate a histogram metric into per-group stats.

    ``metric`` is the family name without ``_bucket``/``_sum``/``_count``.
    Series are grouped by ``group_label`` (all other labels are folded away by
    summing counts, sums, and buckets). Returns ``{group: {count, total,
    avg, p50, p90, p95, p99}}`` with values in the metric's native unit.
    """
    groups: dict[str, dict[str, Any]] = {}
    for name, labels, value in samples:
        if name == f"{metric}_sum":
            kind = "sum"
        elif name == f"{metric}_count":
            kind = "count"
        elif name == f"{metric}_bucket":
            kind = "bucket"
        else:
            continue
        key = labels.get(group_label, "<unknown>") if group_label else "all"
        group = groups.setdefault(key, {"sum": 0.0, "count": 0.0, "buckets": {}})
        if kind == "bucket":
            le = float(labels.get("le", "+Inf"))
            group["buckets"][le] = group["buckets"].get(le, 0.0) + value
        else:
            group[kind] += value

    stats: dict[str, dict[str, Any]] = {}
    for key, group in groups.items():
        count = group["count"]
        buckets = sorted(group["buckets"].items())
        if not buckets or not math.isinf(buckets[-1][0]):
            # Guarantee a +Inf bucket so quantile estimation sees the total.
            buckets.append((math.inf, count))
        stats[key] = {
            "count": int(count),
            "total": group["sum"],
            "avg": (group["sum"] / count) if count > 0 else None,
            "p50": _histogram_quantile(buckets, 0.50),
            "p90": _histogram_quantile(buckets, 0.90),
            "p95": _histogram_quantile(buckets, 0.95),
            "p99": _histogram_quantile(buckets, 0.99),
        }
    return stats


def _counter_by_label(
    samples: list[tuple[str, dict[str, str], float]],
    metric: str,
    label: str,
) -> dict[str, float]:
    """Sum a counter metric grouped by one label."""
    totals: dict[str, float] = {}
    for name, labels, value in samples:
        if name == metric:
            key = labels.get(label, "<unknown>")
            totals[key] = totals.get(key, 0.0) + value
    return totals


def _gauge_value(samples: list[tuple[str, dict[str, str], float]], metric: str) -> float | None:
    """Sum a gauge metric across all label sets (point-in-time snapshot)."""
    values = [value for name, _, value in samples if name == metric]
    return sum(values) if values else None


def analyze_router_metrics(prom_text: str) -> dict[str, Any]:
    """Analyze a kthena-router /metrics snapshot into a structured summary.

    The prom snapshot is taken right after a benchmark run while the router
    counters hold only that run's traffic (the router is rollout-restarted
    when the scheduler config is applied), so cumulative counters map 1:1 to
    the measurement window.

    Sections are included only when the corresponding metric family is
    present, so runs without e.g. kvcache-aware plugins simply omit them.
    """
    samples = _parse_samples(prom_text)
    analysis: dict[str, Any] = {}

    # --- Request outcomes -------------------------------------------------
    by_status = _counter_by_label(samples, _M_REQUESTS_TOTAL, "status_code")
    by_error = _counter_by_label(samples, _M_REQUESTS_TOTAL, "error_type")
    if by_status:
        total = sum(by_status.values())
        successful = sum(v for code, v in by_status.items() if code.startswith("2"))
        analysis["requests"] = {
            "total": int(total),
            "by_status_code": {code: int(v) for code, v in sorted(by_status.items())},
            "by_error_type": {err: int(v) for err, v in sorted(by_error.items())},
            "success_rate_pct": round(successful / total * 100, 2) if total > 0 else None,
        }

    duration_stats = _histogram_stats(samples, _M_REQUEST_DURATION, group_label="status_code")
    if duration_stats:
        analysis["request_duration_seconds"] = {
            code: _duration_entry(stats) for code, stats in sorted(duration_stats.items())
        }

    # --- Tokens ------------------------------------------------------------
    by_token_type = _counter_by_label(samples, _M_TOKENS_TOTAL, "token_type")
    if by_token_type:
        output_tokens = by_token_type.get("output", 0.0)
        successful = sum(v for code, v in by_status.items() if code.startswith("2"))
        analysis["tokens"] = {
            "input": int(by_token_type.get("input", 0.0)),
            "output": int(output_tokens),
            "output_per_successful_request": round(output_tokens / successful, 2) if successful > 0 else None,
        }

    # --- Scheduler plugins -------------------------------------------------
    plugin_samples = [
        (name, labels, value)
        for name, labels, value in samples
        if name.startswith(_M_PLUGIN_DURATION)
    ]
    if plugin_samples:
        plugins: dict[str, dict[str, Any]] = {}
        keys = sorted(
            {
                (labels.get("plugin", "<unknown>"), labels.get("type", "<unknown>"))
                for _, labels, _ in plugin_samples
            }
        )
        for plugin, ptype in keys:
            subset = [
                (name, labels, value)
                for name, labels, value in plugin_samples
                if labels.get("plugin") == plugin and labels.get("type") == ptype
            ]
            stats = _histogram_stats(subset, _M_PLUGIN_DURATION)["all"]
            plugins[f"{plugin}/{ptype}"] = _duration_entry(stats)
        analysis["scheduler_plugins"] = plugins

    # --- Prefix cache plugin ----------------------------------------------
    prefix_match = _histogram_stats(samples, _M_PREFIX_CACHE_MATCH)
    prefix_entries = _gauge_value(samples, _M_PREFIX_CACHE_ENTRIES)
    prefix_evictions = _gauge_value(samples, _M_PREFIX_CACHE_EVICTIONS)
    if prefix_match or prefix_entries is not None or prefix_evictions is not None:
        section: dict[str, Any] = {}
        if prefix_match:
            section["match_ratio"] = _ratio_entry(prefix_match["all"])
        if prefix_entries is not None:
            section["entries"] = int(prefix_entries)
        if prefix_evictions is not None:
            section["evictions_total"] = int(prefix_evictions)
        analysis["prefix_cache"] = section

    # --- kvcache-aware plugin ----------------------------------------------
    kv_match = _histogram_stats(samples, _M_KVCACHE_MATCH)
    kv_redis = _histogram_stats(samples, _M_KVCACHE_REDIS)
    kv_tokenize = _histogram_stats(samples, _M_KVCACHE_TOKENIZE)
    kv_errors = _counter_by_label(samples, _M_KVCACHE_ERRORS, "stage")
    if kv_match or kv_redis or kv_tokenize or kv_errors:
        section = {}
        if kv_match:
            section["match_ratio"] = _ratio_entry(kv_match["all"])
        if kv_redis:
            section["redis_duration_ms"] = _duration_entry(kv_redis["all"], scale=1000)
        if kv_tokenize:
            section["tokenize_duration_ms"] = _duration_entry(kv_tokenize["all"], scale=1000)
        if kv_errors:
            section["errors_total"] = {stage: int(v) for stage, v in sorted(kv_errors.items())}
        analysis["kvcache_aware"] = section

    # --- Rate limiting ------------------------------------------------------
    by_limit_type = _counter_by_label(samples, _M_RATE_LIMIT, "limit_type")
    if by_limit_type:
        analysis["rate_limit"] = {
            "exceeded_total": int(sum(by_limit_type.values())),
            "by_limit_type": {t: int(v) for t, v in sorted(by_limit_type.items())},
        }

    # --- Router runtime snapshot -------------------------------------------
    runtime: dict[str, Any] = {}
    for key, metric in (
        ("go_goroutines", _M_GOROUTINES),
        ("process_resident_memory_bytes", _M_RSS_BYTES),
        ("process_cpu_seconds_total", _M_CPU_SECONDS),
        ("active_requests", _M_ACTIVE_REQUESTS),
    ):
        value = _gauge_value(samples, metric)
        if value is not None:
            runtime[key] = value
    if runtime:
        analysis["runtime"] = runtime

    return analysis


def _duration_entry(stats: dict[str, Any], scale: float = 1000) -> dict[str, Any]:
    """Format histogram stats as a millisecond duration entry."""

    def conv(value: float | None) -> float | None:
        return round(value * scale, 3) if value is not None else None

    return {
        "count": stats["count"],
        "avg_ms": conv(stats["avg"]),
        "p50_ms": conv(stats["p50"]),
        "p90_ms": conv(stats["p90"]),
        "p95_ms": conv(stats["p95"]),
        "p99_ms": conv(stats["p99"]),
    }


def _ratio_entry(stats: dict[str, Any]) -> dict[str, Any]:
    """Format histogram stats as a [0,1] ratio entry."""

    def conv(value: float | None) -> float | None:
        return round(value, 4) if value is not None else None

    return {
        "count": stats["count"],
        "avg": conv(stats["avg"]),
        "p50": conv(stats["p50"]),
        "p90": conv(stats["p90"]),
    }


def format_router_analysis(analysis: dict[str, Any], indent: str = "  ") -> list[str]:
    """Render analyze_router_metrics() output as human-readable lines."""
    lines: list[str] = []

    requests = analysis.get("requests")
    if requests:
        lines.append(
            f"{indent}requests: total={requests['total']} "
            f"success_rate={requests['success_rate_pct']}% "
            f"by_status={requests['by_status_code']}"
        )
        non_success = {err: n for err, n in requests["by_error_type"].items() if err != "successful_request"}
        if non_success:
            lines.append(f"{indent}  errors: {non_success}")

    durations = analysis.get("request_duration_seconds", {})
    for code, stats in durations.items():
        lines.append(
            f"{indent}request_duration[{code}]: n={stats['count']} "
            f"avg={stats['avg_ms']}ms p50={stats['p50_ms']}ms "
            f"p90={stats['p90_ms']}ms p99={stats['p99_ms']}ms"
        )

    tokens = analysis.get("tokens")
    if tokens:
        lines.append(
            f"{indent}tokens: input={tokens['input']} output={tokens['output']} "
            f"output/successful_req={tokens['output_per_successful_request']}"
        )

    plugins = analysis.get("scheduler_plugins", {})
    for plugin_key, stats in plugins.items():
        lines.append(
            f"{indent}plugin[{plugin_key}]: n={stats['count']} "
            f"avg={stats['avg_ms']}ms p95={stats['p95_ms']}ms p99={stats['p99_ms']}ms"
        )

    prefix = analysis.get("prefix_cache")
    if prefix:
        parts = []
        if "match_ratio" in prefix:
            ratio = prefix["match_ratio"]
            parts.append(f"match_ratio avg={ratio['avg']} p50={ratio['p50']} n={ratio['count']}")
        if "entries" in prefix:
            parts.append(f"entries={prefix['entries']}")
        if "evictions_total" in prefix:
            parts.append(f"evictions={prefix['evictions_total']}")
        lines.append(f"{indent}prefix_cache: {' '.join(parts)}")

    kvcache = analysis.get("kvcache_aware")
    if kvcache:
        if "match_ratio" in kvcache:
            ratio = kvcache["match_ratio"]
            lines.append(f"{indent}kvcache_aware: match_ratio avg={ratio['avg']} p50={ratio['p50']} n={ratio['count']}")
        for stage_key, label in (("tokenize_duration_ms", "tokenize"), ("redis_duration_ms", "redis")):
            if stage_key in kvcache:
                stats = kvcache[stage_key]
                lines.append(
                    f"{indent}  {label}: n={stats['count']} avg={stats['avg_ms']}ms p95={stats['p95_ms']}ms"
                )
        if "errors_total" in kvcache:
            lines.append(f"{indent}  errors: {kvcache['errors_total']}")

    rate_limit = analysis.get("rate_limit")
    if rate_limit:
        lines.append(f"{indent}rate_limit: exceeded={rate_limit['exceeded_total']} {rate_limit['by_limit_type']}")

    runtime = analysis.get("runtime")
    if runtime:
        rss_mb = runtime.get("process_resident_memory_bytes")
        rss_str = f" rss={rss_mb / 1024 / 1024:.0f}MiB" if rss_mb is not None else ""
        lines.append(
            f"{indent}runtime: goroutines={runtime.get('go_goroutines')}"
            f" cpu_seconds={runtime.get('process_cpu_seconds_total')}"
            f"{rss_str} active_requests={runtime.get('active_requests')}"
        )

    return lines


def analyze_pprof_profile(
    path: str | Path,
    sample_type: str | None = None,
    focus: str | None = _DEFAULT_PPROF_FOCUS,
    limit: int = _DEFAULT_PPROF_LIMIT,
) -> dict[str, Any]:
    """Return the hottest scheduler/plugin functions from a pprof profile."""
    if limit < 1:
        raise ValueError("limit must be greater than zero")
    profile = Profile()
    with gzip.open(path, "rb") as profile_file:
        profile.ParseFromString(profile_file.read())
    if not profile.sample_type:
        raise ValueError("profile has no sample types")

    profile_strings = profile.string_table
    if sample_type is None:
        sample_type = (
            profile_strings[profile.default_sample_type]
            if profile.default_sample_type
            else profile_strings[profile.sample_type[-1].type]
        )

    sample_index = None
    unit = ""
    available_types = []
    for index, value_type in enumerate(profile.sample_type):
        type_name = profile_strings[value_type.type]
        available_types.append(type_name)
        if type_name == sample_type:
            sample_index = index
            unit = profile_strings[value_type.unit]
    if sample_index is None:
        available = ", ".join(available_types)
        raise ValueError(f"unknown sample type {sample_type!r}; available: {available}")

    locations_by_id = {location.id: location for location in profile.location}
    functions_by_id = {function.id: function for function in profile.function}
    flat_values: dict[str, int] = defaultdict(int)
    cumulative_values: dict[str, int] = defaultdict(int)
    total = 0
    for sample in profile.sample:
        value = sample.value[sample_index]
        total += value
        stack_functions = []
        leaf_function = None
        for location_index, location_id in enumerate(sample.location_id):
            location = locations_by_id.get(location_id)
            if location is None:
                continue
            for line_index, line in enumerate(location.line):
                function = functions_by_id.get(line.function_id)
                if function is None:
                    continue
                function_name = profile_strings[function.name]
                stack_functions.append(function_name)
                if location_index == 0 and line_index == 0:
                    leaf_function = function_name

        if leaf_function:
            flat_values[leaf_function] += value
        for function_name in set(stack_functions):
            cumulative_values[function_name] += value

    pattern = re.compile(focus) if focus else None
    function_names = flat_values.keys() | cumulative_values.keys()
    if pattern:
        function_names = {name for name in function_names if pattern.search(name)}
    top_functions = sorted(
        ((name, flat_values[name], cumulative_values[name]) for name in function_names),
        key=lambda item: (-item[1], -item[2], item[0]),
    )[:limit]
    return {
        "sample_type": sample_type,
        "unit": unit,
        "total": total,
        "top_functions": [
            {
                "name": name,
                "flat": flat,
                "flat_pct": round(flat / total * 100, 2) if total else 0,
                "cumulative": cumulative,
                "cumulative_pct": round(cumulative / total * 100, 2) if total else 0,
            }
            for name, flat, cumulative in top_functions
        ],
    }


class ResultReporter:
    """Build, persist, and print A/B benchmark reports."""

    def compare(self, result_a: BenchmarkResult, result_b: BenchmarkResult) -> dict[str, Any]:
        # Only valid runs participate in A/B comparison. An invalid or
        # framework_error run produces no comparable numbers (issue #1271).
        for result in (result_a, result_b):
            status = result.verdict.get("status") if result.verdict else None
            if status and status != VERDICT_VALID:
                return {
                    "_skipped": True,
                    "reason": f"{result.config_name} verdict is {status!r}; comparison requires both runs to be valid",
                }

        comparison: dict[str, Any] = {}
        for metric, spec in _METRIC_SPECS.items():
            val_a = result_a.metrics.get(metric)
            val_b = result_b.metrics.get(metric)
            if val_a is None or val_b is None or val_a == 0:
                continue

            delta_pct = self._calculate_delta_pct(val_a, val_b, spec["higher_is_better"])
            comparison[metric] = {
                "config_a": val_a,
                "config_b": val_b,
                "delta_pct": round(delta_pct, 2),
                "regression": delta_pct < -spec["regression_threshold"],
            }
        return comparison

    def compare_router(
        self,
        analysis_a: dict[str, Any],
        analysis_b: dict[str, Any],
    ) -> dict[str, Any]:
        """Compare router-side (Prometheus) analysis between two runs.

        Same delta sign convention as ``compare``: positive delta means B is
        better than A. Success rate is compared in absolute percentage points.
        """
        comparison: dict[str, Any] = {}

        rate_a = (analysis_a.get("requests") or {}).get("success_rate_pct")
        rate_b = (analysis_b.get("requests") or {}).get("success_rate_pct")
        if rate_a is not None and rate_b is not None:
            delta_pp = round(rate_b - rate_a, 2)
            comparison["request_success_rate_pct"] = {
                "config_a": rate_a,
                "config_b": rate_b,
                "delta_pp": delta_pp,
                "regression": delta_pp < -_SUCCESS_RATE_REGRESSION_PP,
            }

        for metric_key, spec in _ROUTER_METRIC_SPECS.items():
            val_a = self._router_metric_value(analysis_a, metric_key)
            val_b = self._router_metric_value(analysis_b, metric_key)
            if val_a is None or val_b is None or val_a == 0:
                continue
            delta_pct = self._calculate_delta_pct(val_a, val_b, spec["higher_is_better"])
            comparison[metric_key] = {
                "config_a": val_a,
                "config_b": val_b,
                "delta_pct": round(delta_pct, 2),
                "regression": delta_pct < -spec["regression_threshold"],
            }

        # Per-plugin scheduling latency: only plugins present in BOTH runs
        # are comparable (different configs enable different plugin sets).
        plugins_a = analysis_a.get("scheduler_plugins", {})
        plugins_b = analysis_b.get("scheduler_plugins", {})
        for plugin_key in sorted(set(plugins_a) & set(plugins_b)):
            avg_a = plugins_a[plugin_key].get("avg_ms")
            avg_b = plugins_b[plugin_key].get("avg_ms")
            if not avg_a or avg_b is None:
                continue
            delta_pct = self._calculate_delta_pct(avg_a, avg_b, higher_is_better=False)
            comparison[f"plugin_avg_ms[{plugin_key}]"] = {
                "config_a": avg_a,
                "config_b": avg_b,
                "delta_pct": round(delta_pct, 2),
                "regression": delta_pct < -_PLUGIN_DURATION_REGRESSION_THRESHOLD,
            }
        return comparison

    @staticmethod
    def _router_metric_value(analysis: dict[str, Any], metric_key: str) -> float | None:
        if metric_key == "request_duration_avg_ms":
            # Successful requests only — error paths have unrelated latency.
            for code, stats in analysis.get("request_duration_seconds", {}).items():
                if code.startswith("2"):
                    return stats.get("avg_ms")
            return None
        if metric_key == "prefix_cache_match_ratio_avg":
            return (analysis.get("prefix_cache", {}).get("match_ratio") or {}).get("avg")
        if metric_key == "kvcache_aware_match_ratio_avg":
            return (analysis.get("kvcache_aware", {}).get("match_ratio") or {}).get("avg")
        return None

    def build_report(
        self,
        scenario_name: str,
        description: str,
        config_a_path: str,
        config_b_path: str,
        result_a: BenchmarkResult,
        result_b: BenchmarkResult,
    ) -> dict[str, Any]:
        comparison = self.compare(result_a, result_b)
        analysis_a = self._router_analysis_for(result_a)
        analysis_b = self._router_analysis_for(result_b)
        pprof_analysis_a = self._pprof_analysis_for(result_a)
        pprof_analysis_b = self._pprof_analysis_for(result_b)
        # Router counters from an invalid/framework_error run are no more
        # comparable than the AIPerf numbers — gate on the same verdict rule.
        router_comparison: dict[str, Any] = {}
        if not comparison.get("_skipped") and analysis_a and analysis_b:
            router_comparison = self.compare_router(analysis_a, analysis_b)
        return {
            "scenario": scenario_name,
            "description": description,
            "timestamp": datetime.now().isoformat(),
            "config_a": {
                "path": config_a_path,
                "metrics": result_a.metrics,
                "artifacts": result_a.artifacts,
                "verdict": result_a.verdict,
                "router_analysis": analysis_a,
                "pprof_analysis": pprof_analysis_a,
            },
            "config_b": {
                "path": config_b_path,
                "metrics": result_b.metrics,
                "artifacts": result_b.artifacts,
                "verdict": result_b.verdict,
                "router_analysis": analysis_b,
                "pprof_analysis": pprof_analysis_b,
            },
            "comparison": comparison,
            "router_comparison": router_comparison,
        }

    @staticmethod
    def _router_analysis_for(result: BenchmarkResult) -> dict[str, Any] | None:
        """Analyze the run's router_metrics.prom artifact, if one was collected."""
        prometheus = result.artifacts.get("prometheus") or {}
        prom_path = prometheus.get("path")
        if not prom_path:
            return None
        path = Path(prom_path)
        if not path.is_file():
            return None
        return analyze_router_metrics(path.read_text(encoding="utf-8"))

    @staticmethod
    def _pprof_analysis_for(result: BenchmarkResult) -> dict[str, Any] | None:
        """Analyze every pprof artifact collected for one benchmark run."""
        pprof = result.artifacts.get("pprof") or {}
        if pprof.get("error"):
            return {"error": pprof["error"]}
        profiles = pprof.get("profiles") or {}
        if not profiles:
            return None
        analysis: dict[str, Any] = {}
        for profile_name, profile_path in sorted(profiles.items()):
            path = Path(profile_path)
            if not path.is_file():
                analysis[profile_name] = {"error": f"profile not found: {path}"}
                continue
            try:
                analysis[profile_name] = analyze_pprof_profile(path)
            except (OSError, ValueError, re.error, DecodeError) as error:
                analysis[profile_name] = {"error": str(error)}
        return analysis

    def write_report(self, output_path: str | Path, report: dict[str, Any]) -> None:
        output_path = Path(output_path)
        with output_path.open("w", encoding="utf-8") as file:
            json.dump(report, file, indent=2)

    def print_report(self, report: dict[str, Any]) -> None:
        print("\n" + "=" * 70)
        print(f"A/B Test Report: {report['scenario']}")
        print(f"Description: {report['description']}")
        print("=" * 70)

        self._print_config_section("A", report["config_a"])
        self._print_config_section("B", report["config_b"])

        comparison = report.get("comparison", {})
        if comparison.get("_skipped"):
            print(f"\nComparison skipped: {comparison.get('reason', '<no reason>')}")
        else:
            print("\nEnd-to-end Metrics Comparison (B vs A, positive delta means improvement):")
            for metric, data in comparison.items():
                status = "REGRESSION" if data["regression"] else "OK"
                print(f"  {metric}: {data['delta_pct']:+.2f}% [{status}]")

        router_comparison = report.get("router_comparison") or {}
        if router_comparison:
            print("\nRouter Metrics Comparison (B vs A):")
            for metric, data in router_comparison.items():
                status = "REGRESSION" if data["regression"] else "OK"
                if "delta_pp" in data:
                    print(f"  {metric}: {data['delta_pp']:+.2f}pp [{status}]")
                else:
                    print(f"  {metric}: {data['delta_pct']:+.2f}% [{status}]")

        print("=" * 70)

    @staticmethod
    def _print_config_section(label: str, config_report: dict[str, Any]) -> None:
        print(f"\nRouter Config {label}: {config_report['path']}")
        verdict = config_report.get("verdict") or {}
        if verdict:
            print(f"  verdict: {verdict.get('status', '<unset>')}")
            for reason in verdict.get("reasons", []):
                print(f"    - {reason}")
        for key, value in config_report["metrics"].items():
            print(f"  {key}: {value}")

        router_analysis = config_report.get("router_analysis")
        if router_analysis:
            print("  router metrics (from router_metrics.prom):")
            for line in format_router_analysis(router_analysis, indent="    "):
                print(line)

        pprof_analysis = config_report.get("pprof_analysis") or {}
        for profile_name, profile in pprof_analysis.items():
            if profile_name == "error":
                print(f"  pprof: {profile}")
                continue
            if "error" in profile:
                print(f"  pprof[{profile_name}]: {profile['error']}")
                continue
            print(
                f"  pprof[{profile_name}]: sample_type={profile['sample_type']} "
                f"unit={profile['unit']} total={profile['total']}"
            )
            for function in profile["top_functions"]:
                print(
                    f"    {function['flat']:>12} {function['flat_pct']:>6.2f}% "
                    f"{function['cumulative']:>12} {function['cumulative_pct']:>6.2f}%  "
                    f"{function['name']}"
                )

        if config_report["artifacts"]:
            print("  artifacts:")
            for key in config_report["artifacts"]:
                print(f"    - {key}")

    @staticmethod
    def _calculate_delta_pct(config_a_value: float, config_b_value: float, higher_is_better: bool) -> float:
        if higher_is_better:
            return ((config_b_value - config_a_value) / config_a_value) * 100
        return ((config_a_value - config_b_value) / config_a_value) * 100


def main(argv: list[str] | None = None) -> int:
    """Offline analysis of collected router_metrics.prom files.

    Usage: python -m router_ab_test.reporter <router_metrics.prom> [...]
    """
    parser = argparse.ArgumentParser(description="Analyze kthena router Prometheus metrics snapshots.")
    parser.add_argument("prom_files", nargs="+", help="Path(s) to router_metrics.prom files")
    parser.add_argument("--json", action="store_true", help="Emit raw JSON instead of formatted text")
    args = parser.parse_args(argv)

    analyses: dict[str, dict[str, Any]] = {}
    for prom_file in args.prom_files:
        path = Path(prom_file)
        analysis = analyze_router_metrics(path.read_text(encoding="utf-8"))
        analyses[str(path)] = analysis
        if args.json:
            continue
        print("=" * 70)
        print(f"Router metrics analysis: {path}")
        print("=" * 70)
        for line in format_router_analysis(analysis, indent="  "):
            print(line)

    if args.json:
        print(json.dumps(analyses, indent=2))

    if len(analyses) == 2 and not args.json:
        (path_a, analysis_a), (path_b, analysis_b) = analyses.items()
        comparison = ResultReporter().compare_router(analysis_a, analysis_b)
        if comparison:
            print("\n" + "=" * 70)
            print(f"Comparison ({path_b} vs {path_a}):")
            for metric, data in comparison.items():
                status = "REGRESSION" if data["regression"] else "OK"
                if "delta_pp" in data:
                    print(f"  {metric}: {data['delta_pp']:+.2f}pp [{status}]")
                else:
                    print(f"  {metric}: {data['delta_pct']:+.2f}% [{status}]")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
