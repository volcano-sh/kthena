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
import threading
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
            config_a_path="plugins/router-config-random.yaml",
            config_b_path="plugins/router-config-least-latency.yaml",
            result_a=result_a,
            result_b=result_b,
        )

        self.assertEqual(report["scenario"], "smoke-test-s2-latency-vs-qps")
        self.assertEqual(report["config_a"]["path"], "plugins/router-config-random.yaml")
        self.assertEqual(report["config_b"]["path"], "plugins/router-config-least-latency.yaml")
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
        # Rate mode: concurrency settings must not leak into the command —
        # a scenario expresses exactly one load model (open-loop here).
        self.assertNotIn("--concurrency", cmd)
        self.assertIn("--arrival-pattern", cmd)
        self.assertIn("gamma", cmd)
        self.assertIn("--arrival-smoothness", cmd)
        self.assertIn("0.5", cmd)
        self.assertIn("--request-rate-ramp-duration", cmd)
        self.assertIn("--synthetic-input-tokens-mean", cmd)
        self.assertIn("100,4000", cmd)
        self.assertIn("--output-tokens-mean", cmd)
        self.assertIn("128,1024", cmd)

    def test_build_aiperf_cmd_concurrency_mode_omits_request_rate(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s3-concurrency-scaling",
            description="scenario",
            load={
                "duration": "60s",
                "schedule": {"mode": "concurrency"},
                "traffic": {
                    "burstiness": 1.0,
                    "ramp": {"strategy": "linear"},
                },
                "concurrency": {"connections": 200},
            },
            backends={},
        )

        cmd = self.runner.build_aiperf_cmd(
            config_name="config_a",
            scenario=scenario,
            router_endpoint="localhost:8080",
        )

        self.assertIn("--concurrency", cmd)
        self.assertEqual(cmd[cmd.index("--concurrency") + 1], "200")
        # Closed-loop mode: rate-based flags must not appear.
        self.assertNotIn("--request-rate", cmd)
        self.assertNotIn("--arrival-pattern", cmd)
        self.assertNotIn("--request-rate-ramp-duration", cmd)

    def test_parse_duration_seconds_supports_seconds_minutes_and_hours(self):
        self.assertEqual(self.runner._parse_duration_seconds("60s"), 60)
        self.assertEqual(self.runner._parse_duration_seconds("5m"), 300)
        self.assertEqual(self.runner._parse_duration_seconds("2H"), 7200)

    def test_build_aiperf_cmd_maps_ramp_family_arguments(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            load={
                "duration": "2m",
                "schedule": {"mode": "rate", "rate": 12},
                "traffic": {
                    "burstiness": 1.0,
                    "ramp": {
                        "strategy": "linear",
                        "duration": "30s",
                        "request_rate_duration": 25,
                    },
                },
                "concurrency": {
                    "connections": 42,
                    "ramp": {
                        "strategy": "linear",
                        "duration": "15s",
                        "prefill_duration": "12s",
                    },
                },
            },
            backends={},
        )

        cmd = self.runner.build_aiperf_cmd(
            config_name="config_a",
            scenario=scenario,
            router_endpoint="localhost:8080",
        )

        # Rate mode: only the request-rate ramp applies; concurrency ramp
        # settings are ignored because --concurrency is not emitted.
        self.assertIn("--request-rate-ramp-duration", cmd)
        self.assertEqual(cmd[cmd.index("--request-rate-ramp-duration") + 1], "25")
        self.assertNotIn("--concurrency-ramp-duration", cmd)
        self.assertNotIn("--prefill-concurrency-ramp-duration", cmd)

    def test_build_aiperf_cmd_maps_concurrency_ramp_in_concurrency_mode(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s3-concurrency-scaling",
            description="scenario",
            load={
                "duration": "2m",
                "schedule": {"mode": "concurrency"},
                "concurrency": {
                    "connections": 42,
                    "ramp": {
                        "strategy": "linear",
                        "duration": "15s",
                        "prefill_duration": "12s",
                    },
                },
            },
            backends={},
        )

        cmd = self.runner.build_aiperf_cmd(
            config_name="config_a",
            scenario=scenario,
            router_endpoint="localhost:8080",
        )

        self.assertIn("--concurrency-ramp-duration", cmd)
        self.assertEqual(cmd[cmd.index("--concurrency-ramp-duration") + 1], "15")
        self.assertIn("--prefill-concurrency-ramp-duration", cmd)
        self.assertEqual(cmd[cmd.index("--prefill-concurrency-ramp-duration") + 1], "12")
        self.assertNotIn("--request-rate-ramp-duration", cmd)

    def test_build_aiperf_cmd_ignores_none_ramp_strategy(self):
        scenario = ab_test.ScenarioConfig(
            name="smoke-test-s2-latency-vs-qps",
            description="scenario",
            load={
                "duration": "60s",
                "schedule": {"mode": "rate", "rate": 12},
                "traffic": {
                    "burstiness": 1.0,
                    "ramp": {"strategy": "none", "duration": "20s"},
                },
                "concurrency": {
                    "connections": 42,
                    "ramp": {"strategy": "none", "duration": "15s"},
                },
            },
            backends={},
        )

        cmd = self.runner.build_aiperf_cmd(
            config_name="config_a",
            scenario=scenario,
            router_endpoint="localhost:8080",
        )

        self.assertNotIn("--request-rate-ramp-duration", cmd)
        self.assertNotIn("--concurrency-ramp-duration", cmd)
        self.assertNotIn("--prefill-concurrency-ramp-duration", cmd)

    def _write_summary(self, run_dir: Path, error_summary=None) -> None:
        import json

        run_dir.mkdir(parents=True, exist_ok=True)
        summary = {
            "time_to_first_token": {"avg": 100.0},
            "request_latency": {"avg": 200.0},
            "request_throughput": {"avg": 5.0},
            "error_summary": error_summary or [],
        }
        (run_dir / "profile_export_aiperf.json").write_text(json.dumps(summary), encoding="utf-8")

    def test_read_metrics_from_output_sums_genuine_errors(self):
        run_dir = Path(tempfile.mkdtemp())
        self._write_summary(
            run_dir,
            error_summary=[
                {"error_details": {"code": 503}, "count": 53},
                {"error_details": {"type": "InvalidInferenceResultError"}, "count": 1},
            ],
        )

        metrics = self.runner._read_metrics_from_output(run_dir)

        self.assertEqual(metrics["aiperf_genuine_errors"], 54)

    def test_read_metrics_from_output_zero_errors_when_summary_empty(self):
        run_dir = Path(tempfile.mkdtemp())
        self._write_summary(run_dir, error_summary=[])

        metrics = self.runner._read_metrics_from_output(run_dir)

        self.assertEqual(metrics["aiperf_genuine_errors"], 0)

    def test_read_cancelled_count_parses_phase_summary_line(self):
        run_dir = Path(tempfile.mkdtemp())
        self._write_summary(run_dir)
        log_dir = run_dir / "logs"
        log_dir.mkdir(parents=True, exist_ok=True)
        (log_dir / "aiperf.log").write_text(
            "2026-08-01 07:32:03.010 - PhaseRunner - NOTICE - "
            "Phase profiling complete | completed=237, cancelled=45, errors=19 | "
            "sessions: completed=237, cancelled=45\n",
            encoding="utf-8",
        )

        metrics = self.runner._read_metrics_from_output(run_dir)

        self.assertEqual(metrics["aiperf_cancelled"], 45)

    def test_read_metrics_from_output_omits_cancelled_when_log_missing(self):
        run_dir = Path(tempfile.mkdtemp())
        self._write_summary(run_dir)
        # No logs/aiperf.log written.

        metrics = self.runner._read_metrics_from_output(run_dir)

        self.assertNotIn("aiperf_cancelled", metrics)


