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

import logging
from typing import Iterable, List

from prometheus_client import generate_latest
from prometheus_client.parser import text_string_to_metric_families
from prometheus_client.core import Metric
from prometheus_client.registry import Collector, CollectorRegistry

from kthena.runtime.standard import MetricStandard

logger = logging.getLogger(__name__)


class MetricAdapter(Collector):
    def __init__(self, origin_metric_text: str, standard: MetricStandard):
        self.metrics: List[Metric] = self._parse_and_process_metrics(origin_metric_text, standard)

    def _parse_and_process_metrics(self, origin_metric_text: str, standard: MetricStandard) -> List[Metric]:
        metrics = []
        # An engine can expose one measurement under two names: some vLLM
        # versions have both the old and the new inter-token latency histogram.
        # Each standard name is emitted once, from the first source family
        # seen, because two families of one name are not valid exposition.
        standardized_names = set()

        if not origin_metric_text.strip():
            logger.warning("Empty metric text provided")
            return metrics

        try:
            for origin_metric in text_string_to_metric_families(origin_metric_text):
                metrics.append(origin_metric)

                processed_metric = standard.process(origin_metric)
                if processed_metric is None:
                    continue
                if processed_metric.name in standardized_names:
                    continue
                standardized_names.add(processed_metric.name)
                metrics.append(processed_metric)

        except ValueError as e:
            logger.error("Invalid metric text format: %s", e)
            raise ValueError(f"Failed to parse metric text: {e}") from e
        except Exception as e:
            logger.error("Unexpected error processing metrics: %s", e)
            raise RuntimeError(f"Failed to initialize MetricAdapter: {e}")from e

        return metrics

    def collect(self) -> Iterable[Metric]:
        yield from self.metrics


async def process_metrics(origin_metric_text: str, standard: MetricStandard) -> bytes:
    if not isinstance(origin_metric_text, str):
        raise TypeError("Metric text must be a string")

    if not isinstance(standard, MetricStandard):
        raise TypeError("Standard must be a MetricStandard instance")

    registry = CollectorRegistry()

    try:
        adapter = MetricAdapter(origin_metric_text, standard)
        registry.register(adapter)
        return generate_latest(registry)

    except (ValueError, RuntimeError):
        raise
    except Exception as e:
        logger.error("Unexpected error in process_metrics: %s",e)
        raise RuntimeError(f"Failed to process metrics: {e}")from e
