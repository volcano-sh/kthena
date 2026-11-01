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

import asyncio
from datetime import datetime
from unittest.mock import AsyncMock

import pytest

from kthena.runtime.events import EventType
from kthena.runtime.tensorrtllm_kv_events import (
    TensorRTLLMKVEventPoller,
    _token_ids_from_block,
)


def test_token_ids_from_block():
    block = {
        "tokens": [
            {"type": "unique_token", "token_id": 1, "token_extra_id": 0},
            {"type": "unique_token", "token_id": 2, "token_extra_id": 0},
        ]
    }
    assert _token_ids_from_block(block) == [1, 2]


@pytest.mark.asyncio
async def test_publish_stored_events():
    poller = TensorRTLLMKVEventPoller(
        engine_base_url="http://127.0.0.1:9",
        pod_identifier="pod-a",
        model_name="TinyLlama-1.1B-Chat-v1.0",
        client=AsyncMock(),
    )
    published = []

    async def fake_publish(event):
        published.append(event)

    poller.event_publisher = AsyncMock()
    poller.event_publisher.publish = fake_publish

    sample = {
        "type": "stored",
        "parent_hash": 11,
        "blocks": [
            {
                "type": "stored_block",
                "block_hash": 101,
                "tokens": [{"token_id": 7}, {"token_id": 8}],
            },
            {
                "type": "stored_block",
                "block_hash": 102,
                "tokens": [{"token_id": 9}],
            },
        ],
    }
    await poller._publish_stored(sample, datetime.now())
    assert len(published) == 2
    assert published[0].event_type == EventType.VLLM_BLOCK_STORED
    assert published[0].vllm_event.block_hashes == [101]
    assert published[0].vllm_event.token_ids == [7, 8]
    assert published[0].vllm_event.parent_block_hash == 11
    assert published[1].vllm_event.block_hashes == [102]
    assert published[1].vllm_event.parent_block_hash is None


@pytest.mark.asyncio
async def test_handle_events_wraps_created_and_stored():
    """Matches trtllm-serve POST /kv_cache_events envelope."""
    poller = TensorRTLLMKVEventPoller(
        engine_base_url="http://127.0.0.1:9",
        pod_identifier="pod-a",
        model_name="TinyLlama-1.1B-Chat-v1.0",
        client=AsyncMock(),
    )
    published = []

    async def fake_publish(event):
        published.append(event)

    poller.event_publisher = AsyncMock()
    poller.event_publisher.publish = fake_publish

    envelope = [
        {
            "event_id": 0,
            "data": {"type": "created", "num_blocks_per_cache_level": [1, 0]},
            "window_size": 2049,
        },
        {
            "event_id": 1,
            "data": {
                "type": "stored",
                "parent_hash": None,
                "blocks": [
                    {
                        "type": "stored_block",
                        "block_hash": 4565548111910448042,
                        "tokens": [
                            {"type": "unique_token", "token_id": i, "token_extra_id": 0}
                            for i in range(32)
                        ],
                    }
                ],
            },
            "window_size": 2049,
        },
    ]
    await poller._handle_events(envelope)
    assert len(published) == 1
    assert published[0].vllm_event.block_hashes == [4565548111910448042]
    assert len(published[0].vllm_event.token_ids) == 32
