# Copyright The Volcano Authors
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

import pytest

from kthena.runtime.collect import MetricAdapter
from kthena.runtime.standard import MetricStandard, UnsupportedEngineError

SGLANG_PROMETHEUS_METRICS = """
# HELP sglang:cache_hit_rate The cache hit rate
# TYPE sglang:cache_hit_rate gauge
sglang:cache_hit_rate{model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.007507552643049313
# HELP sglang:time_to_first_token_seconds Histogram of time to first token in seconds.
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_sum{model_name="meta-llama/Llama-3.1-8B-Instruct"} 2.3518979474117756e+06
sglang:time_to_first_token_seconds_bucket{le="0.001",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.0
sglang:time_to_first_token_seconds_bucket{le="0.005",model_name="meta-llama/Llama-3.1-8B-Instruct"} 0.0
sglang:time_to_first_token_seconds_bucket{le="0.08",model_name="meta-llama/Llama-3.1-8B-Instruct"} 6.0
sglang:time_to_first_token_seconds_bucket{le="+Inf",model_name="meta-llama/Llama-3.1-8B-Instruct"} 11008.0
sglang:time_to_first_token_seconds_count{model_name="meta-llama/Llama-3.1-8B-Instruct"} 11008.0
""".strip()


def test_build_operators_dict_with_valid_engine():
    engine_name = "sglang"
    metric_standard1 = MetricStandard(engine_name)
    assert isinstance(metric_standard1.metric_operators_dict, dict)

    engine_name = "sglang"
    metric_standard2 = MetricStandard(engine_name)
    assert (
        metric_standard1.metric_operators_dict == metric_standard2.metric_operators_dict
    )


def test_build_operators_dict_with_invalid_engine():
    invalid_engine_name = "invalid_engine"

    with pytest.raises(
        UnsupportedEngineError,
        match=r"Unsupported engine: invalid_engine.*Supported engine: vllm, sglang"
              r"|Supported engine: \['vllm', 'sglang'\]",
    ):
        MetricStandard(invalid_engine_name)


@pytest.fixture
def mock_metric_standard():
    # Assuming MetricStandard requires an engine parameter
    return MetricStandard("sglang")


def test_metric_adapter_initialization_with_valid_input():
    standard = MetricStandard("sglang")
    adapter = MetricAdapter(SGLANG_PROMETHEUS_METRICS, standard)
    assert len(adapter.metrics) == 3


def test_metric_adapter_initialization_with_invalid_metric_text():
    invalid_metric_text = """
    # HELP invalid_metric Invalid metric format
    INVALID_TYPE invalid_metric 1
    """
    standard = MetricStandard("sglang")

    with pytest.raises(ValueError):
        MetricAdapter(invalid_metric_text, standard)


def test_metric_adapter_initialization_with_exception_in_standard():
    class FailingMetricStandard(MetricStandard):
        def process(self, origin_metric):
            raise RuntimeError("Processing error")

    standard = FailingMetricStandard("sglang")

    with pytest.raises(RuntimeError):
        MetricAdapter(SGLANG_PROMETHEUS_METRICS, standard)


def test_metric_adapter_handles_empty_metrics():
    empty_metric_text = ""
    standard = MetricStandard("sglang")

    adapter = MetricAdapter(empty_metric_text, standard)
    assert len(adapter.metrics) == 0
VLLM_COUNTER_METRICS = """
# HELP vllm:generation_tokens_total Number of generation tokens processed.
# TYPE vllm:generation_tokens_total counter
vllm:generation_tokens_total{model_name="m"} 42.0
""".strip()

SGLANG_COUNTER_METRICS = """
# HELP sglang:generation_tokens_total Number of generation tokens processed.
# TYPE sglang:generation_tokens_total counter
sglang:generation_tokens_total{model_name="m"} 7.0
""".strip()


def _standardized(adapter, name):
    return [m for m in adapter.metrics if m.name == name]


def test_vllm_counter_rename_survives_parser_family_munging():
    # The parser strips _total from a counter FAMILY name, so the rule must
    # match the munged family and the samples must come back out with _total.
    standard = MetricStandard("vllm")
    adapter = MetricAdapter(VLLM_COUNTER_METRICS, standard)

    twins = _standardized(adapter, "kthena:generation_tokens")
    assert len(twins) == 1, [m.name for m in adapter.metrics]
    twin = twins[0]
    assert twin.type == "counter"
    sample_names = [s.name for s in twin.samples]
    assert "kthena:generation_tokens_total" in sample_names
    total = next(s for s in twin.samples if s.name == "kthena:generation_tokens_total")
    assert total.value == 42.0
    assert total.labels == {"model_name": "m"}


def test_sglang_counter_rename_survives_parser_family_munging():
    standard = MetricStandard("sglang")
    adapter = MetricAdapter(SGLANG_COUNTER_METRICS, standard)

    twins = _standardized(adapter, "kthena:generation_tokens")
    assert len(twins) == 1, [m.name for m in adapter.metrics]
    sample_names = [s.name for s in twins[0].samples]
    assert "kthena:generation_tokens_total" in sample_names


def test_process_metrics_exposes_documented_counter_series():
    import asyncio

    from kthena.runtime.collect import process_metrics

    standard = MetricStandard("vllm")
    output = asyncio.run(process_metrics(VLLM_COUNTER_METRICS, standard))
    assert b'kthena:generation_tokens_total{model_name="m"} 42.0' in output
