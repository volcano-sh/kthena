#!/usr/bin/env python3
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
import os
import re
import subprocess
import sys
import tempfile
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

    def delete(self, manifest_path: str) -> None:
        subprocess.run(
            ["kubectl", "delete", "-f", manifest_path, "--ignore-not-found"],
            check=True,
            capture_output=True,
            text=True,
        )

    def wait_for_ready(self, label: str, timeout: int = 120) -> None:
        subprocess.run(
            [
                "kubectl", "wait", "--for=condition=ready", "pod",
                f"-l={label}", f"-n", self.namespace,
                f"--timeout={timeout}s",
            ],
            check=True,
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

        # Give the router a moment to sync with the new config
        time.sleep(5)

    def get_router_endpoint(self) -> str:
        """Return a reachable <ip>:<port> for the router Service.

        Primary: node's InternalIP + the Service's NodePort.
        Fallback: starts a kubectl port-forward to the Service ClusterIP.
        """
        # ── Primary: NodePort ──
        node_ip = subprocess.run(
            [
                "kubectl", "get", "nodes",
                "-o", r"jsonpath={.items[0].status.addresses[?(@.type=='InternalIP')].address}",
            ],
            check=True, capture_output=True, text=True,
        ).stdout.strip()

        node_port = subprocess.run(
            [
                "kubectl", "get", "svc", self.ROUTER_DEPLOYMENT,
                "-n", self.ROUTER_NAMESPACE,
                "-o", "jsonpath={.spec.ports[0].nodePort}",
            ],
            check=True, capture_output=True, text=True,
        ).stdout.strip()

        if node_ip and node_port:
            return f"{node_ip}:{node_port}"

        # ── Fallback: kubectl port-forward ──
        local = f"localhost:{self.local_port}"
        print(f"  NodePort unavailable — starting port-forward ({local} → "
              f"svc/{self.ROUTER_DEPLOYMENT}:{self.ROUTER_SVC_PORT})")

        self._pf_proc = subprocess.Popen(
            [
                "kubectl", "port-forward",
                f"svc/{self.ROUTER_DEPLOYMENT}",
                f"{self.local_port}:{self.ROUTER_SVC_PORT}",
                "-n", self.ROUTER_NAMESPACE,
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

        # Wait for port-forward to be ready (or die early).
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            ret = self._pf_proc.poll()
            if ret is not None:
                raise RuntimeError(
                    f"kubectl port-forward exited early with code {ret}"
                )
            time.sleep(0.5)

        print(f"  Port-forward ready: {local}")
        return local

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
        seed = int(hashlib.md5(scenario.name.encode()).hexdigest()[:8], 16) % (2**31)

        cmd = [
            "aiperf", "profile",
            "--model", "mocker-model",
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

        self.mocker_manifest = mocker_manifest or str(
            Path(__file__).parent.parent / "k8s" / "mocker-deployment.yaml"
        )

    def setup_mock_backends(self) -> None:
        self.k8s.apply(self.mocker_manifest)
        self.k8s.wait_for_ready("app=mocker-llm", timeout=180)

    def run_single_config(self, config_path: str, config_name: str) -> BenchmarkResult:
        self.k8s.apply_router_config(config_path)

        router_endpoint = self.k8s.get_router_endpoint()

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

        for metric in ["ttft_avg_ms", "latency_avg_ms", "throughput_rps"]:
            val_a = a.metrics.get(metric)
            val_b = b.metrics.get(metric)

            if val_a is not None and val_b is not None and val_a != 0:
                delta_pct = ((val_b - val_a) / val_a) * 100
                comparison[metric] = {
                    "config_a": val_a,
                    "config_b": val_b,
                    "delta_pct": round(delta_pct, 2),
                    "regression": delta_pct > 10 if "throughput" in metric else delta_pct > 5,
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

        print("\nComparison (B vs A):")
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
    parser.add_argument("--skip-setup", action="store_true", help="Skip mocker + ModelServer + ModelRoute setup")
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

    if not args.skip_setup:
        orchestrator.setup_mock_backends()

    report = orchestrator.run()

    sys.exit(0 if not any(
        v.get("regression", False) for v in report["comparison"].values()
    ) else 1)


if __name__ == "__main__":
    main()