class BackendsConfigTest(unittest.TestCase):
    def test_profile_resources_are_parsed_from_yaml_dict(self):
        config = ab_test.ScenarioConfig(
            name="s4",
            description="d",
            load={},
            backends={
                "common": {"engineType": "sglang", "model": "Qwen/Qwen3-0.6B"},
                "profiles": [
                    {
                        "name": "homogeneous",
                        "count": 6,
                        "resources": {
                            "requests": {"cpu": "250m", "memory": "256Mi"},
                            "limits": {"cpu": "1", "memory": "1Gi"},
                        },
                    },
                ],
            },
        )

        profile = config.backends.profiles[0]
        self.assertEqual(profile.resources["requests"]["cpu"], "250m")
        self.assertEqual(profile.resources["limits"]["memory"], "1Gi")

    def test_builder_uses_profile_resources_when_present(self):
        from router_ab_test.kubernetes import MockerDeploymentBuilder

        config = ab_test.ScenarioConfig(
            name="s4",
            description="d",
            load={},
            backends={
                "common": {"engineType": "sglang", "model": "Qwen/Qwen3-0.6B"},
                "profiles": [
                    {
                        "name": "homogeneous",
                        "count": 6,
                        "resources": {
                            "requests": {"cpu": "250m", "memory": "256Mi"},
                            "limits": {"cpu": "1", "memory": "1Gi"},
                        },
                    },
                ],
            },
        )

        builder = MockerDeploymentBuilder()
        deployment = builder._build_deployment(config.backends.profiles[0], config.backends)
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["resources"]["requests"]["cpu"], "250m")
        self.assertEqual(container["resources"]["requests"]["memory"], "256Mi")
        self.assertEqual(container["resources"]["limits"]["cpu"], "1")
        self.assertEqual(container["resources"]["limits"]["memory"], "1Gi")

    def test_builder_falls_back_to_defaults_when_profile_has_no_resources(self):
        from router_ab_test.kubernetes import MockerDeploymentBuilder

        config = ab_test.ScenarioConfig(
            name="s2",
            description="d",
            load={},
            backends={
                "common": {"engineType": "sglang", "model": "Qwen/Qwen3-0.6B"},
                "profiles": [{"name": "homogeneous", "count": 4}],
            },
        )

        builder = MockerDeploymentBuilder()
        deployment = builder._build_deployment(config.backends.profiles[0], config.backends)
        container = deployment["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(container["resources"]["requests"]["cpu"], "500m")
        self.assertEqual(container["resources"]["requests"]["memory"], "512Mi")
        self.assertEqual(container["resources"]["limits"]["cpu"], "2")
        self.assertEqual(container["resources"]["limits"]["memory"], "2Gi")


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

        def fake_fetch_bytes(url, timeout=60):
            requested_urls.append(url)
            return f"payload:{url}".encode()

        with mock.patch.object(self.collector, "_fetch_text", side_effect=fake_fetch_text):
            with mock.patch.object(self.collector, "_fetch_bytes", side_effect=fake_fetch_bytes):
                handle = self.collector.start_pprof_collection(
                    "config_a", "localhost:18080", scenario.metrics
                )
                artifacts = self.collector.collect_artifacts(
                    config_name="config_a",
                    scenario=scenario,
                    router_metrics_endpoint="localhost:8080",
                    pprof_handle=handle,
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
        )

        with mock.patch.object(self.collector, "_fetch_text") as fetch_text:
            with mock.patch.object(self.collector, "_fetch_bytes") as fetch_bytes:
                artifacts = self.collector.collect_artifacts(
                    config_name="config_a",
                    scenario=scenario,
                    router_metrics_endpoint="localhost:8080",
                )

        self.assertEqual(artifacts, {})
        fetch_text.assert_not_called()
        fetch_bytes.assert_not_called()

    def test_start_pprof_collection_fetches_profiles(self):
        fetched = []

        def fake_fetch(url, timeout=60):
            fetched.append(url)
            return b"pb"

        with mock.patch.object(self.collector, "_fetch_bytes", side_effect=fake_fetch):
            handle = self.collector.start_pprof_collection(
                "config_a",
                "localhost:18080",
                {"pprof": True, "cpuProfileSeconds": 7, "profiles": ["heap"]},
            )
            result = handle.result(timeout=10)

        self.assertIn("http://localhost:18080/debug/pprof/profile?seconds=7", fetched)
        self.assertIn("http://localhost:18080/debug/pprof/heap", fetched)
        self.assertTrue((self.output_dir / "config_a" / "pprof" / "cpu.pb.gz").exists())
        self.assertIn("cpu", result["profiles"])

    def test_cpu_profile_fetch_timeout_scales_with_seconds(self):
        calls = []

        def fake_fetch(url, timeout=60):
            calls.append((url, timeout))
            return b"pb"

        with mock.patch.object(self.collector, "_fetch_bytes", side_effect=fake_fetch):
            handle = self.collector.start_pprof_collection(
                "config_a",
                "localhost:18080",
                {"pprof": True, "cpuProfileSeconds": 90, "profiles": []},
            )
            handle.result(timeout=10)

        cpu_call = next(c for c in calls if "profile?seconds=90" in c[0])
        self.assertEqual(cpu_call[1], 120)  # seconds + 30 margin

    def test_pprof_thread_error_is_captured_not_raised(self):
        with mock.patch.object(self.collector, "_fetch_bytes", side_effect=OSError("boom")):
            handle = self.collector.start_pprof_collection(
                "config_a",
                "localhost:18080",
                {"pprof": True, "cpuProfileSeconds": 1},
            )
            result = handle.result(timeout=10)
        self.assertIn("boom", result["error"])

    def test_pprof_abandon_then_result_returns_error(self):
        block = threading.Event()

        def hanging_fetch(url, timeout=60):
            block.wait(5)  # simulates a stuck fetch; abandon() must not wait for it
            return b"pb"

        with mock.patch.object(self.collector, "_fetch_bytes", side_effect=hanging_fetch):
            handle = self.collector.start_pprof_collection(
                "config_a",
                "localhost:18080",
                {"pprof": True},
            )
            handle.abandon()
            result = handle.result(timeout=0.2)  # thread still alive -> timeout branch
            block.set()  # let the daemon thread finish
        self.assertIn("error", result)


class OrchestratorPprofTest(unittest.TestCase):
    """Test async pprof wiring in run_single_config."""

    def setUp(self):
        self.scenario = ab_test.ScenarioConfig(
            name="test",
            description="test",
            load={"duration": "10s"},
            backends={},
            metrics={"pprof": True, "cpuProfileSeconds": 7},
        )
        self.collector = mock.MagicMock()
        self.collector.start_pprof_collection.return_value = mock.MagicMock()
        self.k8s = mock.MagicMock()
        self.runner = mock.MagicMock()
        self.runner.run.return_value = ab_test.BenchmarkResult(
            config_name="config_a",
            scenario="test",
            timestamp="",
            metrics={},
            raw_output="",
        )

    def test_starts_pprof_collection_before_aiperf(self):
        events = []

        def track_start(*args, **kwargs):
            events.append("pprof_start")
            return self.collector.start_pprof_collection.return_value

        self.collector.start_pprof_collection.side_effect = track_start
        self.runner.run.side_effect = lambda **kw: (events.append("aiperf") or self.runner.run.return_value)

        orch = ab_test.ABTestOrchestrator.__new__(ab_test.ABTestOrchestrator)
        orch.scenario = self.scenario
        orch.k8s = self.k8s
        orch.runner = self.runner
        orch.collector = self.collector
        orch.router_config_a_path = mock.MagicMock()
        orch.router_config_b_path = mock.MagicMock()
        orch.output_dir = mock.MagicMock()

        orch.run_single_config("a.yaml", "config_a")

        self.collector.start_pprof_collection.assert_called_once()
        self.runner.run.assert_called_once()
        self.assertEqual(events, ["pprof_start", "aiperf"])
        self.collector.collect_artifacts.assert_called_once()
        self.assertIs(
            self.collector.collect_artifacts.call_args.kwargs["pprof_handle"],
            self.collector.start_pprof_collection.return_value,
        )

    def test_aiperf_failure_abandons_pprof_handle(self):
        import subprocess

        self.runner.run.side_effect = subprocess.CalledProcessError(1, "aiperf")

        orch = ab_test.ABTestOrchestrator.__new__(ab_test.ABTestOrchestrator)
        orch.scenario = self.scenario
        orch.k8s = self.k8s
        orch.runner = self.runner
        orch.collector = self.collector
        orch.router_config_a_path = mock.MagicMock()
        orch.router_config_b_path = mock.MagicMock()
        orch.output_dir = mock.MagicMock()

        result = orch.run_single_config("a.yaml", "config_a")

        self.assertEqual(result.verdict["status"], "framework_error")
        self.collector.start_pprof_collection.return_value.abandon.assert_called_once()


class MainTest(unittest.TestCase):
    def test_main_exits_non_zero_when_report_contains_regression(self):
        report = {"comparison": {"latency_avg_ms": {"regression": True}}}
        args = mock.Mock(
            scenario="scenario.yaml",
            router_config_a="config-a.yaml",
            router_config_b="config-b.yaml",
            output="./results",
            local_port=ab_test.K8sManager.DEFAULT_LOCAL_PORT,
            dry_run=False,
        )
        parser = mock.Mock()
        parser.parse_args.return_value = args

        with mock.patch.object(ab_test, "ABTestOrchestrator") as orchestrator_cls:
            orchestrator_cls.return_value.run.return_value = report
            with mock.patch.object(ab_test, "build_parser", return_value=parser):
                with self.assertRaises(SystemExit) as exit_ctx:
                    ab_test.main()

        self.assertEqual(exit_ctx.exception.code, 1)

    def test_main_exits_non_zero_when_only_router_comparison_contains_regression(self):
        # comparison (AIPerf-level) looks clean; only router_comparison
        # (e.g. request success rate) shows a regression. Before this fix
        # the exit code ignored router_comparison entirely (issue #1452).
        report = {
            "comparison": {"latency_avg_ms": {"regression": False}},
            "router_comparison": {"request_success_rate_pct": {"regression": True}},
        }
        args = mock.Mock(
            scenario="scenario.yaml",
            router_config_a="config-a.yaml",
            router_config_b="config-b.yaml",
            output="./results",
            local_port=ab_test.K8sManager.DEFAULT_LOCAL_PORT,
            dry_run=False,
        )
        parser = mock.Mock()
        parser.parse_args.return_value = args

        with mock.patch.object(ab_test, "ABTestOrchestrator") as orchestrator_cls:
            orchestrator_cls.return_value.run.return_value = report
            with mock.patch.object(ab_test, "build_parser", return_value=parser):
                with self.assertRaises(SystemExit) as exit_ctx:
                    ab_test.main()

        self.assertEqual(exit_ctx.exception.code, 1)

    def test_main_exits_zero_when_no_regression_in_either_comparison(self):
        report = {
            "comparison": {"latency_avg_ms": {"regression": False}},
            "router_comparison": {"request_success_rate_pct": {"regression": False}},
        }
        args = mock.Mock(
            scenario="scenario.yaml",
            router_config_a="config-a.yaml",
            router_config_b="config-b.yaml",
            output="./results",
            local_port=ab_test.K8sManager.DEFAULT_LOCAL_PORT,
            dry_run=False,
        )
        parser = mock.Mock()
        parser.parse_args.return_value = args

        with mock.patch.object(ab_test, "ABTestOrchestrator") as orchestrator_cls:
            orchestrator_cls.return_value.run.return_value = report
            with mock.patch.object(ab_test, "build_parser", return_value=parser):
                with self.assertRaises(SystemExit) as exit_ctx:
                    ab_test.main()

        self.assertEqual(exit_ctx.exception.code, 0)

    @mock.patch("router_ab_test.kubernetes.K8sManager")
    @mock.patch.object(ab_test, "ScenarioConfig")
    def test_dry_run_writes_yaml_to_tmp(self, mock_scenario_cls, mock_k8s_cls):
        from pathlib import Path

        mock_scenario = mock.MagicMock()
        mock_scenario.name = "smoke-test-s2-latency-vs-qps"
        mock_scenario.backends = mock.MagicMock()
        mock_scenario_cls.from_yaml.return_value = mock_scenario

        mock_k8s = mock.MagicMock()
        mock_k8s.build_backends_yaml.return_value = "apiVersion: v1\nkind: Pod\n"
        mock_k8s_cls.return_value = mock_k8s

        args = mock.Mock(
            scenario="scenarios/smoke-test-s2.yaml",
            dry_run=True,
        )
        parser = mock.Mock()
        parser.parse_args.return_value = args

        out_path = Path(f"/tmp/kthena-scenario-{mock_scenario.name}.yaml")
        out_path.unlink(missing_ok=True)

        try:
            with mock.patch.object(ab_test, "build_parser", return_value=parser):
                ab_test.main()

            self.assertTrue(out_path.exists())
            self.assertEqual(out_path.read_text(), "apiVersion: v1\nkind: Pod\n")
        finally:
            out_path.unlink(missing_ok=True)

class ComputeRunVerdictTest(unittest.TestCase):
    def test_valid_when_no_restarts_and_no_bad_states(self):
        stats = {
            "total_restarts": 0,
            "pods": [
                {"name": "mocker-llm-a", "restarts": 0, "last_reason": None, "waiting_reason": None},
                {"name": "mocker-llm-b", "restarts": 0, "last_reason": "Completed", "waiting_reason": None},
            ],
        }
        verdict = ab_test.compute_run_verdict(stats)
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)
        self.assertEqual(verdict["reasons"], [])
        self.assertEqual(verdict["offenders"], [])
        self.assertEqual(verdict["restart_stats"], stats)

    def test_invalid_when_pod_restarted(self):
        stats = {
            "total_restarts": 2,
            "pods": [
                {"name": "mocker-llm-a", "restarts": 2, "last_reason": "Error", "waiting_reason": None},
            ],
        }
        verdict = ab_test.compute_run_verdict(stats)
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertEqual(len(verdict["offenders"]), 1)
        offender = verdict["offenders"][0]
        self.assertEqual(offender["name"], "mocker-llm-a")
        self.assertIn("restartCount=2", offender["reasons"])
        self.assertIn("lastState.terminated.reason=Error", offender["reasons"])

    def test_invalid_when_oomkilled(self):
        stats = {
            "total_restarts": 1,
            "pods": [
                {"name": "mocker-llm-a", "restarts": 1, "last_reason": "OOMKilled", "waiting_reason": None},
            ],
        }
        verdict = ab_test.compute_run_verdict(stats)
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("OOMKilled" in r for r in verdict["reasons"]))

    def test_invalid_when_crash_loop_backoff(self):
        stats = {
            "total_restarts": 5,
            "pods": [
                {"name": "mocker-llm-a", "restarts": 5, "last_reason": "Error", "waiting_reason": "CrashLoopBackOff"},
            ],
        }
        verdict = ab_test.compute_run_verdict(stats)
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("CrashLoopBackOff" in r for r in verdict["reasons"]))

    def test_empty_pod_list_is_valid(self):
        verdict = ab_test.compute_run_verdict({"total_restarts": 0, "pods": []})
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)


