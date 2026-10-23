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

"""Visualization for Tier 2 matrix benchmark reports.

Generates grouped bar charts from tier2_matrix_report.json:
  - TTFT average (ms)
  - TPOT average (ms)
  - Latency average (ms)
  - Throughput (rps)

Usage:
    python visualize_matrix.py --input results/tier2/tier2_matrix_report.json --output results/tier2/charts/
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

import matplotlib
matplotlib.use("Agg")  # Non-interactive backend for server/CI use
import matplotlib.pyplot as plt
import numpy as np


def generate_charts(report_path: str, output_dir: str) -> list[str]:
    """Generate 4 PNG charts from a Tier 2 matrix report JSON.

    Returns list of generated file paths.
    """
    with open(report_path, encoding="utf-8") as f:
        report = json.load(f)

    matrix = report.get("matrix", {})
    if not matrix:
        print("Warning: matrix is empty, no charts to generate")
        return []

    scenarios = sorted(matrix.keys())
    # Get chain names from the first scenario (all scenarios should have the same chains)
    first_scenario = matrix[scenarios[0]]
    chains = sorted(first_scenario.keys())

    output_path = Path(output_dir)
    output_path.mkdir(parents=True, exist_ok=True)

    # Define metrics to chart
    metric_specs = [
        ("ttft_avg_ms", "TTFT Average (ms)", "ms"),
        ("tpot_avg_ms", "TPOT Average (ms)", "ms"),
        ("latency_avg_ms", "Latency Average (ms)", "ms"),
        ("throughput_rps", "Throughput (req/s)", "rps"),
    ]

    generated_files = []
    colors = plt.cm.tab10(np.linspace(0, 1, len(chains)))

    x = np.arange(len(scenarios))
    width = 0.8 / len(chains)  # Scale bar width to fit all chains

    for metric_key, title, unit in metric_specs:
        fig, ax = plt.subplots(figsize=(14, 6))

        for i, chain in enumerate(chains):
            values = []
            for scenario in scenarios:
                cell = matrix[scenario].get(chain, {})
                verdict = (cell.get("verdict") or {}).get("status", "")
                metrics = cell.get("metrics")

                if verdict == "valid" and metrics:
                    values.append(metrics.get(metric_key, 0))
                else:
                    values.append(0)  # Invalid/missing runs plotted as 0

            offset = (i - len(chains) / 2 + 0.5) * width
            bars = ax.bar(x + offset, values, width, label=chain, color=colors[i])

            # Add hatch pattern for invalid runs (value is 0 and verdict is not valid)
            for j, (bar, scenario) in enumerate(zip(bars, scenarios)):
                cell = matrix[scenario].get(chain, {})
                verdict = (cell.get("verdict") or {}).get("status", "")
                if verdict != "valid":
                    bar.set_hatch("//")

        ax.set_xlabel("Scenario")
        ax.set_ylabel(f"Value ({unit})")
        ax.set_title(title)
        ax.set_xticks(x)
        # Truncate scenario names for readability
        labels = [s.replace("tier2-", "").replace("latency-variance-composite", "latency-var")
                  for s in scenarios]
        ax.set_xticklabels(labels, rotation=45, ha="right")
        ax.legend(title="Plugin Chain", bbox_to_anchor=(1.05, 1), loc="upper left", fontsize="small")
        ax.grid(axis="y", alpha=0.3)

        plt.tight_layout()
        file_path = output_path / f"{metric_key}.png"
        plt.savefig(str(file_path), dpi=150, bbox_inches="tight")
        plt.close()
        generated_files.append(str(file_path))
        print(f"Generated: {file_path}")

    # 5th chart: plugin scheduling latency from router_analysis
    fig, ax = plt.subplots(figsize=(14, 6))
    for i, chain in enumerate(chains):
        values = []
        for scenario in scenarios:
            cell = matrix[scenario].get(chain, {})
            verdict = (cell.get("verdict") or {}).get("status", "")
            router_analysis = cell.get("router_analysis")

            if verdict == "valid" and router_analysis:
                plugins = router_analysis.get("scheduler_plugins", {})
                if plugins:
                    # Calculate average plugin scheduling latency
                    avg_latencies = [p.get("avg_ms", 0) for p in plugins.values() if p.get("avg_ms") is not None]
                    values.append(sum(avg_latencies) / len(avg_latencies) if avg_latencies else 0)
                else:
                    values.append(0)
            else:
                values.append(0)

        offset = (i - len(chains) / 2 + 0.5) * width
        bars = ax.bar(x + offset, values, width, label=chain, color=colors[i])
        for j, (bar, scenario) in enumerate(zip(bars, scenarios)):
            cell = matrix[scenario].get(chain, {})
            verdict = (cell.get("verdict") or {}).get("status", "")
            if verdict != "valid":
                bar.set_hatch("//")

    ax.set_xlabel("Scenario")
    ax.set_ylabel("Plugin Scheduling Latency (ms)")
    ax.set_title("Average Plugin Scheduling Latency (ms)")
    ax.set_xticks(x)
    labels = [s.replace("tier2-", "").replace("latency-variance-composite", "latency-var") for s in scenarios]
    ax.set_xticklabels(labels, rotation=45, ha="right")
    ax.legend(title="Plugin Chain", bbox_to_anchor=(1.05, 1), loc="upper left", fontsize="small")
    ax.grid(axis="y", alpha=0.3)

    plt.tight_layout()
    file_path = output_path / "plugin_scheduling_latency_avg_ms.png"
    plt.savefig(str(file_path), dpi=150, bbox_inches="tight")
    plt.close()
    generated_files.append(str(file_path))
    print(f"Generated: {file_path}")

    print(f"\nGenerated {len(generated_files)} charts in {output_dir}")
    print("Note: Invalid/missing runs shown with hatch pattern (//) and value 0")
    return generated_files


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate visualization charts from Tier 2 matrix report")
    parser.add_argument("--input", "-i", required=True, help="Path to tier2_matrix_report.json")
    parser.add_argument("--output", "-o", default="./charts", help="Output directory for PNG files")
    args = parser.parse_args()

    generate_charts(args.input, args.output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
