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

import os
from dataclasses import dataclass

DEFAULT_VLLM_ZMQ_ENDPOINT = "tcp://localhost:5557"
DEFAULT_VLLM_ZMQ_TOPIC_FILTER = "kv-events"


@dataclass(frozen=True)
class VLLMConfig:
    pod_identifier: str
    model_name: str
    zmq_endpoint: str = DEFAULT_VLLM_ZMQ_ENDPOINT
    zmq_topic_filter: str = DEFAULT_VLLM_ZMQ_TOPIC_FILTER
    zmq_retry_interval: float = 5.0
    zmq_poll_timeout: int = 250
    zmq_max_retries: int = -1


def get_vllm_config(pod_identifier: str, model_name: str) -> VLLMConfig:
    return VLLMConfig(
        pod_identifier=pod_identifier,
        model_name=model_name,
        zmq_endpoint=os.getenv("VLLM_ZMQ_ENDPOINT", DEFAULT_VLLM_ZMQ_ENDPOINT),
        zmq_topic_filter=os.getenv("VLLM_ZMQ_TOPIC_FILTER", DEFAULT_VLLM_ZMQ_TOPIC_FILTER),
    )
