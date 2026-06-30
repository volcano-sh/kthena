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

# !/usr/bin/env python3
"""
A/B Test Orchestrator for Kthena Router Benchmarks.

Compares two router scheduler configurations by:
  1. Applying router ConfigMap A, restarting the router, running benchmark
  2. Applying router ConfigMap B, restarting the router, running benchmark

Usage:
    python ab_test.py \
        --scenario scenarios/smoke-test-s2.yaml \
        --router-config-b k8s/router-config-least-latency.yaml \
        --router-config-b k8s/router-config-least-request.yaml \
        --output results/
"""

import argparse
import hashlib
import json
import re
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, List, Optional

import yaml


@dataclass
class ScenarioConfig:
    name: str
    description: str
    load: Dict[str, Any]
    backends: Dict[str, Any]
    routing: Dict[str, Any]
    aiperf: Dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_yaml(cls, path: str) -> "ScenarioConfig":
        with open(path) as f:
            data = yaml.safe_load(f)
        return cls(**data)


@dataclass
class BenchmarkResult:
    config_name: str
    scenario: str
    timestamp: str
    metrics: Dict[str, Any]
    raw_output: str


class K8sManager:
    """Manages Kubernetes resources for the benchmark."""

    ROUTER_NAMESPACE = "kthena-system"
    ROUTER_DEPLOYMENT = "kthena-router"
    ROUTER_SVC_PORT = 80
    DEFAULT_LOCAL_PORT = 8080
    MOCKER_DEPLOYMENT = "mocker-llm"

    def __init__(self, namespace: str = "default", local_port: int = DEFAULT_LOCAL_PORT):
        self.namespace = namespace
        self.local_port = local_port
        self._pf_proc: "Optional[subprocess.Popen]" = None

    def apply(self, manifest_path: str) -> None:
        subprocess.run(
            ["kubectl", "apply", "-f", manifest_path],
            check=True,
            capture_output=True,
            text=True,
        )

        time.sleep(5)

    def delete(self, manifest_path: str) -> None:
        subprocess.run(
            ["kubectl", "delete", "-f", manifest_path, "--ignore-not-found"],
            check=True,
            capture_output=True,
            text=True,
        )

    def wait_for_router_ready(
            self, mocker_model_name: str, endpoint: str, timeout: int = 300
    ) -> None:
        """Probe the router to confirm the route table has the given model loaded."""
        print(f"  Probing router for model={mocker_model_name} at {endpoint}...")
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                result = subprocess.run(
                    ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
                     "-X", "POST", f"http://{endpoint}/v1/chat/completions",
                     "-H", "Content-Type: application/json",
                     "-d", (
                             '{"model":"' + mocker_model_name + '",'
                                                                '"messages":[{"role":"user","content":"ping"}],'
                                                                '"max_tokens":1}'
                     )],
                    capture_output=True, text=True, timeout=5,
                )
                if result.stdout.strip() == "200":
                    print(f"  Router route for '{mocker_model_name}' is ready")
                    return
                # 404 = route not found (still warming up). Other codes = unexpected.
                if result.stdout.strip() != "404":
                    print(f"  Router probe returned {result.stdout.strip()}, retrying...")
            except subprocess.TimeoutExpired:
                pass
            time.sleep(2)
        raise RuntimeError(
            f"Router route for '{mocker_model_name}' not ready within {timeout}s. "
        )

    def apply_router_config(self, config_path: str) -> None:
        """Apply router ConfigMap and restart the router deployment to pick it up."""
        print(f"  Applying router config: {config_path}")
        self.apply(config_path)

        print(f"  Restarting router deployment {self.ROUTER_DEPLOYMENT}...")
        subprocess.run(
            [
                "kubectl", "rollout", "restart",
                f"deployment/{self.ROUTER_DEPLOYMENT}",
                "-n", self.ROUTER_NAMESPACE,
            ],
            check=True,
            capture_output=True,
            text=True,
        )

        print(f"  Waiting for router rollout to complete...")
        subprocess.run(
            [
                "kubectl", "rollout", "status",
                f"deployment/{self.ROUTER_DEPLOYMENT}",
                "-n", self.ROUTER_NAMESPACE,
                f"--timeout=120s",
            ],
            check=True,
            capture_output=True,
            text=True,
        )

    def wait_for_backend_ready(self):
        print(f"  Waiting for mocker-llm to be ready...")
        subprocess.run(
            [
                "kubectl", "rollout", "status",
                f"deployment/{self.MOCKER_DEPLOYMENT}",
                f"--timeout=120s",
            ],
            check=True,
            capture_output=True,
            text=True,
        )

    def get_router_endpoint(self) -> str:
        """Return a reachable <ip>:<port> for the router Service.

        Uses kubectl port-forward directly (NodePort is unreliable in Kind cluster).
        """
        local = f"localhost:{self.local_port}"
        print(f"  Starting port-forward ({local} → "
              f"svc/{self.ROUTER_DEPLOYMENT}:{self.ROUTER_SVC_PORT})")

        self._pf_proc = subprocess.Popen(
            [
                "kubectl", "port-forward",
                f"svc/{self.ROUTER_DEPLOYMENT}",
                f"{self.local_port}:{self.ROUTER_SVC_PORT}",
                "-n", self.ROUTER_NAMESPACE,
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

        # Wait for port-forward to be ready (or die early).
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            ret = self._pf_proc.poll()
            if ret is not None:
                # Process exited — read stderr for error details
                _, stderr = self._pf_proc.communicate(timeout=5)
                error_msg = stderr.strip() if stderr else f"exit code {ret}"
                raise RuntimeError(f"kubectl port-forward failed: {error_msg}")

            # Check if port is actually accepting connections
            try:
                import socket
                with socket.create_connection(("localhost", self.local_port), timeout=1):
                    pass
                print(f"  Port-forward ready: {local}")
                return local
            except (socket.error, OSError):
                # Port not ready yet, keep waiting
                time.sleep(0.5)

        # Timeout — port-forward still running but port not reachable
        if self._pf_proc.poll() is None:
            # Try to get any stderr output
            stderr = ""
            try:
                import select
                if self._pf_proc.stderr:
                    ready, _, _ = select.select([self._pf_proc.stderr], [], [], 0.1)
                    if ready:
                        stderr = self._pf_proc.stderr.read()
            except Exception:
                pass
            raise RuntimeError(
                f"Port-forward timeout after 15s — port {local} not reachable. "
                f"stderr: {stderr.strip() if stderr else '<none>'}"
            )
        else:
            _, stderr = self._pf_proc.communicate(timeout=5)
            raise RuntimeError(
                f"Port-forward exited unexpectedly: {stderr.strip() if stderr else 'unknown error'}"
            )

    def cleanup_port_forward(self) -> None:
        """Stop the port-forward process if one was started."""
        if self._pf_proc is None:
            return
        if self._pf_proc.poll() is None:
            self._pf_proc.terminate()
            try:
                self._pf_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._pf_proc.kill()
                self._pf_proc.wait()
        self._pf_proc = None


class AIPerfRunner:
    """Runs AIPerf benchmarks against the router."""

    def __init__(self, output_dir: str):
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

    def build_aiperf_cmd(
            self,
            config_name: str,
            scenario: "ScenarioConfig",
            router_endpoint: str,
    ) -> List[str]:
        """Build the aiperf profile command from a ScenarioConfig.

        Maps YAML scenario fields to AIPerf CLI flags with full support for
        sweep syntax, arrival patterns, rate ramping, and token length control.
        """
        load = scenario.load
        schedule = load.get("schedule", {})
        traffic = load.get("traffic", {})
        concurrency = load.get("concurrency", {})

        # Deterministic seed from scenario name (MD5 for cross-run stability).
        seed = int(hashlib.md5(scenario.name.encode()).hexdigest()[:8], 16) % (2 ** 31)

        cmd = [
            "aiperf", "profile",
            "--model", "Qwen/Qwen3-0.6B",
            "--endpoint-type", "chat",
            "--url", f"http://{router_endpoint}",
            "--streaming",
            "--use-server-token-count",
            "--random-seed", str(seed),
        ]

        # ── Duration ──
        duration_raw = load.get("duration", "60s")
        benchmark_secs = self._parse_duration_seconds(duration_raw)
        cmd.extend(["--benchmark-duration", str(benchmark_secs)])

        # ── Output directory (AIPerf flag is --output-artifact-dir) ──
        cmd.extend([
            "--output-artifact-dir",
            str(self.output_dir / config_name),
        ])

        # ── Rate control (with sweep support) ──
        if schedule.get("mode") in ("rate", "constant_rate"):
            rate = schedule.get("rate", 10)
            # Support comma-separated sweep (e.g. YAML list → "10,50,100").
            cmd.extend(["--request-rate", str(rate)])

        # ── Concurrency (with sweep support) ──
        cmd.extend(["--concurrency", str(concurrency.get("connections", 10))])

        # ── Arrival pattern (burstiness → arrival-pattern + arrival-smoothness) ──
        burstiness = float(traffic.get("burstiness", 1.0))
        if burstiness == 1.0:
            cmd.extend(["--arrival-pattern", "poisson"])
        else:
            cmd.extend([
                "--arrival-pattern", "gamma",
                "--arrival-smoothness", str(burstiness),
            ])

        # ── Rate ramp-up ──
        ramp = traffic.get("ramp", {})
        if ramp.get("strategy", "none") != "none":
            cmd.extend([
                "--request-rate-ramp-duration",
                str(benchmark_secs),
            ])

        # ── Prompt token lengths (sweep: comma-separated) ──
        prompts = load.get("prompts", [])
        if prompts:
            tokens = ",".join(str(p.get("tokens", 512)) for p in prompts)
            cmd.extend(["--synthetic-input-tokens-mean", tokens])

        # ── Output token lengths (sweep: comma-separated) ──
        max_tokens = load.get("max_tokens", [])
        if max_tokens:
            tokens = ",".join(str(m.get("tokens", 128)) for m in max_tokens)
            cmd.extend(["--output-tokens-mean", tokens])

        return cmd

    def run(
            self,
            config_name: str,
            scenario: "ScenarioConfig",
            router_endpoint: str,
            extra_args: Optional[list] = None,
    ) -> BenchmarkResult:
        cmd = self.build_aiperf_cmd(config_name, scenario, router_endpoint)

        if extra_args:
            cmd.extend(extra_args)

        # Run without capture so output streams to terminal (visible in CI logs).
        run_dir = self.output_dir / config_name
        print(f"  Running: {' '.join(cmd)}")
        subprocess.run(cmd, check=True)

        metrics = self._read_metrics_from_output(run_dir)

        return BenchmarkResult(
            config_name=config_name,
            scenario=scenario.name,
            timestamp=datetime.now().isoformat(),
            metrics=metrics,
            raw_output=f"metrics read from {run_dir}",
        )

    def _read_metrics_from_output(self, run_dir: Path) -> Dict[str, Any]:
        """Parse aiperf's aggregated JSON output (profile_export_aiperf.json)."""
        metrics: Dict[str, Any] = {}

        summary_path = run_dir / "profile_export_aiperf.json"
        if not summary_path.exists():
            print(f"  WARNING: {summary_path} not found; metrics will be empty")
            return metrics

        with open(summary_path) as f:
            data = json.load(f)

        # Map aiperf metric names to our internal keys.
        metric_map = {
            "time_to_first_token": "ttft_avg_ms",
            "request_latency": "latency_avg_ms",
            "request_throughput": "throughput_rps",
        }

        for aiperf_key, our_key in metric_map.items():
            entry = data.get(aiperf_key)
            if isinstance(entry, dict) and "avg" in entry:
                metrics[our_key] = entry["avg"]

        return metrics


class ABTestOrchestrator:
    """Orchestrates A/B tests comparing two router scheduler configurations."""

    def __init__(
            self,
            scenario_path: str,
            router_config_a_path: str,
            router_config_b_path: str,
            output_dir: str,
            local_port: int = K8sManager.DEFAULT_LOCAL_PORT,
            mocker_manifest: Optional[str] = None,
    ):
        self.scenario = ScenarioConfig.from_yaml(scenario_path)
        self.router_config_a_path = Path(router_config_a_path)
        self.router_config_b_path = Path(router_config_b_path)
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

        self.k8s = K8sManager(local_port=local_port)
        self.runner = AIPerfRunner(str(self.output_dir / "runs"))

        self.mocker_manifest = mocker_manifest

    def run_single_config(self, config_path: str, config_name: str) -> BenchmarkResult:
        self.k8s.apply_router_config(config_path)

        self.k8s.wait_for_backend_ready()

        # get forwarded endpoint
        router_endpoint = self.k8s.get_router_endpoint()

        # Wait until router's route table is warm
        self.k8s.wait_for_router_ready("Qwen/Qwen3-0.6B", router_endpoint, timeout=300)

        #  launching AIPerf
        return self.runner.run(
            config_name=config_name,
            scenario=self.scenario,
            router_endpoint=router_endpoint,
            extra_args=self.scenario.aiperf.get("extraArgs"),
        )

    def run(self) -> Dict[str, Any]:
        """Run A/B test."""
        try:
            result_a = self.run_single_config(str(self.router_config_a_path), "config_a")
            result_b = self.run_single_config(str(self.router_config_b_path), "config_b")
        finally:
            self.k8s.cleanup_port_forward()

        comparison = self._compare(result_a, result_b)

        report = {
            "scenario": self.scenario.name,
            "description": self.scenario.description,
            "timestamp": datetime.now().isoformat(),
            "config_a": {
                "path": str(self.router_config_a_path),
                "metrics": result_a.metrics,
            },
            "config_b": {
                "path": str(self.router_config_b_path),
                "metrics": result_b.metrics,
            },
            "comparison": comparison,
        }

        report_path = self.output_dir / f"report_{self.scenario.name}.json"
        with open(report_path, "w") as f:
            json.dump(report, f, indent=2)

        self._print_report(report)

        return report

    def _compare(self, a: BenchmarkResult, b: BenchmarkResult) -> Dict[str, Any]:
        comparison = {}

        metric_specs = {
            "ttft_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
            "latency_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
            "throughput_rps": {"higher_is_better": True, "regression_threshold": 10},
        }

        for metric, spec in metric_specs.items():
            val_a = a.metrics.get(metric)
            val_b = b.metrics.get(metric)

            if val_a is not None and val_b is not None and val_a != 0:
                if spec["higher_is_better"]:
                    delta_pct = ((val_b - val_a) / val_a) * 100
                else:
                    delta_pct = ((val_a - val_b) / val_a) * 100
                comparison[metric] = {
                    "config_a": val_a,
                    "config_b": val_b,
                    "delta_pct": round(delta_pct, 2),
                    "regression": delta_pct < -spec["regression_threshold"],
                }

        return comparison

    def _print_report(self, report: Dict[str, Any]) -> None:
        print("\n" + "=" * 70)
        print(f"A/B Test Report: {report['scenario']}")
        print(f"Description: {report['description']}")
        print("=" * 70)

        print(f"\nRouter Config A: {report['config_a']['path']}")
        for k, v in report["config_a"]["metrics"].items():
            print(f"  {k}: {v}")

        print(f"\nRouter Config B: {report['config_b']['path']}")
        for k, v in report["config_b"]["metrics"].items():
            print(f"  {k}: {v}")

        print("\nComparison (B vs A, positive delta means improvement):")
        for metric, data in report["comparison"].items():
            status = "REGRESSION" if data["regression"] else "OK"
            print(f"  {metric}: {data['delta_pct']:+.2f}% [{status}]")

        print("=" * 70)


def main():
    parser = argparse.ArgumentParser(description="Kthena Router A/B Test Orchestrator")
    parser.add_argument("--scenario", required=True, help="Path to scenario YAML")
    parser.add_argument("--router-config-a", required=True, help="Path to router scheduler ConfigMap for config A")
    parser.add_argument("--router-config-b", required=True, help="Path to router scheduler ConfigMap for config B")
    parser.add_argument("--output", default="./results", help="Output directory")
    parser.add_argument("--mocker-manifest", help="Path to mocker deployment YAML")
    parser.add_argument(
        "--local-port", type=int, default=K8sManager.DEFAULT_LOCAL_PORT,
        help=f"Local port for kubectl port-forward fallback (default: {K8sManager.DEFAULT_LOCAL_PORT})",
    )

    args = parser.parse_args()

    orchestrator = ABTestOrchestrator(
        scenario_path=args.scenario,
        router_config_a_path=args.router_config_a,
        router_config_b_path=args.router_config_b,
        output_dir=args.output,
        local_port=args.local_port,
        mocker_manifest=args.mocker_manifest,
    )

    report = orchestrator.run()

    sys.exit(0 if not any(
        v.get("regression", False) for v in report["comparison"].values()
    ) else 1)


if __name__ == "__main__":
    main()
