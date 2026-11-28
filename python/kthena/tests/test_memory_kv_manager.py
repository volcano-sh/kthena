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

import asyncio
import json
import sys

import msgspec
import pytest

from kthena.runtime import memory_kv_manager as mkm
from kthena.runtime.events import EventPublisher, EventType
from kthena.runtime.kv_cache_manager import (
    VLLMKVCacheEventHandler,
    VLLMKVCacheRedisManager,
    compute_standardized_hash,
    standardize_block_hashes,
)
from kthena.runtime.memory_kv_manager import (
    KV_EVENT_CLEARED,
    KV_EVENT_REMOVED,
    KV_EVENT_STORED,
    MemoryKVCacheManager,
    RouterStreamUnavailable,
)
from kthena.runtime.zmq_subscriber import VLLMZMQSubscriber


# Tokens 0..63 in blocks of 16. Shared with TestTokenBlockProcessor_ChainedHashGolden in Go.
GOLDEN_HASHES = [6473494984593326032, 8529154092578337085, 7004526854457690277, 6210590132355917408]


async def _read(stream_iter, count):
    """Read count decoded messages from a router stream."""
    messages = []
    for _ in range(count):
        line = await asyncio.wait_for(stream_iter.__anext__(), timeout=1)
        messages.append(json.loads(line))
    return messages


def _queued(stream):
    """Decoded messages queued on a stream, without consuming its initial
    block set."""
    messages = []
    while not stream.queue.empty():
        item = stream.queue.get_nowait()
        messages.append(None if item is None else json.loads(item))
    return messages


@pytest.mark.asyncio
async def test_standardized_hashes_golden():
    token_ids = list(range(64))
    blocks = [token_ids[i:i + 16] for i in range(0, 64, 16)]
    engine_hashes = [11, 22, 33, 44]
    assert standardize_block_hashes(engine_hashes, token_ids) == list(zip(engine_hashes, GOLDEN_HASHES))
    assert await VLLMKVCacheRedisManager.get_standardized_hashes(blocks) == GOLDEN_HASHES


@pytest.mark.asyncio
@pytest.mark.parametrize("known_parent", [True, False], ids=["known-parent", "unknown-parent"])
async def test_memory_block_stored_parent(known_parent, caplog):
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    subscriber = VLLMZMQSubscriber("pod-1", "qwen")
    subscriber.event_publisher = EventPublisher()
    subscriber.event_publisher.subscribe(EventType.VLLM_BLOCK_STORED, VLLMKVCacheEventHandler(manager))

    async def store(hashes, token_ids, parent=None):
        event = {"type": "BlockStored", "block_hashes": hashes, "parent_block_hash": parent,
                 "token_ids": token_ids, "block_size": 16}
        await subscriber._process_message(msgspec.msgpack.encode([1.0, [event], None]), "pod-1", "qwen")

    # Newer vLLM sends full byte digests. Zero is also a valid engine hash.
    b0, b1, b2 = (n.to_bytes(32, "big") for n in (111, 0, 333))
    await store([b0, b1], list(range(32)))
    _queued(stream)
    await store([b2], list(range(32, 48)), b1 if known_parent else b"missing")

    if known_parent:
        assert manager.hash_mapping[333] == GOLDEN_HASHES[2]
        (message,) = _queued(stream)
        assert message["events"][0]["block_hashes"] == [GOLDEN_HASHES[2]]
    else:
        assert 333 not in manager.hash_mapping
        assert _queued(stream) == []
        assert set(manager._blocks["qwen"]) == set(GOLDEN_HASHES[:2])
        assert "unknown parent" in caplog.text