class ApplyRequestLevelVerdictTest(unittest.TestCase):
    def _valid_verdict(self):
        return ab_test.compute_run_verdict({"total_restarts": 0, "pods": []})

    def test_stays_valid_when_503s_fully_explained_by_cancellations(self):
        # s2 rate=3 stability rerun shape: 0 503s, 0 genuine errors.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 2, "total_503": 0, "p50_ms": 1744.1, "p95_ms": 2424.4},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)
        self.assertEqual(verdict["reasons"], [])
        self.assertEqual(verdict["warnings"], [])

    def test_valid_when_503s_within_cancelled_count(self):
        # s3 concurrency=5 shape: some 503s, all covered by cancellations.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 5, "total_503": 5, "p50_ms": 1750.0, "p95_ms": 2430.0},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)

    def test_invalid_when_genuine_errors_present(self):
        # s2 rate=5/60s shape: genuine AIPerf errors observed.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 19, "cancelled": 45, "total_503": 64, "p50_ms": 1800.0, "p95_ms": 6750.0},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("genuine_errors=19" in o["reasons"][0] for o in verdict["offenders"]))

    def test_invalid_when_503s_exceed_cancellations(self):
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 2, "total_503": 17, "p50_ms": 1750.0, "p95_ms": 2450.0},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("unexplained_503s=15" in o["reasons"][0] for o in verdict["offenders"]))

    def test_unknown_cancelled_count_does_not_invalidate(self):
        # cancelled is None (unknown), not 0 - must not be treated as "no
        # cancellations occurred", or every unrelated 503 count would be
        # flagged as fully unexplained.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": None, "total_503": 20, "p50_ms": 1750.0, "p95_ms": 2450.0},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)

    def test_tail_latency_ratio_warns_but_does_not_invalidate(self):
        # s2 rate=5/45s random-arm outlier shape: p95/p50 ~3.2x, no errors.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 11, "total_503": 7, "p50_ms": 1800.0, "p95_ms": 5790.0},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)
        self.assertEqual(len(verdict["warnings"]), 1)
        self.assertIn("p95/p50", verdict["warnings"][0])

    def test_tail_latency_ratio_below_threshold_no_warning(self):
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 2, "total_503": 0, "p50_ms": 1744.1, "p95_ms": 2424.4},
        )
        self.assertEqual(verdict["warnings"], [])

    def test_framework_error_verdict_is_untouched(self):
        verdict = {"status": ab_test.VERDICT_FRAMEWORK_ERROR, "reasons": ["aiperf exited with code 1"], "offenders": []}
        result = ab_test.apply_request_level_verdict(
            verdict, {"genuine_errors": 5, "cancelled": 0, "total_503": 10, "p50_ms": None, "p95_ms": None},
        )
        self.assertEqual(result, verdict)

    def test_preexisting_pod_restart_invalidity_is_preserved(self):
        pod = {"name": "p", "restarts": 1, "last_reason": "Error", "waiting_reason": None}
        pod_verdict = ab_test.compute_run_verdict({"total_restarts": 1, "pods": [pod]})
        verdict = ab_test.apply_request_level_verdict(
            pod_verdict,
            {"genuine_errors": 0, "cancelled": 2, "total_503": 0, "p50_ms": 1744.1, "p95_ms": 2424.4},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertEqual(len(verdict["offenders"]), 1)
        self.assertEqual(verdict["offenders"][0]["name"], "p")

    def test_invalid_when_success_rate_below_floor(self):
        # smoke-test-s6 least-request shape: 503s fully covered by
        # cancellations, 0 genuine errors, but success rate (81.48%) is
        # still well below the floor because the whole latency distribution
        # is uniformly slow (p50=19.8s) rather than a fast median with a
        # stretched tail - the ratio check alone would miss this.
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {
                "genuine_errors": 0, "cancelled": 12, "total_503": 10,
                "p50_ms": 19767.4, "p95_ms": 28976.7, "success_rate_pct": 81.48,
            },
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("success_rate_pct=81.48" in o["reasons"][0] for o in verdict["offenders"]))

    def test_valid_when_success_rate_at_floor(self):
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {
                "genuine_errors": 0, "cancelled": 2, "total_503": 0,
                "p50_ms": 1744.1, "p95_ms": 2424.4, "success_rate_pct": 90.0,
            },
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)

    def test_unknown_success_rate_does_not_invalidate(self):
        verdict = ab_test.apply_request_level_verdict(
            self._valid_verdict(),
            {"genuine_errors": 0, "cancelled": 2, "total_503": 0, "p50_ms": 1744.1, "p95_ms": 2424.4},
        )
        self.assertEqual(verdict["status"], ab_test.VERDICT_VALID)


