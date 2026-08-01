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

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


@dataclass
class BackendProfile:
    """A group of mock backend pods sharing the same behaviour.

    YAML key → mocker CLI flag:
      engineType     → --engine-type
      model          → --model-path
      speedupRatio   → --speedup-ratio
      kvCacheBlocks  → --num-gpu-blocks-override
      maxNumSeqs     → --max-num-seqs

    Optional ``resources`` overrides the mocker container's k8s
    requests/limits for this profile — useful for CI-constrained
    scenarios (e.g. many small pods) where the defaults would not
    fit on the node. Shape::

        resources:
          requests: {cpu: "250m", memory: "256Mi"}
          limits:   {cpu: "1",    memory: "1Gi"}
    """
    name: str
    count: int
    engine_type: str
    model: str
    speedup_ratio: float
    kv_cache_blocks: int | None = None
    max_num_seqs: int | None = None
    resources: dict[str, dict[str, str]] | None = None


@dataclass
class BackendsConfig:
    """Typed representation of the ``backends:`` block in a scenario YAML.

    Supports an optional ``common`` key for per-field defaults shared across
    all profiles.  Each profile can override any common field.
    """
    profiles: list[BackendProfile]

    default_engine_type: str = "sglang"
    default_model: str = "Qwen/Qwen3-0.6B"
    default_speedup_ratio: float = 1.0
    default_kv_cache_blocks: int = 16384
    default_max_num_seqs: int = 256

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BackendsConfig":
        common = data.get("common", {})
        defaults = {
            "default_engine_type": common.get("engineType", cls.default_engine_type),
            "default_model": common.get("model", cls.default_model),
            "default_speedup_ratio": float(common.get("speedupRatio", cls.default_speedup_ratio)),
            "default_kv_cache_blocks": common.get("kvCacheBlocks", cls.default_kv_cache_blocks),
            "default_max_num_seqs": common.get("maxNumSeqs", cls.default_max_num_seqs),
        }
        profiles = [
            BackendProfile(
                name=p["name"],
                count=p["count"],
                engine_type=p.get("engineType", defaults["default_engine_type"]),
                model=p.get("model", defaults["default_model"]),
                speedup_ratio=float(p.get("speedupRatio", defaults["default_speedup_ratio"])),
                kv_cache_blocks=p.get("kvCacheBlocks"),
                max_num_seqs=p.get("maxNumSeqs"),
                resources=p.get("resources"),
            )
            for p in data.get("profiles", [])
        ]
        return cls(profiles=profiles, **defaults)


@dataclass
class ScenarioConfig:
    name: str
    description: str
    load: dict[str, Any]
    backends: BackendsConfig
    aiperf: dict[str, Any] = field(default_factory=dict)
    metrics: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self):
        """Coerce a raw dict backends to BackendsConfig for ergonomic construction."""
        if isinstance(self.backends, dict):
            self.backends = BackendsConfig.from_dict(self.backends)

    @classmethod
    def from_yaml(cls, path: str | Path) -> "ScenarioConfig":
        with Path(path).open(encoding="utf-8") as file:
            data = yaml.safe_load(file)
        # ``__post_init__`` coerces the raw ``backends`` dict to BackendsConfig.
        return cls(**data)


@dataclass
class BenchmarkResult:
    config_name: str
    scenario: str
    timestamp: str
    metrics: dict[str, Any]
    raw_output: str
    artifacts: dict[str, Any] = field(default_factory=dict)
    verdict: dict[str, Any] = field(default_factory=dict)


# Run verdict values (see https://github.com/volcano-sh/kthena/issues/1271).
# Only ``VERDICT_VALID`` runs are eligible for A/B comparison.
VERDICT_VALID = "valid"
VERDICT_INVALID = "invalid"
VERDICT_FRAMEWORK_ERROR = "framework_error"