@pytest.mark.asyncio
async def test_add_blocks_streams_standardized_hashes_to_all_routers():
    manager = MemoryKVCacheManager()
    streams = [manager.open_stream("router-a"), manager.open_stream("router-b")]

    token_ids = list(range(32))  # 2 blocks of 16
    expected_std = GOLDEN_HASHES[:2]

    ok = await manager.add_blocks("qwen", [111, 222], "pod-1.default", token_ids)
    assert ok

    for stream in streams:
        messages = _queued(stream)
        assert len(messages) == 1
        assert messages[0]["model_name"] == "qwen"
        # The router attributes events to the pod it connected to.
        assert "pod_identifier" not in messages[0]
        (event,) = messages[0]["events"]
        assert event["type"] == KV_EVENT_STORED
        assert event["block_hashes"] == expected_std
        assert event["timestamp"] > 0

    # engine -> std mapping is retained for later removals
    assert manager.hash_mapping == {111: expected_std[0], 222: expected_std[1]}


@pytest.mark.asyncio
async def test_remove_blocks_uses_engine_hash_mapping():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    token_ids = list(range(16))
    await manager.add_blocks("qwen", [111], "pod-1.default", token_ids)
    _queued(stream)

    removed = await manager.remove_blocks("qwen", [111, 999], "pod-1.default")
    assert removed == 1

    (message,) = _queued(stream)
    (event,) = message["events"]
    assert event["type"] == KV_EVENT_REMOVED
    assert event["block_hashes"] == [compute_standardized_hash(token_ids)]
    assert 111 not in manager.hash_mapping


@pytest.mark.asyncio
async def test_shared_std_hash_survives_until_last_engine_hash_removed():
    """Two engine hashes for the same prefix (e.g. different LoRA adapters)
    must retain ownership until the last engine-hash reference is removed."""
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    token_ids = list(range(16))
    std_hash = compute_standardized_hash(token_ids)

    await manager.add_blocks("qwen", [111], "pod-1.default", token_ids)
    await manager.add_blocks("qwen", [222], "pod-1.default", token_ids)
    _queued(stream)

    # Removing one reference keeps the block indexed and streams nothing.
    assert await manager.remove_blocks("qwen", [111], "pod-1.default") == 1
    assert _queued(stream) == []
    assert std_hash in manager._blocks["qwen"]

    # Removing the last reference drops the block and announces it.
    assert await manager.remove_blocks("qwen", [222], "pod-1.default") == 1
    (message,) = _queued(stream)
    assert message["events"] == [
        {"type": KV_EVENT_REMOVED, "block_hashes": [std_hash]}]
    assert std_hash not in manager._blocks["qwen"]


@pytest.mark.asyncio
async def test_remove_blocks_without_mapping_streams_nothing():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    assert await manager.remove_blocks("qwen", [12345], "pod-1.default") == 0
    assert _queued(stream) == []


@pytest.mark.asyncio
async def test_clear_all_blocks_streams_cleared_event():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    _queued(stream)

    assert await manager.clear_all_blocks("qwen", "pod-1.default") == 1
    (message,) = _queued(stream)
    assert message == {"model_name": "qwen", "events": [{"type": KV_EVENT_CLEARED}]}
    assert manager.hash_mapping == {}


@pytest.mark.asyncio
async def test_stream_starts_with_full_block_set(monkeypatch):
    monkeypatch.setattr(mkm, "MAX_BLOCKS_PER_MESSAGE", 2)
    manager = MemoryKVCacheManager()
    token_ids = list(range(48))
    await manager.add_blocks("qwen", [1, 2, 3], "pod-1.default", token_ids)
    await manager.add_blocks("llama", [4], "pod-1.default", list(range(16)))
    await manager.add_blocks("cleared", [5], "pod-1.default", list(range(16)))
    await manager.clear_all_blocks("cleared", "pod-1.default")

    stream = manager.open_stream("router-a")
    # Mutations after registering follow the block set.
    await manager.add_blocks("qwen", [6], "pod-1.default", list(range(100, 116)))
    stream_iter = manager.iter_stream(stream)
    messages = await _read(stream_iter, 4)

    blocks = {}
    for message in messages[:3]:
        (event,) = message["events"]
        assert event["type"] == KV_EVENT_STORED
        assert len(event["block_hashes"]) <= 2
        # Original per-block store times are preserved so the router's
        # engine-restart freshness filter keeps working.
        stored = manager._blocks[message["model_name"]]
        assert event["timestamps"] == [stored[h] for h in event["block_hashes"]]
        blocks.setdefault(message["model_name"], []).extend(event["block_hashes"])
    assert blocks == {
        "qwen": GOLDEN_HASHES[:3],
        "llama": [compute_standardized_hash(list(range(16)))],
    }
    assert messages[3]["model_name"] == "qwen"
    assert messages[3]["events"][0]["block_hashes"] == [
        compute_standardized_hash(list(range(100, 116)))]
    await stream_iter.aclose()