class ReporterVerdictTest(unittest.TestCase):
    def make_result(self, verdict_status=None, **metrics):
        result = ab_test.BenchmarkResult(
            config_name="config",
            scenario="scenario",
            timestamp="2026-07-18T00:00:00",
            metrics=metrics,
            raw_output="",
        )
        if verdict_status is not None:
            result.verdict = {"status": verdict_status, "reasons": [], "offenders": [], "restart_stats": {}}
        return result

    def test_compare_skipped_when_config_a_invalid(self):
        result_a = self.make_result(verdict_status=ab_test.VERDICT_INVALID, ttft_avg_ms=100.0)
        result_b = self.make_result(verdict_status=ab_test.VERDICT_VALID, ttft_avg_ms=80.0)
        comparison = ab_test.ResultReporter().compare(result_a, result_b)
        self.assertTrue(comparison["_skipped"])
        self.assertIn("invalid", comparison["reason"])

    def test_compare_skipped_when_framework_error(self):
        result_a = self.make_result(verdict_status=ab_test.VERDICT_FRAMEWORK_ERROR)
        result_b = self.make_result(verdict_status=ab_test.VERDICT_VALID, ttft_avg_ms=80.0)
        comparison = ab_test.ResultReporter().compare(result_a, result_b)
        self.assertTrue(comparison["_skipped"])
        self.assertIn("framework_error", comparison["reason"])

    def test_compare_runs_when_both_valid(self):
        result_a = self.make_result(verdict_status=ab_test.VERDICT_VALID, ttft_avg_ms=100.0, throughput_rps=50.0)
        result_b = self.make_result(verdict_status=ab_test.VERDICT_VALID, ttft_avg_ms=80.0, throughput_rps=60.0)
        comparison = ab_test.ResultReporter().compare(result_a, result_b)
        self.assertNotIn("_skipped", comparison)
        self.assertEqual(comparison["ttft_avg_ms"]["delta_pct"], 20.0)

    def test_compare_runs_when_verdict_unset(self):
        # Legacy BenchmarkResult without verdict should still compare
        result_a = self.make_result(ttft_avg_ms=100.0)
        result_b = self.make_result(ttft_avg_ms=80.0)
        comparison = ab_test.ResultReporter().compare(result_a, result_b)
        self.assertNotIn("_skipped", comparison)

    def test_build_report_includes_verdicts(self):
        result_a = self.make_result(verdict_status=ab_test.VERDICT_VALID, ttft_avg_ms=100.0)
        result_b = self.make_result(verdict_status=ab_test.VERDICT_INVALID, ttft_avg_ms=80.0)
        report = ab_test.ResultReporter().build_report(
            scenario_name="s",
            description="d",
            config_a_path="a.yaml",
            config_b_path="b.yaml",
            result_a=result_a,
            result_b=result_b,
        )
        self.assertEqual(report["config_a"]["verdict"]["status"], ab_test.VERDICT_VALID)
        self.assertEqual(report["config_b"]["verdict"]["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(report["comparison"]["_skipped"])

    def test_build_report_downgrades_verdict_on_genuine_aiperf_errors(self):
        output_dir = Path(tempfile.mkdtemp())
        # config_a: low-success fixture (its genuine errors invalidate it
        # regardless). config_b: high-success fixture, to legitimately
        # demonstrate that cancellation-covered 503s alone don't invalidate.
        prom_path_a = output_dir / "router_metrics_a.prom"
        prom_path_a.write_text(_PROM_FIXTURE, encoding="utf-8")
        prom_path_b = output_dir / "router_metrics_b.prom"
        prom_path_b.write_text(_PROM_FIXTURE_SAFE, encoding="utf-8")

        result_a = ab_test.BenchmarkResult(
            config_name="config_a", scenario="s", timestamp="",
            metrics={"aiperf_genuine_errors": 19, "aiperf_cancelled": 5}, raw_output="",
            artifacts={"prometheus": {"path": str(prom_path_a)}},
            verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
        )
        result_b = ab_test.BenchmarkResult(
            config_name="config_b", scenario="s", timestamp="",
            metrics={"aiperf_genuine_errors": 0, "aiperf_cancelled": 4}, raw_output="",
            artifacts={"prometheus": {"path": str(prom_path_b)}},
            verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
        )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s", description="d",
            config_a_path="a.yaml", config_b_path="b.yaml",
            result_a=result_a, result_b=result_b,
        )

        # config_a's 20 genuine errors invalidate it regardless of cancelled
        # count; config_b's 4 503s are fully covered by its 4 cancellations
        # and its 96% success rate clears the floor, so it stays valid.
        self.assertEqual(report["config_a"]["verdict"]["status"], ab_test.VERDICT_INVALID)
        self.assertEqual(report["config_b"]["verdict"]["status"], ab_test.VERDICT_VALID)
        self.assertTrue(report["comparison"]["_skipped"])
        self.assertEqual(report["router_comparison"], {})

    def test_build_report_stays_valid_when_cancelled_data_present_and_sufficient(self):
        output_dir = Path(tempfile.mkdtemp())
        prom_path = output_dir / "router_metrics.prom"
        prom_path.write_text(_PROM_FIXTURE_SAFE, encoding="utf-8")

        def make_clean_result(config_name):
            return ab_test.BenchmarkResult(
                config_name=config_name, scenario="s", timestamp="",
                metrics={"aiperf_genuine_errors": 0, "aiperf_cancelled": 4}, raw_output="",
                artifacts={"prometheus": {"path": str(prom_path)}},
                verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
            )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s", description="d",
            config_a_path="a.yaml", config_b_path="b.yaml",
            result_a=make_clean_result("config_a"), result_b=make_clean_result("config_b"),
        )

        self.assertEqual(report["config_a"]["verdict"]["status"], ab_test.VERDICT_VALID)
        self.assertEqual(report["config_b"]["verdict"]["status"], ab_test.VERDICT_VALID)
        self.assertNotIn("_skipped", report["comparison"] if report["comparison"] else {})
        self.assertIn("request_success_rate_pct", report["router_comparison"])

    def test_build_report_invalid_when_success_rate_below_floor_despite_covered_503s(self):
        # Documents the new floor's purpose: _PROM_FIXTURE's 503s are fully
        # covered by cancellations and there are no genuine errors, but its
        # 80% success rate is still below SUCCESS_RATE_FLOOR_PCT (90.0).
        output_dir = Path(tempfile.mkdtemp())
        prom_path = output_dir / "router_metrics.prom"
        prom_path.write_text(_PROM_FIXTURE, encoding="utf-8")

        def make_result(config_name):
            return ab_test.BenchmarkResult(
                config_name=config_name, scenario="s", timestamp="",
                metrics={"aiperf_genuine_errors": 0, "aiperf_cancelled": 20}, raw_output="",
                artifacts={"prometheus": {"path": str(prom_path)}},
                verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
            )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s", description="d",
            config_a_path="a.yaml", config_b_path="b.yaml",
            result_a=make_result("config_a"), result_b=make_result("config_b"),
        )

        self.assertEqual(report["config_a"]["verdict"]["status"], ab_test.VERDICT_INVALID)
        self.assertTrue(any("below the 90.0% floor" in r for r in report["config_a"]["verdict"]["reasons"]))


