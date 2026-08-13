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

import msgspec
import pytest

from kthena.runtime import zmq_subscriber
from kthena.runtime.zmq_subscriber import (
    AllBlocksCleared,
    BlockRemoved,
    BlockStored,
    VLLMZMQSubscriber,
)


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


# ---------------------------------------------------------------------------
# KV-event wire-format compatibility.
#
# The subscriber must decode both the current vLLM map-shaped events and the
# legacy positional-array events still emitted by older vLLM and by
# llm-d-inference-sim (use-vllm-map-event-format=false, its default). Each
# fixture is built as raw msgpack so it is the wire itself, not a re-encoding of
# the runtime's own structs.
# ---------------------------------------------------------------------------


def _decode(*events):
    payload = msgspec.msgpack.encode([1.0, list(events), None])
    sub = VLLMZMQSubscriber(pod_identifier="pod-a", model_name="m")
    return sub._decode_batch(payload).events


def test_decodes_current_map_block_stored():
    (event,) = _decode({"type": "BlockStored", "block_hashes": [10, 11],
                        "token_ids": [1, 2, 3, 4], "parent_block_hash": None,
                        "block_size": 2, "lora_id": None})
    assert isinstance(event, BlockStored)
    assert event.block_hashes == [10, 11]
    assert event.token_ids == [1, 2, 3, 4]


def test_decodes_current_map_block_removed():
    (event,) = _decode({"type": "BlockRemoved", "block_hashes": [7]})
    assert isinstance(event, BlockRemoved)
    assert event.block_hashes == [7]


def test_decodes_current_map_all_blocks_cleared():
    (event,) = _decode({"type": "AllBlocksCleared"})
    assert isinstance(event, AllBlocksCleared)


def test_decodes_legacy_array_block_stored():
    # [tag, block_hashes, parent_block_hash, token_ids, block_size, lora_id]
    (event,) = _decode(["BlockStored", [10, 11], None, [1, 2, 3, 4], 2, None])
    assert isinstance(event, BlockStored)
    assert event.block_hashes == [10, 11]
    assert event.token_ids == [1, 2, 3, 4]


def test_decodes_legacy_array_block_removed():
    (event,) = _decode(["BlockRemoved", [7]])
    assert isinstance(event, BlockRemoved)
    assert event.block_hashes == [7]


def test_decodes_legacy_array_all_blocks_cleared():
    (event,) = _decode(["AllBlocksCleared"])
    assert isinstance(event, AllBlocksCleared)


def test_decodes_simulator_array_with_trailing_medium():
    # Exact llm-d-inference-sim BlockStored wire: a zero parent hash when there
    # is no parent, and a trailing omitempty `medium` the runtime does not
    # declare. It must decode with the hashes preserved and `medium` ignored.
    (event,) = _decode(["BlockStored", [10, 11], 0, [1, 2, 3, 4], 2, 0, "GPU"])
    assert isinstance(event, BlockStored)
    assert event.block_hashes == [10, 11]
    assert event.token_ids == [1, 2, 3, 4]
    assert event.parent_block_hash == 0


def test_rejects_map_missing_block_hashes():
    with pytest.raises(msgspec.ValidationError):
        _decode({"type": "BlockStored", "token_ids": [1, 2]})


def test_rejects_map_missing_token_ids():
    with pytest.raises(msgspec.ValidationError):
        _decode({"type": "BlockStored", "block_hashes": [7]})
