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


class MetricsCollector:
    """Collect router Prometheus metrics and pprof artifacts for a benchmark run."""

    def __init__(self, output_dir: str | Path):
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

    def collect_artifacts(
        self,
        config_name: str,
        scenario: ScenarioConfig,
        router_metrics_endpoint: str,
        router_debug_endpoint: str,
    ) -> dict[str, Any]:
        metrics_config = getattr(scenario, "metrics", {}) or {}
        if not metrics_config:
            return {}

        config_dir = self.output_dir / config_name
        config_dir.mkdir(parents=True, exist_ok=True)

        artifacts: dict[str, Any] = {}
        if metrics_config.get("prometheus", False):
            artifacts["prometheus"] = self._collect_prometheus(config_dir, router_metrics_endpoint)
        if metrics_config.get("pprof", False):
            artifacts["pprof"] = self._collect_pprof(config_dir, router_debug_endpoint, metrics_config)
        return artifacts

    def build_router_debug_patch(self, debug_port: int = 15000) -> dict[str, Any]:
        return {
            "spec": {
                "template": {
                    "spec": {
                        "containers": [
                            {
                                "name": "kthena-router",
                                "ports": [{"containerPort": debug_port, "name": "debug"}],
                            }
                        ]
                    }
                }
            }
        }

    def _collect_prometheus(self, config_dir: Path, router_metrics_endpoint: str) -> dict[str, Any]:
        url = f"http://{router_metrics_endpoint}/metrics"
        body = self._fetch_text(url)
        output_path = config_dir / "router_metrics.prom"
        output_path.write_text(body, encoding="utf-8")

        return {
            "endpoint": url,
            "path": str(output_path),
            "sample_count": len([line for line in body.splitlines() if line and not line.startswith("#")]),
            "key_metrics": self._extract_key_metrics(body),
        }

    def _collect_pprof(
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
        cpu_path.write_bytes(self._fetch_bytes(cpu_url))
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
    def _fetch_bytes(url: str) -> bytes:
        with urllib.request.urlopen(url, timeout=60) as response:  # noqa: S310
            return response.read()