class K8sManagerRestartStatsTest(unittest.TestCase):
    def _pod(self, name, restart_count, last_reason=None, waiting_reason=None):
        container_status: dict = {
            "restartCount": restart_count,
            "lastState": {},
            "state": {},
        }
        if last_reason:
            container_status["lastState"] = {"terminated": {"reason": last_reason}}
        if waiting_reason:
            container_status["state"] = {"waiting": {"reason": waiting_reason}}
        return {
            "metadata": {"name": name},
            "status": {"containerStatuses": [container_status]},
        }

    def test_parses_restart_counts_and_reasons(self):
        import json as _json
        from router_ab_test.kubernetes import K8sManager

        payload = {
            "items": [
                self._pod("mocker-llm-a", 0),
                self._pod("mocker-llm-b", 2, last_reason="OOMKilled"),
                self._pod("mocker-llm-c", 5, last_reason="Error", waiting_reason="CrashLoopBackOff"),
            ]
        }
        fake_result = mock.Mock(stdout=_json.dumps(payload), returncode=0)
        k8s = K8sManager()
        with mock.patch("subprocess.run", return_value=fake_result):
            stats = k8s.get_mocker_pod_restart_stats()

        self.assertEqual(stats["total_restarts"], 7)
        pods_by_name = {p["name"]: p for p in stats["pods"]}
        self.assertEqual(pods_by_name["mocker-llm-a"]["restarts"], 0)
        self.assertEqual(pods_by_name["mocker-llm-b"]["last_reason"], "OOMKilled")
        self.assertEqual(pods_by_name["mocker-llm-c"]["waiting_reason"], "CrashLoopBackOff")

    def test_empty_cluster_returns_zero(self):
        import json as _json
        from router_ab_test.kubernetes import K8sManager

        fake_result = mock.Mock(stdout=_json.dumps({"items": []}), returncode=0)
        k8s = K8sManager()
        with mock.patch("subprocess.run", return_value=fake_result):
            stats = k8s.get_mocker_pod_restart_stats()
        self.assertEqual(stats["total_restarts"], 0)
        self.assertEqual(stats["pods"], [])


