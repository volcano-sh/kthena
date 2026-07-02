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
import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT_PATH = Path(__file__).resolve().parents[1] / "scripts" / "ab_test.py"
SCRIPT_ROOT = SCRIPT_PATH.parent
if str(SCRIPT_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPT_ROOT))

SPEC = importlib.util.spec_from_file_location("benchmark_ab_test", SCRIPT_PATH)
assert SPEC is not None
ab_test = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(ab_test)


class CompareMetricsTest(unittest.TestCase):
    def make_result(self, **metrics):
        return ab_test.BenchmarkResult(
            config_name="config",
            scenario="scenario",
            timestamp="2026-06-30T00:00:00",
            metrics=metrics,
            raw_output="",
        )

    def test_compare_reports_positive_improvement_for_lower_latency_and_higher_throughput(self):
        result_a = self.make_result(
            ttft_avg_ms=100.0,
            latency_avg_ms=200.0,
            throughput_rps=50.0,
        )
        result_b = self.make_result(
            ttft_avg_ms=80.0,
            latency_avg_ms=150.0,
            throughput_rps=60.0,
        )

        comparison = ab_test.ResultReporter().compare(result_a, result_b)

        self.assertEqual(comparison["ttft_avg_ms"]["delta_pct"], 20.0)
        self.assertEqual(comparison["latency_avg_ms"]["delta_pct"], 25.0)
        self.assertEqual(comparison["throughput_rps"]["delta_pct"], 20.0)
        self.assertFalse(comparison["ttft_avg_ms"]["regression"])
        self.assertFalse(comparison["latency_avg_ms"]["regression"])
        self.assertFalse(comparison["throughput_rps"]["regression"])

    def test_compare_marks_regression_when_latency_rises_or_throughput_drops(self):
        result_a = self.make_result(
            ttft_avg_ms=100.0,
            latency_avg_ms=200.0,
            throughput_rps=50.0,
        )
        result_b = self.make_result(
            ttft_avg_ms=106.0,
            latency_avg_ms=212.0,
            throughput_rps=44.0,
        )

        comparison = ab_test.ResultReporter().compare(result_a, result_b)

        self.assertEqual(comparison["ttft_avg_ms"]["delta_pct"], -6.0)
        self.assertEqual(comparison["latency_avg_ms"]["delta_pct"], -6.0)
        self.assertEqual(comparison["throughput_rps"]["delta_pct"], -12.0)
        self.assertTrue(comparison["ttft_avg_ms"]["regression"])
        self.assertTrue(comparison["latency_avg_ms"]["regression"])
        self.assertTrue(comparison["throughput_rps"]["regression"])

    def test_report_builder_keeps_paths_metrics_artifacts_and_comparison(self):
        result_a = self.make_result(ttft_avg_ms=100.0, throughput_rps=50.0)
        result_b = self.make_result(ttft_avg_ms=80.0, throughput_rps=55.0)
        result_a.artifacts = {"prometheus": {"sample_count": 12}}
        result_b.artifacts = {"pprof": {"profiles": {"heap": "path"}}}

        report = ab_test.ResultReporter().build_report(
            scenario_name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            config_a_path="k8s/router-config-random.yaml",
            config_b_path="k8s/router-config-least-latency.yaml",
            result_a=result_a,
            result_b=result_b,
        )

        self.assertEqual(report["scenario"], "smoke-test-s2-latency-vs-qps")
        self.assertEqual(report["config_a"]["path"], "k8s/router-config-random.yaml")
        self.assertEqual(report["config_b"]["path"], "k8s/router-config-least-latency.yaml")
        self.assertEqual(report["config_a"]["metrics"], result_a.metrics)
        self.assertEqual(report["config_b"]["metrics"], result_b.metrics)
        self.assertEqual(report["config_a"]["artifacts"], result_a.artifacts)
        self.assertEqual(report["config_b"]["artifacts"], result_b.artifacts)
        self.assertIn("ttft_avg_ms", report["comparison"])


class AIPerfRunnerTest(unittest.TestCase):
    def setUp(self):
        self.runner = ab_test.AIPerfRunner(tempfile.mkdtemp())

    def test_build_aiperf_cmd_maps_scenario_fields_to_cli_flags(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            load={
                "duration": "5m",
                "schedule": {"mode": "constant_rate", "rate": 25},
                "traffic": {
                    "burstiness": 0.5,
                    "ramp": {"strategy": "linear"},
                },
                "concurrency": {"connections": 42},
                "prompts": [{"tokens": 100}, {"tokens": 4000}],
                "max_tokens": [{"tokens": 128}, {"tokens": 1024}],
            },
            backends={},
            routing={},
        )

        cmd = self.runner.build_aiperf_cmd(
            config_name="config_a",
            scenario=scenario,
            router_endpoint="localhost:8080",
        )

        self.assertIn("--benchmark-duration", cmd)
        self.assertIn("300", cmd)
        self.assertIn("--request-rate", cmd)
        self.assertIn("25", cmd)
        self.assertIn("--concurrency", cmd)
        self.assertIn("42", cmd)
        self.assertIn("--arrival-pattern", cmd)
        self.assertIn("gamma", cmd)
        self.assertIn("--arrival-smoothness", cmd)
        self.assertIn("0.5", cmd)
        self.assertIn("--request-rate-ramp-duration", cmd)
        self.assertIn("--synthetic-input-tokens-mean", cmd)
        self.assertIn("100,4000", cmd)
        self.assertIn("--output-tokens-mean", cmd)
        self.assertIn("128,1024", cmd)

    def test_parse_duration_seconds_supports_seconds_minutes_and_hours(self):
        self.assertEqual(self.runner._parse_duration_seconds("60s"), 60)
        self.assertEqual(self.runner._parse_duration_seconds("5m"), 300)
        self.assertEqual(self.runner._parse_duration_seconds("2H"), 7200)


