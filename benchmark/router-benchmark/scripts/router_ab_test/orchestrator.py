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

from pathlib import Path
from typing import Any

from router_ab_test.kubernetes import EndpointMode, K8sManager
from router_ab_test.load_generator import AIPerfRunner
from router_ab_test.metrics_collector import MetricsCollector
from router_ab_test.models import BenchmarkResult, ScenarioConfig
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
        self.k8s.cleanup_port_forward()  # Clean up any stale port-forwards before starting

        # Deploy mocker backends from scenario config
        self.k8s.deploy_backends(self.scenario.backends)

        self.k8s.apply_router_config(config_path)
        router_endpoint = self.k8s.get_router_endpoint()
        router_debug_endpoint = self.k8s.get_router_debug_endpoint()
        self.k8s.wait_for_router_ready("Qwen/Qwen3-0.6B", router_endpoint, timeout=300)
        result = self.runner.run(
            config_name=config_name,
            scenario=self.scenario,
            router_endpoint=router_endpoint,
            extra_args=self.scenario.aiperf.get("extraArgs"),
        )
        result.artifacts = self.collector.collect_artifacts(
            config_name=config_name,
            scenario=self.scenario,
            router_metrics_endpoint=router_endpoint,
            router_debug_endpoint=router_debug_endpoint,
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