class TempManifestCleanupTest(unittest.TestCase):
    """Regression test for the temp-manifest leak (PR #1285 review r3619142918)."""

    def _backends_config(self):
        return ab_test.ScenarioConfig(
            name="s2",
            description="d",
            load={},
            backends={
                "common": {"engineType": "sglang", "model": "Qwen/Qwen3-0.6B"},
                "profiles": [{"name": "homogeneous", "count": 1}],
            },
        ).backends

    @staticmethod
    def _spy_named_tempfile(created: list[str]):
        real_named_tempfile = tempfile.NamedTemporaryFile

        def spy(*args, **kwargs):
            tmp = real_named_tempfile(*args, **kwargs)
            created.append(tmp.name)
            return tmp

        return mock.patch("tempfile.NamedTemporaryFile", side_effect=spy)

    def test_deploy_backends_deletes_temp_manifests(self):
        from router_ab_test.kubernetes import K8sManager

        k8s = K8sManager()
        created: list[str] = []
        with self._spy_named_tempfile(created):
            with mock.patch.object(k8s, "_apply"):
                with mock.patch.object(k8s, "_wait_for_deployment_ready"):
                    k8s.deploy_backends(self._backends_config())

        # One manifest for the mocker backends, one for the model CRDs.
        self.assertEqual(len(created), 2)
        for path in created:
            self.assertFalse(Path(path).exists(), f"temp manifest leaked: {path}")

    def test_deploy_backends_deletes_temp_manifest_when_apply_fails(self):
        import subprocess

        from router_ab_test.kubernetes import K8sManager

        k8s = K8sManager()
        created: list[str] = []
        with self._spy_named_tempfile(created):
            with mock.patch.object(
                k8s, "_apply", side_effect=subprocess.CalledProcessError(1, "kubectl")
            ):
                with self.assertRaises(subprocess.CalledProcessError):
                    k8s.deploy_backends(self._backends_config())

        # The CRD manifest is never reached, but the backends manifest must
        # still be removed by the finally block.
        self.assertEqual(len(created), 1)
        self.assertFalse(Path(created[0]).exists(), f"temp manifest leaked: {created[0]}")


