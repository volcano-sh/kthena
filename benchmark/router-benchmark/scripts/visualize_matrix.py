#!/usr/bin/env python3
"""Visualization for Tier 2 matrix benchmark reports.

Generates grouped bar charts from tier2_matrix_report.json:
  - TTFT average (ms)
  - Latency average (ms)
  - Throughput (rps)
  - Request duration (ms)

Usage:
    python visualize_matrix.py --input results/tier2/tier2_matrix_report.json --output results/tier2/charts/
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

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
        ax.set_xticklabels([s.replace("tier2-", "").replace("latency-variance-composite", "latency-var") for s in scenarios],
                          rotation=45, ha="right")
        ax.legend(title="Plugin Chain", bbox_to_anchor=(1.05, 1), loc="upper left", fontsize="small")
        ax.grid(axis="y", alpha=0.3)

        plt.tight_layout()
        file_path = output_path / f"{metric_key}.png"
        plt.savefig(str(file_path), dpi=150, bbox_inches="tight")
        plt.close()
        generated_files.append(str(file_path))
        print(f"Generated: {file_path}")

    # 4th chart: request_duration_avg_ms from router_analysis
    fig, ax = plt.subplots(figsize=(14, 6))
    for i, chain in enumerate(chains):
        values = []
        for scenario in scenarios:
            cell = matrix[scenario].get(chain, {})
            verdict = (cell.get("verdict") or {}).get("status", "")
            router_analysis = cell.get("router_analysis")

            if verdict == "valid" and router_analysis:
                # Try to get request_duration_seconds for 2xx status codes
                duration = router_analysis.get("request_duration_seconds", {})
                avg_ms = None
                for code, stats in duration.items():
                    if code.startswith("2"):
                        avg_ms = stats.get("avg_ms")
                        break
                if avg_ms is None:
                    # Fallback to latency_avg_ms from metrics
                    avg_ms = (cell.get("metrics") or {}).get("latency_avg_ms", 0)
                values.append(avg_ms)
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
    ax.set_ylabel("Duration (ms)")
    ax.set_title("Request Duration Average (ms)")
    ax.set_xticks(x)
    ax.set_xticklabels([s.replace("tier2-", "").replace("latency-variance-composite", "latency-var") for s in scenarios],
                      rotation=45, ha="right")
    ax.legend(title="Plugin Chain", bbox_to_anchor=(1.05, 1), loc="upper left", fontsize="small")
    ax.grid(axis="y", alpha=0.3)

    plt.tight_layout()
    file_path = output_path / "request_duration_avg_ms.png"
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
