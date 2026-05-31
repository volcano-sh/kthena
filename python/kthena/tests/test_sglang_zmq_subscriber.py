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
from unittest.mock import AsyncMock

import msgspec
import pytest

from kthena.runtime.events import (
    EventType,
    SGLangEventData,
    SGLangBlockStoredEvent,
    SGLangBlockRemovedEvent,
    SGLangAllBlocksClearedEvent,
)
from kthena.runtime.sglang_zmq_subscriber import (
    AllBlocksCleared,
    BlockRemoved,
    BlockStored,
    KVEventBatch,
    SGLangZMQSubscriber,
)


def test_sglang_event_types_exist():
    assert EventType.SGLANG_BLOCK_STORED.value == "sglang_block_stored"
    assert EventType.SGLANG_BLOCK_REMOVED.value == "sglang_block_removed"
    assert EventType.SGLANG_ALL_BLOCKS_CLEARED.value == "sglang_all_blocks_cleared"


def test_sglang_block_stored_event_has_medium_default_none():
    ev = SGLangBlockStoredEvent(block_hashes=[1, 2])
    assert ev.medium is None
    ev2 = SGLangBlockStoredEvent(block_hashes=[1], medium="GPU")
    assert ev2.medium == "GPU"


def test_sglang_block_removed_event_has_medium_default_none():
    ev = SGLangBlockRemovedEvent(block_hashes=[1, 2])
    assert ev.medium is None


def test_msgspec_roundtrip_with_medium_and_attn_dp_rank():
    batch = KVEventBatch(
        ts=123.456,
        events=[
            BlockStored(
                block_hashes=[10, 11, 12],
                parent_block_hash=9,
                token_ids=[1, 2, 3, 4],
                block_size=16,
                lora_id=None,
                medium="GPU",
            ),
            BlockRemoved(block_hashes=[10], medium="DISK"),
            AllBlocksCleared(),
        ],
        attn_dp_rank=2,
    )
    payload = msgspec.msgpack.encode(batch)
    decoded = msgspec.msgpack.Decoder(type=KVEventBatch).decode(payload)

    assert decoded.ts == 123.456
    assert decoded.attn_dp_rank == 2
    assert len(decoded.events) == 3

    stored = decoded.events[0]
    assert isinstance(stored, BlockStored)
    assert stored.block_hashes == [10, 11, 12]
    assert stored.medium == "GPU"

    removed = decoded.events[1]
    assert isinstance(removed, BlockRemoved)
    assert removed.medium == "DISK"

    assert isinstance(decoded.events[2], AllBlocksCleared)


def test_msgspec_decodes_block_stored_without_medium():
    batch = KVEventBatch(
        ts=1.0,
        events=[
            BlockStored(
                block_hashes=[1],
                parent_block_hash=None,
                token_ids=[1],
                block_size=16,
                lora_id=None,
            )
        ],
        attn_dp_rank=None,
    )
    payload = msgspec.msgpack.encode(batch)
    decoded = msgspec.msgpack.Decoder(type=KVEventBatch).decode(payload)
    assert decoded.events[0].medium is None
    assert decoded.attn_dp_rank is None


@pytest.mark.asyncio
async def test_process_event_block_stored_publishes_correct_type():
    sub = SGLangZMQSubscriber(pod_identifier="pod-a", model_name="m")
    sub.event_publisher = AsyncMock()
    event = BlockStored(
        block_hashes=[1, 2],
        parent_block_hash=None,
        token_ids=[1, 2],
        block_size=16,
        lora_id=None,
        medium="GPU",
    )
    await sub._process_event(event, timestamp=10.0,
                             pod_identifier="pod-a", model_name="m",
                             attn_dp_rank=3)

    sub.event_publisher.publish.assert_awaited_once()
    published: SGLangEventData = sub.event_publisher.publish.await_args.args[0]
    assert isinstance(published, SGLangEventData)
    assert published.event_type == EventType.SGLANG_BLOCK_STORED
    assert published.pod_identifier == "pod-a"
    assert published.model_name == "m"
    assert published.attn_dp_rank == 3
    assert isinstance(published.sglang_event, SGLangBlockStoredEvent)
    assert published.sglang_event.medium == "GPU"
    assert published.sglang_event.block_hashes == [1, 2]


@pytest.mark.asyncio
async def test_process_event_block_removed_with_medium():
    sub = SGLangZMQSubscriber(pod_identifier="pod-a", model_name="m")
    sub.event_publisher = AsyncMock()
    await sub._process_event(
        BlockRemoved(block_hashes=[7], medium="DISK"),
        timestamp=1.0, pod_identifier="pod-a", model_name="m", attn_dp_rank=0,
    )

    published = sub.event_publisher.publish.await_args.args[0]
    assert published.event_type == EventType.SGLANG_BLOCK_REMOVED
    assert isinstance(published.sglang_event, SGLangBlockRemovedEvent)
    assert published.sglang_event.medium == "DISK"


@pytest.mark.asyncio
async def test_process_event_all_blocks_cleared():
    sub = SGLangZMQSubscriber(pod_identifier="pod-a", model_name="m")
    sub.event_publisher = AsyncMock()
    await sub._process_event(
        AllBlocksCleared(),
        timestamp=1.0, pod_identifier="pod-a", model_name="m", attn_dp_rank=None,
    )

    published = sub.event_publisher.publish.await_args.args[0]
    assert published.event_type == EventType.SGLANG_ALL_BLOCKS_CLEARED
    assert isinstance(published.sglang_event, SGLangAllBlocksClearedEvent)


@pytest.mark.asyncio
async def test_process_event_unknown_type_is_dropped():
    sub = SGLangZMQSubscriber(pod_identifier="pod-a", model_name="m")
    sub.event_publisher = AsyncMock()

    class _Mystery:
        pass

    await sub._process_event(
        _Mystery(),
        timestamp=1.0, pod_identifier="pod-a", model_name="m", attn_dp_rank=None,
    )
    sub.event_publisher.publish.assert_not_awaited()


@pytest.mark.asyncio
async def test_process_message_decodes_and_dispatches():
    batch = KVEventBatch(
        ts=42.0,
        events=[
            BlockStored(
                block_hashes=[1],
                parent_block_hash=None,
                token_ids=[1],
                block_size=16,
                lora_id=None,
                medium="GPU",
            ),
            BlockRemoved(block_hashes=[2], medium=None),
        ],
        attn_dp_rank=1,
    )
    payload = msgspec.msgpack.encode(batch)

    sub = SGLangZMQSubscriber(pod_identifier="pod-x", model_name="qwen")
    sub.event_publisher = AsyncMock()
    await sub._process_message(payload, "pod-x", "qwen")

    assert sub.event_publisher.publish.await_count == 2
    types = [c.args[0].event_type for c in sub.event_publisher.publish.await_args_list]
    assert types == [EventType.SGLANG_BLOCK_STORED, EventType.SGLANG_BLOCK_REMOVED]