_PROM_FIXTURE = """\
# HELP kthena_router_requests_total Total number of HTTP requests processed by the router
# TYPE kthena_router_requests_total counter
kthena_router_requests_total{error_type="successful_request",model="m",path="/v1/chat/completions",status_code="200"} 80
kthena_router_requests_total{error_type="proxy",model="m",path="/v1/chat/completions",status_code="503"} 20
# TYPE kthena_router_request_duration_seconds histogram
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="0.5"} 40
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="1"} 60
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="2.5"} 80
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="+Inf"} 80
kthena_router_request_duration_seconds_sum{model="m",path="/v1/chat/completions",status_code="200"} 80
kthena_router_request_duration_seconds_count{model="m",path="/v1/chat/completions",status_code="200"} 80
# TYPE kthena_router_scheduler_plugin_duration_seconds histogram
kthena_router_scheduler_plugin_duration_seconds_bucket{model="m",plugin="least-latency",type="score",le="0.001"} 50
kthena_router_scheduler_plugin_duration_seconds_bucket{model="m",plugin="least-latency",type="score",le="0.005"} 90
kthena_router_scheduler_plugin_duration_seconds_bucket{model="m",plugin="least-latency",type="score",le="0.01"} 100
kthena_router_scheduler_plugin_duration_seconds_bucket{model="m",plugin="least-latency",type="score",le="+Inf"} 100
kthena_router_scheduler_plugin_duration_seconds_sum{model="m",plugin="least-latency",type="score"} 0.2
kthena_router_scheduler_plugin_duration_seconds_count{model="m",plugin="least-latency",type="score"} 100
kthena_router_tokens_total{model="m",path="/v1/chat/completions",token_type="input"} 1000
kthena_router_tokens_total{model="m",path="/v1/chat/completions",token_type="output"} 400
# TYPE kthena_router_prefix_cache_match_ratio histogram
kthena_router_prefix_cache_match_ratio_bucket{model="m",le="0.5"} 10
kthena_router_prefix_cache_match_ratio_bucket{model="m",le="1"} 20
kthena_router_prefix_cache_match_ratio_bucket{model="m",le="+Inf"} 20
kthena_router_prefix_cache_match_ratio_sum{model="m"} 10
kthena_router_prefix_cache_match_ratio_count{model="m"} 20
kthena_router_prefix_cache_entries 123
kthena_router_prefix_cache_evictions_total{model="m"} 7
kthena_router_rate_limit_exceeded_total{limit_type="user",model="m",path="/v1/chat/completions"} 3
go_goroutines 100
process_resident_memory_bytes 104857600
"""

# Minimal fixture for tests that need a verdict to legitimately stay valid:
# 96% success sits above SUCCESS_RATE_FLOOR_PCT (90.0), unlike _PROM_FIXTURE
# above (80%, intentionally low to exercise the metrics-parsing paths in
# RouterMetricsAnalysisTest, not meant to represent a healthy run).
_PROM_FIXTURE_SAFE = """\
kthena_router_requests_total{error_type="successful_request",model="m",path="/v1/chat/completions",status_code="200"} 96
kthena_router_requests_total{error_type="proxy",model="m",path="/v1/chat/completions",status_code="503"} 4
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="0.5"} 48
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="1"} 72
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="2.5"} 96
kthena_router_request_duration_seconds_bucket{model="m",path="/v1/chat/completions",status_code="200",le="+Inf"} 96
kthena_router_request_duration_seconds_sum{model="m",path="/v1/chat/completions",status_code="200"} 96
kthena_router_request_duration_seconds_count{model="m",path="/v1/chat/completions",status_code="200"} 96
"""


