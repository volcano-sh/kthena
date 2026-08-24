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

import re
import threading
import urllib.request
from pathlib import Path
from typing import Any

from router_ab_test.models import ScenarioConfig

_DEFAULT_CPU_PROFILE_SECONDS = 30
_DEFAULT_PPROF_PROFILES = ("heap", "goroutine", "allocs", "block", "mutex")
_KEY_PROMETHEUS_METRICS = (
    "go_goroutines",
    "go_memstats_heap_alloc_bytes",
    "go_memstats_heap_inuse_bytes",
    "go_memstats_next_gc_bytes",
    "go_gc_duration_seconds",
    "process_cpu_seconds_total",
    "process_resident_memory_bytes",
    "process_open_fds",
)
_PROMETHEUS_LINE_RE = re.compile(
    r"^(?P<metric>[a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s+(?P<value>[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?)$"
)


class PprofCollection:
    """Handle for an in-flight async pprof collection (daemon thread).

    `result()` joins with a caller-supplied timeout and ALWAYS returns a
    dict — the artifact payload or {"error": ...}. A hung fetch can never
    block the benchmark: daemon thread + abandon() + bounded join.
    """

    def __init__(self, work: Any) -> None:
        self._cancelled = threading.Event()
        self._result: dict[str, Any] | None = None

        def runner() -> None:
            try:
                self._result = {"error": "cancelled"} if self._cancelled.is_set() else work()
            except Exception as exc:  # noqa: BLE001 — must never kill the run
                self._result = {"error": f"{type(exc).__name__}: {exc}"}

        self._thread = threading.Thread(target=runner, daemon=True)
        self._thread.start()

    def abandon(self) -> None:
        self._cancelled.set()

    def result(self, timeout: float) -> dict[str, Any]:
        self._thread.join(timeout)
        if self._thread.is_alive():
            self._cancelled.set()
            return {"error": f"pprof collection timed out after {timeout}s"}
        return self._result if self._result is not None else {"error": "cancelled"}


class MetricsCollector:
    """Collect router Prometheus metrics and pprof artifacts for a benchmark run."""

    def __init__(self, output_dir: str | Path):
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

    def start_pprof_collection(
        self,
        config_name: str,
        router_debug_endpoint: str,
        metrics_config: dict[str, Any],
    ) -> PprofCollection:
        """Start async pprof collection covering the run; join via the returned handle."""
        config_dir = self.output_dir / config_name
        (config_dir / "pprof").mkdir(parents=True, exist_ok=True)
        return PprofCollection(
            lambda: self._fetch_pprof_profiles(config_dir, router_debug_endpoint, metrics_config)
        )

    def collect_artifacts(
        self,
        config_name: str,
        scenario: ScenarioConfig,
        router_metrics_endpoint: str,
        pprof_handle: PprofCollection | None = None,
    ) -> dict[str, Any]:
        metrics_config = getattr(scenario, "metrics", {}) or {}
        if not metrics_config:
            return {}

        config_dir = self.output_dir / config_name
        config_dir.mkdir(parents=True, exist_ok=True)

        artifacts: dict[str, Any] = {}
        if metrics_config.get("prometheus", False):
            artifacts["prometheus"] = self._collect_prometheus(config_dir, router_metrics_endpoint)
        if pprof_handle is not None:
            join_timeout = int(metrics_config.get("cpuProfileSeconds", _DEFAULT_CPU_PROFILE_SECONDS)) + 60
            artifacts["pprof"] = pprof_handle.result(timeout=join_timeout)
        return artifacts

    def _collect_prometheus(self, config_dir: Path, router_metrics_endpoint: str) -> dict[str, Any]:
        url = f"http://{router_metrics_endpoint}/metrics"
        try:
            body = self._fetch_text(url)
        except urllib.error.HTTPError as exc:
            print(f"  WARNING: metrics endpoint {url} returned HTTP {exc.code}; skipping prometheus collection")
            return {
                "endpoint": url,
                "path": "",
                "sample_count": 0,
                "key_metrics": {},
                "error": f"HTTP {exc.code}",
            }
        except urllib.error.URLError as exc:
            print(f"  WARNING: metrics endpoint {url} unreachable: {exc.reason}; skipping prometheus collection")
            return {
                "endpoint": url,
                "path": "",
                "sample_count": 0,
                "key_metrics": {},
                "error": str(exc.reason),
            }
        output_path = config_dir / "router_metrics.prom"
        output_path.write_text(body, encoding="utf-8")

        return {
            "endpoint": url,
            "path": str(output_path),
            "sample_count": len([line for line in body.splitlines() if line and not line.startswith("#")]),
            "key_metrics": self._extract_key_metrics(body),
        }

    def _fetch_pprof_profiles(
        self,
        config_dir: Path,
        router_debug_endpoint: str,
        metrics_config: dict[str, Any],
    ) -> dict[str, Any]:
        pprof_dir = config_dir / "pprof"
        pprof_dir.mkdir(parents=True, exist_ok=True)

        cpu_profile_seconds = int(metrics_config.get("cpuProfileSeconds", _DEFAULT_CPU_PROFILE_SECONDS))
        profiles = list(metrics_config.get("profiles") or _DEFAULT_PPROF_PROFILES)
        collected_profiles: dict[str, str] = {}

        cpu_url = f"http://{router_debug_endpoint}/debug/pprof/profile?seconds={cpu_profile_seconds}"
        cpu_path = pprof_dir / "cpu.pb.gz"
        cpu_path.write_bytes(self._fetch_bytes(cpu_url, timeout=cpu_profile_seconds + 30))
        collected_profiles["cpu"] = str(cpu_path)

        for profile_name in profiles:
            profile_url = f"http://{router_debug_endpoint}/debug/pprof/{profile_name}"
            profile_path = pprof_dir / f"{profile_name}.pb.gz"
            profile_path.write_bytes(self._fetch_bytes(profile_url))
            collected_profiles[profile_name] = str(profile_path)

        return {
            "endpoint": f"http://{router_debug_endpoint}/debug/pprof",
            "cpu_profile_seconds": cpu_profile_seconds,
            "profiles": collected_profiles,
        }

    def _extract_key_metrics(self, body: str) -> dict[str, float]:
        key_metrics: dict[str, float] = {}
        for line in body.splitlines():
            if not line or line.startswith("#"):
                continue
            match = _PROMETHEUS_LINE_RE.match(line.strip())
            if not match:
                continue
            metric_name = match.group("metric")
            if metric_name not in _KEY_PROMETHEUS_METRICS:
                continue
            key_metrics[metric_name] = float(match.group("value"))
        return key_metrics

    @staticmethod
    def _fetch_text(url: str) -> str:
        with urllib.request.urlopen(url, timeout=30) as response:  # noqa: S310
            return response.read().decode("utf-8")

    @staticmethod
    def _fetch_bytes(url: str, timeout: float = 60) -> bytes:
        with urllib.request.urlopen(url, timeout=timeout) as response:  # noqa: S310
            return response.read()
