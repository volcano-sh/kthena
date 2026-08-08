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
from dataclasses import replace

import pytest

from kthena.runtime import zmq_subscriber
from kthena.runtime.zmq_subscriber import VLLMZMQSubscriber


@pytest.mark.asyncio
async def test_vllm_subscriber_honors_configured_topic_filter(monkeypatch):
    sub = VLLMZMQSubscriber(pod_identifier="pod-a", model_name="m")
    sub.config = replace(sub.config, zmq_topic_filter="test-topic")
    processed_payloads = []

    class _FakeZMQ:
        def __init__(self):
            self.messages = [
                [b"wrong-topic", b"seq", b"wrong"],
                [b"test-topic", b"seq", b"right"],
            ]

        def socket(self, socket_type):
            return self

        def connect(self, endpoint):
            pass

        def setsockopt_string(self, option, value):
            pass

        def setsockopt(self, option, value):
            pass

        async def recv_multipart(self, flags):
            return self.messages.pop(0)

        def close(self):
            pass

        def term(self):
            pass

    fake_zmq = _FakeZMQ()
    monkeypatch.setattr(zmq_subscriber.zmq.asyncio, "Context", lambda: fake_zmq)

    async def process_message(payload, pod_identifier, model_name):
        processed_payloads.append(payload)
        sub.running = False

    sub._process_message = process_message
    sub.running = True

    await sub._run_subscriber()

    assert processed_payloads == [b"right"]
