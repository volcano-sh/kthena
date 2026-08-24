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

"""Tier 2 Matrix Test Orchestrator for Kthena Router Benchmarks.

Runs an N-way matrix of scenarios x plugin chains (8 x 7 = 56 runs)
to characterize router scheduler plugin behavior under varied conditions.

Usage:
    python tier2_matrix.py --scenarios-dir scenarios/ --chains-dir plugins/ --output results/tier2/
    python tier2_matrix.py --dry-run --scenarios-dir scenarios/ --chains-dir plugins/
    python tier2_matrix.py --visualize --output results/tier2/
"""

from __future__ import annotations

import argparse
from pathlib import Path

import yaml

from router_ab_test import (
    EndpointMode,
    K8sManager,
    MatrixOrchestrator,
    ScenarioConfig,
)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Kthena Router Tier 2 Matrix Test Orchestrator")
    parser.add_argument(
        "--scenarios-dir",
        default="scenarios/",
        help="Directory containing tier2 scenario YAML files (default: scenarios/)",
    )
    parser.add_argument(
        "--chains-dir",
        default="plugins/",
        help="Directory containing router config chain YAML files (default: plugins/)",
    )
    parser.add_argument("--output", default="./results/tier2/", help="Output directory")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="Validate all YAMLs load correctly, print planned matrix, and exit",
    )
    parser.add_argument(
        "--visualize",
        action="store_true",
        help="Generate charts from the matrix report after completion",
    )
    parser.add_argument("--scenarios", nargs="*", help="Explicit scenario YAML files (overrides dir)")
    parser.add_argument("--chains", nargs="*", help="Explicit chain YAML files (overrides dir)")
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
        help="Router endpoint access mode: 'pf' for Kind clusters (default), "
             "'lb' for clusters with LoadBalancer support",
    )
    return parser


def collect_scenario_files(scenarios_dir: str, explicit_scenarios: list[str] | None) -> list[str]:
    """Collect tier2 scenario YAML files from directory or explicit list."""
    if explicit_scenarios:
        return sorted(explicit_scenarios)

    path = Path(scenarios_dir)
    if not path.exists():
        raise FileNotFoundError(f"Scenarios directory not found: {scenarios_dir}")

    # Match pattern: tier2-*.yaml
    files = sorted(path.glob("tier2-*.yaml"))
    if not files:
        print(f"Warning: No tier2-*.yaml files found in {scenarios_dir}")
    return [str(f) for f in files]


def collect_chain_files(chains_dir: str, explicit_chains: list[str] | None) -> list[str]:
    """Collect router config chain YAML files from directory or explicit list."""
    if explicit_chains:
        return sorted(explicit_chains)

    path = Path(chains_dir)
    if not path.exists():
        raise FileNotFoundError(f"Chains directory not found: {chains_dir}")

    # Match pattern: router-config-*.yaml
    files = sorted(path.glob("router-config-*.yaml"))
    if not files:
        print(f"Warning: No router-config-*.yaml files found in {chains_dir}")
    return [str(f) for f in files]


def main() -> None:
    parser = build_parser()
    args = parser.parse_args()

    # Collect scenario and chain files
    try:
        scenario_paths = collect_scenario_files(args.scenarios_dir, args.scenarios)
        chain_paths = collect_chain_files(args.chains_dir, args.chains)
    except FileNotFoundError as exc:
        print(f"Error: {exc}")
        raise SystemExit(1)

    total_runs = len(scenario_paths) * len(chain_paths)

    if args.dry_run:
        # Validate all YAMLs load correctly and print planned matrix
        print(f"Planned matrix: {len(scenario_paths)} scenarios x {len(chain_paths)} chains = {total_runs} runs\n")

        # Load and validate scenarios
        scenarios = []
        for sp in scenario_paths:
            try:
                scenario = ScenarioConfig.from_yaml(sp)
                scenarios.append((sp, scenario.name))
            except Exception as exc:
                print(f"Error loading scenario {sp}: {exc}")
                raise SystemExit(1)

        # Load and validate chains
        chains = []
        for cp in chain_paths:
            try:
                with open(cp) as f:
                    yaml.safe_load(f)
                chain_name = Path(cp).stem
                chains.append((cp, chain_name))
            except Exception as exc:
                print(f"Error loading chain {cp}: {exc}")
                raise SystemExit(1)

        print("Matrix runs:")
        run_idx = 0
        for scenario_path, scenario_name in scenarios:
            for chain_path, chain_name in chains:
                run_idx += 1
                print(f"  {run_idx}. scenario={scenario_name} ({scenario_path}) chain={chain_name} ({chain_path})")

        print(f"\nDry-run complete. All {len(scenarios)} scenarios and {len(chains)} chains validated successfully.")
        return

    # Execute the full matrix test
    orchestrator = MatrixOrchestrator(
        scenario_paths=scenario_paths,
        chain_paths=chain_paths,
        output_dir=args.output,
        local_port=args.local_port,
        endpoint_mode=args.endpoint_mode,
    )

    orchestrator.run_matrix()
    report_path = Path(args.output) / "tier2_matrix_report.json"

    if args.visualize:
        try:
            from visualize_matrix import generate_charts
            charts_dir = Path(args.output) / "charts"
            generate_charts(str(report_path), str(charts_dir))
            print(f"Charts generated in: {charts_dir}")
        except ImportError:
            print("Warning: visualize_matrix.py not found or matplotlib not installed")


if __name__ == "__main__":
    main()
