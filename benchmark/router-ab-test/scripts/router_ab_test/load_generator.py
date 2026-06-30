from __future__ import annotations

import hashlib
import json
import re
import subprocess
from datetime import datetime
from pathlib import Path
from typing import Any

from router_ab_test.models import BenchmarkResult, ScenarioConfig

_AIPERF_METRIC_MAP = {
    "time_to_first_token": "ttft_avg_ms",
    "request_latency": "latency_avg_ms",
    "request_throughput": "throughput_rps",
}


class AIPerfRunner:
    """Run AIPerf benchmarks against the router endpoint."""

    def __init__(self, output_dir: str | Path):
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

    @staticmethod
    def _parse_duration_seconds(duration: str) -> int:
        """Parse a duration string like '60s', '5m', '1h' to seconds."""
        match = re.fullmatch(r"(\d+)\s*(s|m|h)", duration.strip().lower())
        if not match:
            raise ValueError(f"Invalid duration format: {duration!r}")
        value = int(match.group(1))
        unit = match.group(2)
        if unit == "m":
            return value * 60
        if unit == "h":
            return value * 3600
        return value

    def build_aiperf_cmd(self, config_name: str, scenario: ScenarioConfig, router_endpoint: str) -> list[str]:
        """Build the aiperf profile command from a ScenarioConfig."""
        load = scenario.load
        schedule = load.get("schedule", {})
        traffic = load.get("traffic", {})
        concurrency = load.get("concurrency", {})

        seed = int(hashlib.md5(scenario.name.encode()).hexdigest()[:8], 16) % (2**31)
        benchmark_secs = self._parse_duration_seconds(load.get("duration", "60s"))

        cmd = [
            "aiperf",
            "profile",
            "--model",
            "Qwen/Qwen3-0.6B",
            "--endpoint-type",
            "chat",
            "--url",
            f"http://{router_endpoint}",
            "--streaming",
            "--use-server-token-count",
            "--random-seed",
            str(seed),
            "--benchmark-duration",
            str(benchmark_secs),
            "--output-artifact-dir",
            str(self.output_dir / config_name),
            "--concurrency",
            str(concurrency.get("connections", 10)),
        ]

        self._append_schedule_args(cmd, schedule)
        self._append_traffic_args(cmd, traffic, benchmark_secs)
        self._append_token_args(cmd, load.get("prompts", []), load.get("max_tokens", []))
        return cmd

    def run(
        self,
        config_name: str,
        scenario: ScenarioConfig,
        router_endpoint: str,
        extra_args: list[str] | None = None,
    ) -> BenchmarkResult:
        cmd = self.build_aiperf_cmd(config_name, scenario, router_endpoint)
        if extra_args:
            cmd.extend(extra_args)

        run_dir = self.output_dir / config_name
        print(f"  Running: {' '.join(cmd)}")
        subprocess.run(cmd, check=True)

        return BenchmarkResult(
            config_name=config_name,
            scenario=scenario.name,
            timestamp=datetime.now().isoformat(),
            metrics=self._read_metrics_from_output(run_dir),
            raw_output=f"metrics read from {run_dir}",
        )

    def _append_schedule_args(self, cmd: list[str], schedule: dict[str, Any]) -> None:
        if schedule.get("mode") in {"rate", "constant_rate"}:
            cmd.extend(["--request-rate", str(schedule.get("rate", 10))])

    def _append_traffic_args(self, cmd: list[str], traffic: dict[str, Any], benchmark_secs: int) -> None:
        burstiness = float(traffic.get("burstiness", 1.0))
        if burstiness == 1.0:
            cmd.extend(["--arrival-pattern", "poisson"])
        else:
            cmd.extend(["--arrival-pattern", "gamma", "--arrival-smoothness", str(burstiness)])

        ramp = traffic.get("ramp", {})
        if ramp.get("strategy", "none") != "none":
            cmd.extend(["--request-rate-ramp-duration", str(benchmark_secs)])

    def _append_token_args(
        self,
        cmd: list[str],
        prompts: list[dict[str, Any]],
        max_tokens: list[dict[str, Any]],
    ) -> None:
        prompt_tokens = self._join_token_values(prompts, default=512)
        if prompt_tokens:
            cmd.extend(["--synthetic-input-tokens-mean", prompt_tokens])

        output_tokens = self._join_token_values(max_tokens, default=128)
        if output_tokens:
            cmd.extend(["--output-tokens-mean", output_tokens])

    @staticmethod
    def _join_token_values(items: list[dict[str, Any]], default: int) -> str:
        if not items:
            return ""
        return ",".join(str(item.get("tokens", default)) for item in items)

    def _read_metrics_from_output(self, run_dir: Path) -> dict[str, Any]:
        """Parse aiperf's aggregated JSON output (profile_export_aiperf.json)."""
        summary_path = run_dir / "profile_export_aiperf.json"
        if not summary_path.exists():
            print(f"  WARNING: {summary_path} not found; metrics will be empty")
            return {}

        with summary_path.open(encoding="utf-8") as file:
            data = json.load(file)

        metrics: dict[str, Any] = {}
        for aiperf_key, internal_key in _AIPERF_METRIC_MAP.items():
            entry = data.get(aiperf_key)
            if isinstance(entry, dict) and "avg" in entry:
                metrics[internal_key] = entry["avg"]
        return metrics