class MetricsCollectorTest(unittest.TestCase):
    def setUp(self):
        self.output_dir = Path(tempfile.mkdtemp())
        self.collector = ab_test.MetricsCollector(self.output_dir)

    def test_collect_artifacts_fetches_prometheus_and_pprof_profiles(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            load={"duration": "60s"},
            backends={},
            routing={},
            metrics={
                "prometheus": True,
                "pprof": True,
                "cpuProfileSeconds": 7,
                "profiles": ["heap", "goroutine"],
            },
        )
        requested_urls = []

        def fake_fetch_text(url):
            requested_urls.append(url)
            return "go_goroutines 17\nprocess_resident_memory_bytes 42\n"

        def fake_fetch_bytes(url):
            requested_urls.append(url)
            return f"payload:{url}".encode()

        with mock.patch.object(self.collector, "_fetch_text", side_effect=fake_fetch_text):
            with mock.patch.object(self.collector, "_fetch_bytes", side_effect=fake_fetch_bytes):
                artifacts = self.collector.collect_artifacts(
                    config_name="config_a",
                    scenario=scenario,
                    router_metrics_endpoint="localhost:8080",
                    router_debug_endpoint="localhost:18080",
                )

        self.assertEqual(artifacts["prometheus"]["key_metrics"]["go_goroutines"], 17.0)
        self.assertEqual(artifacts["prometheus"]["key_metrics"]["process_resident_memory_bytes"], 42.0)
        self.assertTrue((self.output_dir / "config_a" / "router_metrics.prom").exists())
        self.assertTrue((self.output_dir / "config_a" / "pprof" / "cpu.pb.gz").exists())
        self.assertTrue((self.output_dir / "config_a" / "pprof" / "heap.pb.gz").exists())
        self.assertTrue((self.output_dir / "config_a" / "pprof" / "goroutine.pb.gz").exists())
        self.assertIn("http://localhost:8080/metrics", requested_urls)
        self.assertIn("http://localhost:18080/debug/pprof/profile?seconds=7", requested_urls)
        self.assertIn("http://localhost:18080/debug/pprof/heap", requested_urls)
        self.assertIn("http://localhost:18080/debug/pprof/goroutine", requested_urls)

    def test_collect_artifacts_skips_when_metrics_collection_disabled(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            load={"duration": "60s"},
            backends={},
            routing={},
        )

        with mock.patch.object(self.collector, "_fetch_text") as fetch_text:
            with mock.patch.object(self.collector, "_fetch_bytes") as fetch_bytes:
                artifacts = self.collector.collect_artifacts(
                    config_name="config_a",
                    scenario=scenario,
                    router_metrics_endpoint="localhost:8080",
                    router_debug_endpoint="localhost:18080",
                )

        self.assertEqual(artifacts, {})
        fetch_text.assert_not_called()
        fetch_bytes.assert_not_called()

    def test_build_router_debug_patch_exposes_debug_container_port(self):
        patch = self.collector.build_router_debug_patch()

        self.assertEqual(patch["spec"]["template"]["spec"]["containers"][0]["name"], "kthena-router")
        self.assertEqual(
            patch["spec"]["template"]["spec"]["containers"][0]["ports"],
            [{"containerPort": 15000, "name": "debug"}],
        )


class MainTest(unittest.TestCase):
    def test_main_exits_non_zero_when_report_contains_regression(self):
        report = {"comparison": {"latency_avg_ms": {"regression": True}}}
        args = mock.Mock(
            scenario="scenario.yaml",
            router_config_a="config-a.yaml",
            router_config_b="config-b.yaml",
            output="./results",
            local_port=ab_test.K8sManager.DEFAULT_LOCAL_PORT,
            mocker_manifest=None,
        )
        parser = mock.Mock()
        parser.parse_args.return_value = args

        with mock.patch.object(ab_test, "ABTestOrchestrator") as orchestrator_cls:
            orchestrator_cls.return_value.run.return_value = report
            with mock.patch.object(ab_test, "build_parser", return_value=parser):
                with self.assertRaises(SystemExit) as exit_ctx:
                    ab_test.main()

        self.assertEqual(exit_ctx.exception.code, 1)


if __name__ == "__main__":
    unittest.main()
