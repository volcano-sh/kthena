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

import subprocess
from pathlib import Path
from typing import Any

from router_ab_test.kubernetes import EndpointMode, K8sManager
from router_ab_test.load_generator import AIPerfRunner
from router_ab_test.metrics_collector import MetricsCollector
from router_ab_test.models import (
    VERDICT_FRAMEWORK_ERROR,
    BenchmarkResult,
    ScenarioConfig,
    compute_run_verdict,
)
from router_ab_test.reporter import ResultReporter


class ABTestOrchestrator:
    """Orchestrate A/B tests comparing two router scheduler configurations."""

    def __init__(
        self,
        scenario_path: str,
        router_config_a_path: str,
        router_config_b_path: str,
        output_dir: str,
        local_port: int = K8sManager.DEFAULT_LOCAL_PORT,
        endpoint_mode: str = EndpointMode.PORT_FORWARD,
    ):
        self.scenario = ScenarioConfig.from_yaml(scenario_path)
        self.router_config_a_path = Path(router_config_a_path)
        self.router_config_b_path = Path(router_config_b_path)
        self.output_dir = Path(output_dir)
        self.output_dir.mkdir(parents=True, exist_ok=True)
        self.k8s = K8sManager(local_port=local_port, endpoint_mode=endpoint_mode)
        self.runner = AIPerfRunner(self.output_dir / "runs")
        self.collector = MetricsCollector(self.output_dir / "artifacts")
        self.reporter = ResultReporter()

    def run_single_config(self, config_path: str, config_name: str) -> BenchmarkResult:
        self.k8s.cleanup_port_forward()

        # Tear down and re-deploy backends for a cold-start baseline
        self.k8s.cleanup_backends()
        self.k8s.deploy_backends(self.scenario.backends)

        self.k8s.apply_router_config(config_path)
        router_endpoint = self.k8s.get_router_endpoint()
        router_debug_endpoint = self.k8s.get_router_debug_endpoint()

        self.k8s.wait_for_router_ready(self.scenario.backends.default_model, router_endpoint, timeout=300)

        # Start async pprof collection after the router is confirmed ready,
        # so the CPU profile covers the router under load (not just idle).
        metrics_config = getattr(self.scenario, "metrics", {}) or {}
        pprof_handle = None
        if metrics_config.get("pprof", False):
            pprof_handle = self.collector.start_pprof_collection(
                config_name, router_debug_endpoint, metrics_config,
            )
        try:
            result = self.runner.run(
                config_name=config_name,
                scenario=self.scenario,
                router_endpoint=router_endpoint,
                extra_args=self.scenario.aiperf.get("extraArgs"),
            )
        except subprocess.CalledProcessError as exc:
            # Benchmark tooling itself failed — the run is not a measurement
            # and must not be judged against backend stability signals.
            if pprof_handle is not None:
                pprof_handle.abandon()
            result = BenchmarkResult(
                config_name=config_name,
                scenario=self.scenario.name,
                timestamp="",
                metrics={},
                raw_output=f"aiperf exited with code {exc.returncode}",
                artifacts={},
                verdict={
                    "status": VERDICT_FRAMEWORK_ERROR,
                    "reasons": [f"aiperf exited with code {exc.returncode}"],
                    "offenders": [],
                    "restart_stats": {},
                },
            )
            return result

        # Steady-state validity check (post-traffic only): query mocker pods
        # for restarts / OOMKilled / CrashLoopBackOff that happened during
        # the measurement window. See issue #1271.
        restart_stats = self.k8s.get_mocker_pod_restart_stats()
        result.verdict = compute_run_verdict(restart_stats)
        print(f"  Run verdict for {config_name}: {result.verdict['status']}")
        for reason in result.verdict.get("reasons", []):
            print(f"    - {reason}")

        result.artifacts = self.collector.collect_artifacts(
            config_name=config_name,
            scenario=self.scenario,
            router_metrics_endpoint=router_endpoint,
            pprof_handle=pprof_handle,
        )
        return result

    def run(self) -> dict[str, Any]:
        try:
            result_a = self.run_single_config(str(self.router_config_a_path), "config_a")
            result_b = self.run_single_config(str(self.router_config_b_path), "config_b")
        finally:
            self.k8s.cleanup_port_forward()
            self.k8s.cleanup_backends()

        report = self.reporter.build_report(
            scenario_name=self.scenario.name,
            description=self.scenario.description,
            config_a_path=str(self.router_config_a_path),
            config_b_path=str(self.router_config_b_path),
            result_a=result_a,
            result_b=result_b,
        )

        report_path = self.output_dir / f"report_{self.scenario.name}.json"
        self.reporter.write_report(report_path, report)
        self.reporter.print_report(report)
        return report
