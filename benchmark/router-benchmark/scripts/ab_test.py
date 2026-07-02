# !/usr/bin/env python3

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

"""
A/B Test Orchestrator for Kthena Router Benchmarks.

Compares two router scheduler configurations by:
  1. Applying router ConfigMap A, restarting the router, running benchmark
  2. Applying router ConfigMap B, restarting the router, running benchmark

Usage:
    python ab_test.py \
        --scenario scenarios/smoke-test-s2.yaml \
        --router-config-a k8s/router-config-random.yaml \
        --router-config-b k8s/router-config-least-latency.yaml \
        --output results/
"""

from __future__ import annotations

import argparse

from router_ab_test import (
    ABTestOrchestrator,
    AIPerfRunner,
    BenchmarkResult,
    EndpointMode,
    K8sManager,
    MetricsCollector,
    ResultReporter,
    ScenarioConfig,
)

__all__ = [
    "ABTestOrchestrator",
    "AIPerfRunner",
    "BenchmarkResult",
    "EndpointMode",
    "K8sManager",
    "MetricsCollector",
    "ResultReporter",
    "ScenarioConfig",
    "build_parser",
    "main",
]


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Kthena Router A/B Test Orchestrator")
    parser.add_argument("--scenario", required=True, help="Path to scenario YAML")
    parser.add_argument("--router-config-a", required=True, help="Path to router scheduler ConfigMap for config A")
    parser.add_argument("--router-config-b", required=True, help="Path to router scheduler ConfigMap for config B")
    parser.add_argument("--output", default="./results", help="Output directory")
    parser.add_argument("--mocker-manifest", help="Path to mocker deployment YAML")
    parser.add_argument(
        "--local-port",
        type=int,
        default=K8sManager.DEFAULT_LOCAL_PORT,
        help=f"Local port for kubectl port-forward (default: {K8sManager.DEFAULT_LOCAL_PORT})",
    )
    parser.add_argument(
        "--endpoint-mode",
        choices=[EndpointMode.PORT_FORWARD, EndpointMode.LB],
        default=EndpointMode.PORT_FORWARD,
        help="Router endpoint access mode: 'port-forward' for Kind clusters (default), "
             "'lb' for clusters with LoadBalancer support",
    )
    return parser


def main() -> None:
    args = build_parser().parse_args()
    orchestrator = ABTestOrchestrator(
        scenario_path=args.scenario,
        router_config_a_path=args.router_config_a,
        router_config_b_path=args.router_config_b,
        output_dir=args.output,
        local_port=args.local_port,
        mocker_manifest=args.mocker_manifest,
        endpoint_mode=args.endpoint_mode,
    )
    report = orchestrator.run()
    has_regression = any(metric.get("regression", False) for metric in report["comparison"].values())
    raise SystemExit(1 if has_regression else 0)


if __name__ == "__main__":
    main()