@pytest.mark.asyncio
async def test_stream_releases_initial_block_set_once_sent(monkeypatch):
    monkeypatch.setattr(mkm, "MAX_BLOCKS_PER_MESSAGE", 1)
    manager = MemoryKVCacheManager()
    await manager.add_blocks("qwen", [1, 2], "pod-1.default", list(range(32)))
    stream = manager.open_stream("router-a")
    initial_blocks = stream.initial_blocks[0][1]
    stream_iter = manager.iter_stream(stream)
    await _read(stream_iter, 2)

    # Wait for the next delta, past the initial block set.
    pending = asyncio.ensure_future(stream_iter.__anext__())
    await asyncio.sleep(0)
    assert not pending.done()
    # Only this test and getrefcount's argument refer to the copy.
    assert sys.getrefcount(initial_blocks) == 2
    pending.cancel()


@pytest.mark.asyncio
async def test_large_deltas_are_split(monkeypatch):
    monkeypatch.setattr(mkm, "MAX_BLOCKS_PER_MESSAGE", 2)
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    await manager.add_blocks("qwen", [1, 2, 3], "pod-1.default", list(range(48)))
    messages = _queued(stream)
    assert [len(m["events"][0]["block_hashes"]) for m in messages] == [2, 1]


@pytest.mark.asyncio
async def test_idle_stream_sends_heartbeats(monkeypatch):
    monkeypatch.setattr(mkm, "STREAM_HEARTBEAT_SECONDS", 0.01)
    manager = MemoryKVCacheManager()
    stream_iter = manager.iter_stream(manager.open_stream("router-a"))
    assert await asyncio.wait_for(stream_iter.__anext__(), timeout=1) == b"\n"
    await stream_iter.aclose()


@pytest.mark.asyncio
async def test_registering_again_replaces_the_stream():
    manager = MemoryKVCacheManager()
    old = manager.open_stream("router-a")
    old_iter = manager.iter_stream(old)
    new = manager.open_stream("router-a")

    with pytest.raises(StopAsyncIteration):
        await asyncio.wait_for(old_iter.__anext__(), timeout=1)
    # Ending the replaced stream keeps the new one registered.
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    assert len(_queued(new)) == 1


@pytest.mark.asyncio
async def test_ended_response_unregisters_the_stream():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    stream_iter = manager.iter_stream(stream)

    async def respond():
        await _read(stream_iter, 1)
        # E.g. the router disconnected.
        await stream_iter.aclose()

    serving = asyncio.create_task(manager.serve(stream, respond))
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    await asyncio.wait_for(serving, timeout=1)
    assert manager._streams == {}
    assert manager._live == set()


@pytest.mark.asyncio
async def test_closed_stream_ends_without_sending_pending_data(monkeypatch):
    monkeypatch.setattr(mkm, "MAX_BLOCKS_PER_MESSAGE", 1)
    manager = MemoryKVCacheManager()
    await manager.add_blocks("qwen", [1, 2], "pod-1.default", list(range(32)))
    stream = manager.open_stream("router-a")
    await manager.add_blocks("qwen", [3], "pod-1.default", list(range(32, 48)))
    stream_iter = manager.iter_stream(stream)
    await _read(stream_iter, 1)

    stream.close()
    # Neither the rest of the initial block set nor the queued delta.
    with pytest.raises(StopAsyncIteration):
        await asyncio.wait_for(stream_iter.__anext__(), timeout=1)


