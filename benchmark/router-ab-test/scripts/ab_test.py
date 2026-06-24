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
import json
import os
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, Optional

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

    def __init__(self, namespace: str = "default"):
        self.namespace = namespace

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
        result = subprocess.run(
            [
                "kubectl", "get", "svc", self.ROUTER_DEPLOYMENT,
                "-n", self.ROUTER_NAMESPACE,
                "-o", "jsonpath={.spec.clusterIP}:{.spec.ports[0].port}",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
        return result.stdout.strip()


class AIPerfRunner:
    """Runs AIPerf benchmarks against the router."""

    def __init__(self, output_dir: str):
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

    def run(
        self,
        config_name: str,
        scenario: ScenarioConfig,
        router_endpoint: str,
        extra_args: Optional[list] = None,
    ) -> BenchmarkResult:
        load = scenario.load
        schedule = load.get("schedule", {})
        concurrency = load.get("concurrency", {})
        duration = load.get("duration", "60s")

        cmd = [
            "aiperf", "profile",
            "--model", "mocker-model",
            "--endpoint-type", "chat",
            "--url", f"http://{router_endpoint}",
            "--streaming",
            "--duration", duration,
            "--output-dir", str(self.output_dir / config_name),
        ]

        if schedule.get("mode") == "rate":
            cmd.extend(["--request-rate", str(schedule.get("rate", 10))])

        cmd.extend(["--concurrency", str(concurrency.get("connections", 10))])

        if extra_args:
            cmd.extend(extra_args)

        result = subprocess.run(cmd, capture_output=True, text=True, timeout=300)

        metrics = self._parse_output(result.stdout)

        return BenchmarkResult(
            config_name=config_name,
            scenario=scenario.name,
            timestamp=datetime.now().isoformat(),
            metrics=metrics,
            raw_output=result.stdout,
        )

    def _parse_output(self, output: str) -> Dict[str, Any]:
        metrics = {}
        for line in output.split("\n"):
            if "Time to First Token" in line and "avg" in line:
                parts = line.split("│")
                if len(parts) >= 3:
                    metrics["ttft_avg_ms"] = float(parts[2].strip().replace(",", ""))
            elif "Request Latency" in line and "avg" in line:
                parts = line.split("│")
                if len(parts) >= 3:
                    metrics["latency_avg_ms"] = float(parts[2].strip().replace(",", ""))
            elif "Request Throughput" in line:
                parts = line.split("│")
                if len(parts) >= 3:
                    try:
                        metrics["throughput_rps"] = float(parts[2].strip().replace(",", ""))
                    except ValueError:
                        pass
        return metrics


class ABTestOrchestrator:
    """Orchestrates A/B tests comparing two router scheduler configurations."""

    def __init__(
        self,
        scenario_path: str,
        router_config_a_path: str,
        router_config_b_path: str,
        output_dir: str,
        mocker_manifest: Optional[str] = None,
    ):
        self.scenario = ScenarioConfig.from_yaml(scenario_path)
        self.router_config_a_path = Path(router_config_a_path)
        self.router_config_b_path = Path(router_config_b_path)
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)

        self.k8s = K8sManager()
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
        result_a = self.run_single_config(str(self.router_config_a_path), "config_a")
        result_b = self.run_single_config(str(self.router_config_b_path), "config_b")

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

    args = parser.parse_args()

    orchestrator = ABTestOrchestrator(
        scenario_path=args.scenario,
        router_config_a_path=args.router_config_a,
        router_config_b_path=args.router_config_b,
        output_dir=args.output,
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