# Pod state reasons that, when observed on a steady-state backend, invalidate
# the run regardless of restart count (e.g. OOMKill followed by a successful
# restart still produced a gap in capacity mid-measurement).
INVALIDATING_TERMINATED_REASONS = frozenset({"OOMKilled", "Error"})
INVALIDATING_WAITING_REASONS = frozenset({"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull"})

# p95/p50 latency ratio above which a run is annotated with a non-fatal
# warning (see apply_request_level_verdict). Evidence from issue #1452's CI
# experiments: every empirically-validated safe run (s2 rate=3, s3
# concurrency=5) stayed at p95/p50 <= ~1.4x across repeated runs; the one s2
# rate=5 run later rejected for run-to-run instability hit ~3.2x on its
# random-plugin arm; the confirmed-saturated s2 rate=20 baseline hit
# 10-25x+. A single ambiguous sample near 3x isn't enough evidence to
# hard-invalidate on — this flags it for human review instead.
TAIL_LATENCY_WARNING_RATIO = 3.0

# Absolute success-rate floor below which a run is invalid regardless of the
# genuine-error/cancelled-503 checks. Added after s1/s4-s8 round-1
# calibration (issue #1452) surfaced a failure mode those checks miss: a run
# where the whole latency distribution shifts up together (p50 ~20s, p95/p50
# ratio a mundane ~1.5x) rather than a fast median with a stretched tail, and
# where every 503 happens to be covered by end-of-window cancellations, so
# neither existing check fires even though the run is clearly unhealthy
# (smoke-test-s6's least-request arm: 81.48% success, p50=19.8s). Every
# validated-safe sample collected for this issue stayed >=95% success; every
# confirmed-bad sample (s2 rate=20's 31-33%, s2 rate=5/60s's 74-77%, s1's
# 5-73%, s6's 81.48%) sat well under 90%. This floor is a coarse backstop for
# catching that kind of obviously-bad single run, not a replacement for the
# repeated-run methodology used to validate a load level.
SUCCESS_RATE_FLOOR_PCT = 90.0


def compute_run_verdict(restart_stats: dict[str, Any]) -> dict[str, Any]:
    """Compute the run verdict from post-traffic mocker pod restart stats.

    A run is ``valid`` only if no mocker pod restarted and no pod shows an
    invalidating terminated/waiting reason. Otherwise the run is ``invalid``
    and the offending pods are listed under ``offenders`` for diagnosis.

    ``framework_error`` is reserved for benchmark tooling failures (e.g.
    AIPerf exit failure) and is set by the caller, not by this function.
    """
    offenders: list[dict[str, Any]] = []
    reasons: list[str] = []
    for pod in restart_stats.get("pods", []):
        pod_reasons: list[str] = []
        if pod.get("restarts", 0) > 0:
            pod_reasons.append(f"restartCount={pod['restarts']}")
        last_reason = pod.get("last_reason")
        if last_reason in INVALIDATING_TERMINATED_REASONS:
            pod_reasons.append(f"lastState.terminated.reason={last_reason}")
        waiting_reason = pod.get("waiting_reason")
        if waiting_reason in INVALIDATING_WAITING_REASONS:
            pod_reasons.append(f"state.waiting.reason={waiting_reason}")
        if pod_reasons:
            offenders.append({"name": pod.get("name"), "reasons": pod_reasons})
            reasons.extend(f"{pod.get('name')}: {r}" for r in pod_reasons)

    if offenders:
        return {
            "status": VERDICT_INVALID,
            "reasons": reasons,
            "offenders": offenders,
            "restart_stats": restart_stats,
        }
    return {
        "status": VERDICT_VALID,
        "reasons": [],
        "offenders": [],
        "restart_stats": restart_stats,
    }