class RouterMetricsAnalysisTest(unittest.TestCase):
    def test_analyze_requests_and_success_rate(self):
        analysis = ab_test.analyze_router_metrics(_PROM_FIXTURE)
        requests = analysis["requests"]
        self.assertEqual(requests["total"], 100)
        self.assertEqual(requests["by_status_code"], {"200": 80, "503": 20})
        self.assertEqual(requests["success_rate_pct"], 80.0)
        self.assertEqual(requests["by_error_type"]["proxy"], 20)

    def test_analyze_request_duration_quantiles(self):
        analysis = ab_test.analyze_router_metrics(_PROM_FIXTURE)
        stats = analysis["request_duration_seconds"]["200"]
        self.assertEqual(stats["count"], 80)
        self.assertEqual(stats["avg_ms"], 1000.0)
        # p50 rank=40 hits the le=0.5 bucket boundary exactly.
        self.assertEqual(stats["p50_ms"], 500.0)
        # p90 rank=72 interpolates between le=1 (cum 60) and le=2.5 (cum 80).
        self.assertEqual(stats["p90_ms"], 1900.0)

    def test_analyze_scheduler_plugin_duration(self):
        analysis = ab_test.analyze_router_metrics(_PROM_FIXTURE)
        plugin = analysis["scheduler_plugins"]["least-latency/score"]
        self.assertEqual(plugin["count"], 100)
        self.assertEqual(plugin["avg_ms"], 2.0)
        self.assertEqual(plugin["p95_ms"], 7.5)

    def test_analyze_tokens_prefix_cache_rate_limit_and_runtime(self):
        analysis = ab_test.analyze_router_metrics(_PROM_FIXTURE)
        self.assertEqual(analysis["tokens"]["input"], 1000)
        self.assertEqual(analysis["tokens"]["output"], 400)
        self.assertEqual(analysis["tokens"]["output_per_successful_request"], 5.0)
        self.assertEqual(analysis["prefix_cache"]["match_ratio"]["avg"], 0.5)
        self.assertEqual(analysis["prefix_cache"]["entries"], 123)
        self.assertEqual(analysis["prefix_cache"]["evictions_total"], 7)
        self.assertEqual(analysis["rate_limit"]["exceeded_total"], 3)
        self.assertEqual(analysis["runtime"]["go_goroutines"], 100)
        self.assertEqual(analysis["runtime"]["process_resident_memory_bytes"], 104857600)

    def test_analyze_omits_absent_plugin_sections(self):
        analysis = ab_test.analyze_router_metrics(
            'kthena_router_requests_total{error_type="successful_request",model="m",'
            'path="/v1/chat/completions",status_code="200"} 5\n'
        )
        self.assertNotIn("prefix_cache", analysis)
        self.assertNotIn("kvcache_aware", analysis)
        self.assertNotIn("rate_limit", analysis)
        self.assertNotIn("scheduler_plugins", analysis)

    def test_compare_router_flags_success_rate_regression_in_percentage_points(self):
        analysis_a = {
            "requests": {"success_rate_pct": 90.0},
            "request_duration_seconds": {"200": {"avg_ms": 1000.0}},
            "scheduler_plugins": {"least-latency/score": {"avg_ms": 2.0}},
            "prefix_cache": {"match_ratio": {"avg": 0.5}},
        }
        analysis_b = {
            "requests": {"success_rate_pct": 80.0},
            "request_duration_seconds": {"200": {"avg_ms": 1100.0}},
            "scheduler_plugins": {"least-latency/score": {"avg_ms": 2.3}},
            "prefix_cache": {"match_ratio": {"avg": 0.6}},
        }

        comparison = ab_test.ResultReporter().compare_router(analysis_a, analysis_b)

        self.assertEqual(comparison["request_success_rate_pct"]["delta_pp"], -10.0)
        self.assertTrue(comparison["request_success_rate_pct"]["regression"])
        self.assertEqual(comparison["request_duration_avg_ms"]["delta_pct"], -10.0)
        self.assertTrue(comparison["request_duration_avg_ms"]["regression"])
        self.assertEqual(comparison["plugin_avg_ms[least-latency/score]"]["delta_pct"], -15.0)
        self.assertTrue(comparison["plugin_avg_ms[least-latency/score]"]["regression"])
        self.assertEqual(comparison["prefix_cache_match_ratio_avg"]["delta_pct"], 20.0)
        self.assertFalse(comparison["prefix_cache_match_ratio_avg"]["regression"])

    def test_compare_router_only_compares_plugins_present_in_both_runs(self):
        analysis_a = {"scheduler_plugins": {"random/score": {"avg_ms": 1.0}}}
        analysis_b = {"scheduler_plugins": {"least-latency/score": {"avg_ms": 2.0}}}

        comparison = ab_test.ResultReporter().compare_router(analysis_a, analysis_b)

        self.assertEqual(comparison, {})

    def test_build_report_attaches_router_analysis_from_prom_artifact(self):
        output_dir = Path(tempfile.mkdtemp())
        prom_path = output_dir / "router_metrics.prom"
        # Uses the high-success fixture (not _PROM_FIXTURE) so the run stays
        # valid and router_comparison actually gets populated - this test is
        # about analysis attachment, not about verdict/saturation behavior.
        prom_path.write_text(_PROM_FIXTURE_SAFE, encoding="utf-8")

        def make_result():
            return ab_test.BenchmarkResult(
                config_name="config",
                scenario="scenario",
                timestamp="2026-07-30T00:00:00",
                metrics={"aiperf_genuine_errors": 0, "aiperf_cancelled": 4},
                raw_output="",
                artifacts={"prometheus": {"path": str(prom_path)}},
                verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
            )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s",
            description="d",
            config_a_path="a.yaml",
            config_b_path="b.yaml",
            result_a=make_result(),
            result_b=make_result(),
        )

        self.assertEqual(report["config_a"]["router_analysis"]["requests"]["total"], 100)
        self.assertEqual(report["config_b"]["router_analysis"]["requests"]["total"], 100)
        self.assertIn("request_success_rate_pct", report["router_comparison"])

    def test_build_report_router_analysis_none_without_prom_artifact(self):
        result = ab_test.BenchmarkResult(
            config_name="config",
            scenario="scenario",
            timestamp="2026-07-30T00:00:00",
            metrics={},
            raw_output="",
            artifacts={"prometheus": {"path": "/nonexistent/router_metrics.prom"}},
        )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s",
            description="d",
            config_a_path="a.yaml",
            config_b_path="b.yaml",
            result_a=result,
            result_b=result,
        )

        self.assertIsNone(report["config_a"]["router_analysis"])
        self.assertEqual(report["router_comparison"], {})

    def test_router_comparison_skipped_when_run_invalid(self):
        output_dir = Path(tempfile.mkdtemp())
        prom_path = output_dir / "router_metrics.prom"
        prom_path.write_text(_PROM_FIXTURE, encoding="utf-8")
        artifacts = {"prometheus": {"path": str(prom_path)}}
        result_a = ab_test.BenchmarkResult(
            config_name="config_a", scenario="s", timestamp="",
            metrics={}, raw_output="", artifacts=artifacts,
            verdict={"status": ab_test.VERDICT_INVALID, "reasons": [], "offenders": [], "restart_stats": {}},
        )
        result_b = ab_test.BenchmarkResult(
            config_name="config_b", scenario="s", timestamp="",
            metrics={}, raw_output="", artifacts=artifacts,
            verdict={"status": ab_test.VERDICT_VALID, "reasons": [], "offenders": [], "restart_stats": {}},
        )

        report = ab_test.ResultReporter().build_report(
            scenario_name="s", description="d",
            config_a_path="a.yaml", config_b_path="b.yaml",
            result_a=result_a, result_b=result_b,
        )

        self.assertEqual(report["router_comparison"], {})


if __name__ == "__main__":
    unittest.main()
