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


if __name__ == "__main__":
    unittest.main()
