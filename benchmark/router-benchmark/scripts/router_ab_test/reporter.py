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

import json
from datetime import datetime
from pathlib import Path
from typing import Any

from router_ab_test.models import BenchmarkResult

_METRIC_SPECS: dict[str, dict[str, Any]] = {
    "ttft_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
    "latency_avg_ms": {"higher_is_better": False, "regression_threshold": 5},
    "throughput_rps": {"higher_is_better": True, "regression_threshold": 10},
}


class ResultReporter:
    """Build, persist, and print A/B benchmark reports."""

    def compare(self, result_a: BenchmarkResult, result_b: BenchmarkResult) -> dict[str, Any]:
        comparison: dict[str, Any] = {}
        for metric, spec in _METRIC_SPECS.items():
            val_a = result_a.metrics.get(metric)
            val_b = result_b.metrics.get(metric)
            if val_a is None or val_b is None or val_a == 0:
                continue

            delta_pct = self._calculate_delta_pct(val_a, val_b, spec["higher_is_better"])
            comparison[metric] = {
                "config_a": val_a,
                "config_b": val_b,
                "delta_pct": round(delta_pct, 2),
                "regression": delta_pct < -spec["regression_threshold"],
            }
        return comparison

    def build_report(
        self,
        scenario_name: str,
        description: str,
        config_a_path: str,
        config_b_path: str,
        result_a: BenchmarkResult,
        result_b: BenchmarkResult,
    ) -> dict[str, Any]:
        return {
            "scenario": scenario_name,
            "description": description,
            "timestamp": datetime.now().isoformat(),
            "config_a": {
                "path": config_a_path,
                "metrics": result_a.metrics,
                "artifacts": result_a.artifacts,
            },
            "config_b": {
                "path": config_b_path,
                "metrics": result_b.metrics,
                "artifacts": result_b.artifacts,
            },
            "comparison": self.compare(result_a, result_b),
        }

    def write_report(self, output_path: str | Path, report: dict[str, Any]) -> None:
        output_path = Path(output_path)
        with output_path.open("w", encoding="utf-8") as file:
            json.dump(report, file, indent=2)

    def print_report(self, report: dict[str, Any]) -> None:
        print("\n" + "=" * 70)
        print(f"A/B Test Report: {report['scenario']}")
        print(f"Description: {report['description']}")
        print("=" * 70)

        print(f"\nRouter Config A: {report['config_a']['path']}")
        for key, value in report["config_a"]["metrics"].items():
            print(f"  {key}: {value}")

        if report["config_a"]["artifacts"]:
            print("  artifacts:")
            for key in report["config_a"]["artifacts"]:
                print(f"    - {key}")

        print(f"\nRouter Config B: {report['config_b']['path']}")
        for key, value in report["config_b"]["metrics"].items():
            print(f"  {key}: {value}")

        if report["config_b"]["artifacts"]:
            print("  artifacts:")
            for key in report["config_b"]["artifacts"]:
                print(f"    - {key}")

        print("\nComparison (B vs A, positive delta means improvement):")
        for metric, data in report["comparison"].items():
            status = "REGRESSION" if data["regression"] else "OK"
            print(f"  {metric}: {data['delta_pct']:+.2f}% [{status}]")

        print("=" * 70)

    @staticmethod
    def _calculate_delta_pct(config_a_value: float, config_b_value: float, higher_is_better: bool) -> float:
        if higher_is_better:
            return ((config_b_value - config_a_value) / config_a_value) * 100
        return ((config_a_value - config_b_value) / config_a_value) * 100