def apply_request_level_verdict(verdict: dict[str, Any], request_stats: dict[str, Any]) -> dict[str, Any]:
    """Layer request-level saturation checks onto an existing pod-based verdict.

    ``compute_run_verdict`` only sees mocker pod restart/crash signals, which
    stay clean even when the backend is saturated at the request level: every
    CI run in issue #1452's investigation showed 0 mocker restarts on both
    safe and saturated loads. This adds two additional, evidence-derived
    checks on top of that verdict:

    - genuine AIPerf-reported request errors (``request_stats["genuine_errors"]``,
      summed from AIPerf's own ``error_summary``) — 0 on every validated-safe
      run this issue produced, and >0 whenever a load was later rejected as
      saturated.
    - router 503s in excess of AIPerf's own end-of-window cancellation count
      (``request_stats["cancelled"]``). A fixed-duration benchmark always
      cancels a few trailing in-flight requests when it ends; every
      validated-safe run's 503 count stayed at or below that cancellation
      count. 503s beyond it indicate requests failed mid-run rather than
      being cut off by the harness — i.e. this deliberately does NOT treat
      every 503 as saturation.
    - success rate below ``SUCCESS_RATE_FLOOR_PCT``. This is a coarse
      backstop for a failure mode the other two checks miss entirely: a run
      where the whole latency distribution shifts up together (not just the
      tail) and every 503 happens to be covered by cancellations, so neither
      check above fires despite the run being clearly unhealthy.

    Each check is skipped (not assumed clean/full-rate) when its input is
    missing, since "unknown" is not the same as "zero".

    A high p95/p50 tail-latency ratio is recorded as a non-fatal warning
    (see ``TAIL_LATENCY_WARNING_RATIO``) rather than an invalidating
    condition — the evidence for a hard cutoff there is a single ambiguous
    sample, not a clean separator like the two checks above.

    ``request_stats`` keys (all optional): genuine_errors, cancelled,
    total_503, p50_ms, p95_ms, success_rate_pct.
    """
    if verdict.get("status") == VERDICT_FRAMEWORK_ERROR:
        # Benchmark tooling itself failed; there is no real traffic to judge.
        return verdict

    reasons = list(verdict.get("reasons", []))
    offenders = list(verdict.get("offenders", []))
    warnings = list(verdict.get("warnings", []))

    genuine_errors = request_stats.get("genuine_errors")
    if genuine_errors:
        reasons.append(
            f"aiperf reported {genuine_errors} genuine request error(s) "
            "(excludes end-of-window cancellations)"
        )
        offenders.append({"name": "aiperf_client", "reasons": [f"genuine_errors={genuine_errors}"]})

    cancelled = request_stats.get("cancelled")
    total_503 = request_stats.get("total_503") or 0
    if cancelled is not None and total_503 > 0:
        unexplained_503s = max(0, total_503 - min(total_503, cancelled))
        if unexplained_503s > 0:
            reasons.append(
                f"{unexplained_503s} of {total_503} router 503(s) are not accounted for by "
                f"AIPerf's {cancelled} end-of-window cancellation(s) — requests failed mid-run"
            )
            offenders.append({"name": "router", "reasons": [f"unexplained_503s={unexplained_503s}"]})

    success_rate_pct = request_stats.get("success_rate_pct")
    if success_rate_pct is not None and success_rate_pct < SUCCESS_RATE_FLOOR_PCT:
        reasons.append(
            f"success rate {success_rate_pct}% is below the {SUCCESS_RATE_FLOOR_PCT}% floor"
        )
        offenders.append({"name": "router", "reasons": [f"success_rate_pct={success_rate_pct}"]})

    p50_ms = request_stats.get("p50_ms")
    p95_ms = request_stats.get("p95_ms")
    if p50_ms and p95_ms and p50_ms > 0:
        ratio = p95_ms / p50_ms
        if ratio > TAIL_LATENCY_WARNING_RATIO:
            warnings.append(
                f"p95/p50 latency ratio {ratio:.1f}x exceeds {TAIL_LATENCY_WARNING_RATIO}x "
                "(possible queueing under load; not treated as invalidating on its own)"
            )

    status = VERDICT_INVALID if offenders else verdict.get("status", VERDICT_VALID)
    return {
        **verdict,
        "status": status,
        "reasons": reasons,
        "offenders": offenders,
        "warnings": warnings,
        "request_stats": request_stats,
    }