@pytest.mark.asyncio
async def test_response_that_does_not_end_is_cancelled(monkeypatch):
    """A response whose client does not read blocks on sending and never
    notices that its stream was closed; it must not outlive the stream nor
    escape MAX_ROUTER_STREAMS."""
    monkeypatch.setattr(mkm, "MAX_ROUTER_STREAMS", 1)
    monkeypatch.setattr(mkm, "STREAM_ABORT_GRACE_SECONDS", 0.01)
    manager = MemoryKVCacheManager()
    old = manager.open_stream("router-a")
    blocked = asyncio.Event()
    cancelled = asyncio.Event()

    async def respond():
        blocked.set()
        try:
            await asyncio.Event().wait()
        except asyncio.CancelledError:
            cancelled.set()
            raise

    serving = asyncio.create_task(manager.serve(old, respond))
    await blocked.wait()

    # The replaced stream still counts until its response has exited.
    with pytest.raises(RouterStreamUnavailable):
        manager.open_stream("router-a")
    assert old.closed
    await asyncio.wait_for(serving, timeout=1)
    assert cancelled.is_set()
    assert manager._live == set()
    new = manager.open_stream("router-a")
    assert manager._streams == {"router-a": new}


@pytest.mark.asyncio
async def test_close_cancels_responses_that_do_not_end(monkeypatch):
    monkeypatch.setattr(mkm, "STREAM_ABORT_GRACE_SECONDS", 0.01)
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    serving = asyncio.create_task(
        manager.serve(stream, lambda: asyncio.Event().wait()))
    await asyncio.sleep(0)

    await manager.close()
    await asyncio.wait_for(serving, timeout=1)
    assert manager._live == set()


@pytest.mark.asyncio
async def test_serve_propagates_response_errors():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")

    async def respond():
        raise RuntimeError("boom")

    with pytest.raises(RuntimeError, match="boom"):
        await manager.serve(stream, respond)
    assert manager._streams == {}
    assert manager._live == set()


@pytest.mark.asyncio
async def test_queue_overflow_ends_the_stream(monkeypatch):
    """A router that cannot keep up must not stall event processing or grow
    memory without bound: its stream is ended, and the router registers again
    to receive the full block set."""
    monkeypatch.setattr(mkm, "STREAM_QUEUE_MAXSIZE", 2)
    manager = MemoryKVCacheManager()
    slow = manager.open_stream("router-slow")
    fast = manager.open_stream("router-fast")

    for i in range(3):
        assert await manager.add_blocks(
            "qwen", [100 + i], "pod-1.default", list(range(16 * i, 16 * i + 16)))
        if i < 2:
            _queued(fast)

    assert _queued(slow) == [None]
    assert list(manager._streams) == ["router-fast"]
    assert len(_queued(fast)) == 1


@pytest.mark.asyncio
async def test_open_stream_limits(monkeypatch):
    monkeypatch.setattr(mkm, "MAX_ROUTER_STREAMS", 1)
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    with pytest.raises(RouterStreamUnavailable):
        manager.open_stream("router-b")

    await manager.close()
    assert _queued(stream) == [None]
    assert manager._streams == {}
    with pytest.raises(RouterStreamUnavailable):
        manager.open_stream("router-a")


@pytest.mark.asyncio
async def test_no_registered_routers_streams_nothing():
    manager = MemoryKVCacheManager()
    assert await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    assert manager._blocks["qwen"]


@pytest.mark.asyncio
async def test_add_blocks_with_mismatched_tokens_fails():
    manager = MemoryKVCacheManager()
    stream = manager.open_stream("router-a")
    # 10 tokens cannot be split evenly across 3 hashes
    ok = await manager.add_blocks("qwen", [1, 2, 3], "pod-1.default", list(range(10)))
    assert not ok
    assert _queued(stream) == []
