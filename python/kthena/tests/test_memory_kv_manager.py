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

from unittest.mock import AsyncMock, MagicMock

import pytest

from kthena.runtime.kv_cache_manager import compute_standardized_hash
from kthena.runtime.memory_kv_manager import (
    KV_EVENT_CLEARED,
    KV_EVENT_REMOVED,
    KV_EVENT_SNAPSHOT,
    KV_EVENT_STORED,
    KV_EVENTS_PATH,
    MemoryKVCacheManager,
)
from kthena.runtime.router_registry import RouterRegistry


def _make_manager(endpoints):
    registry = RouterRegistry()
    for i, endpoint in enumerate(endpoints):
        registry.register(f"router-{i}", endpoint, ttl_seconds=60)

    client = MagicMock()
    response = MagicMock()
    response.raise_for_status = MagicMock()
    client.post = AsyncMock(return_value=response)
    return MemoryKVCacheManager(registry=registry, client=client), client


def _pushed_payloads(client):
    return [(call.args[0], call.kwargs["json"]) for call in client.post.await_args_list]


@pytest.mark.asyncio
async def test_add_blocks_pushes_standardized_hashes_to_all_routers():
    manager, client = _make_manager(
        ["http://router-a:9080", "http://router-b:9080"])

    token_ids = list(range(32))  # 2 blocks of 16
    engine_hashes = [111, 222]
    expected_std = [
        compute_standardized_hash(token_ids[0:16]),
        compute_standardized_hash(token_ids[16:32]),
    ]

    ok = await manager.add_blocks("qwen", engine_hashes, "pod-1.default", token_ids)
    assert ok

    payloads = _pushed_payloads(client)
    assert len(payloads) == 2
    urls = {url for url, _ in payloads}
    assert urls == {
        f"http://router-a:9080{KV_EVENTS_PATH}",
        f"http://router-b:9080{KV_EVENTS_PATH}",
    }
    for _, payload in payloads:
        assert payload["pod_identifier"] == "pod-1.default"
        assert payload["model_name"] == "qwen"
        assert len(payload["events"]) == 1
        event = payload["events"][0]
        assert event["type"] == KV_EVENT_STORED
        assert event["block_hashes"] == expected_std
        assert event["timestamp"] > 0

    # engine -> std mapping is retained for later removals
    assert manager.hash_mapping == {111: expected_std[0], 222: expected_std[1]}


@pytest.mark.asyncio
async def test_remove_blocks_uses_engine_hash_mapping():
    manager, client = _make_manager(["http://router-a:9080"])
    token_ids = list(range(16))
    await manager.add_blocks("qwen", [111], "pod-1.default", token_ids)
    client.post.reset_mock()

    removed = await manager.remove_blocks("qwen", [111, 999], "pod-1.default")
    assert removed == 1

    payloads = _pushed_payloads(client)
    assert len(payloads) == 1
    event = payloads[0][1]["events"][0]
    assert event["type"] == KV_EVENT_REMOVED
    assert event["block_hashes"] == [compute_standardized_hash(token_ids)]
    assert 111 not in manager.hash_mapping


@pytest.mark.asyncio
async def test_remove_blocks_without_mapping_pushes_nothing():
    manager, client = _make_manager(["http://router-a:9080"])
    removed = await manager.remove_blocks("qwen", [12345], "pod-1.default")
    assert removed == 0
    client.post.assert_not_awaited()


@pytest.mark.asyncio
async def test_clear_all_blocks_pushes_cleared_event():
    manager, client = _make_manager(["http://router-a:9080"])
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    client.post.reset_mock()

    cleared = await manager.clear_all_blocks("qwen", "pod-1.default")
    assert cleared == 1

    payloads = _pushed_payloads(client)
    assert payloads[0][1]["events"][0]["type"] == KV_EVENT_CLEARED
    assert manager.hash_mapping == {}


@pytest.mark.asyncio
async def test_push_snapshot_sends_full_index_to_single_router():
    manager, client = _make_manager(["http://router-a:9080"])
    token_ids = list(range(32))
    await manager.add_blocks("qwen", [111, 222], "pod-1.default", token_ids)
    client.post.reset_mock()

    await manager.push_snapshot("http://router-new:9080", "pod-1.default")

    payloads = _pushed_payloads(client)
    assert len(payloads) == 1
    url, payload = payloads[0]
    assert url == f"http://router-new:9080{KV_EVENTS_PATH}"
    event = payload["events"][0]
    assert event["type"] == KV_EVENT_SNAPSHOT
    assert sorted(event["block_hashes"]) == sorted([
        compute_standardized_hash(token_ids[0:16]),
        compute_standardized_hash(token_ids[16:32]),
    ])
    # Snapshots must preserve the original per-block store times so the
    # router's engine-restart freshness filter keeps working.
    stored = manager._blocks["qwen"]
    assert event["timestamps"] == [stored[h] for h in event["block_hashes"]]


@pytest.mark.asyncio
async def test_push_snapshot_represents_cleared_model_as_empty():
    manager, client = _make_manager(["http://router-a:9080"])
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    await manager.clear_all_blocks("qwen", "pod-1.default")
    client.post.reset_mock()

    await manager.push_snapshot("http://router-new:9080", "pod-1.default")

    payloads = _pushed_payloads(client)
    assert len(payloads) == 1
    event = payloads[0][1]["events"][0]
    assert event["type"] == KV_EVENT_SNAPSHOT
    assert event["block_hashes"] == []


@pytest.mark.asyncio
async def test_failed_push_marks_endpoint_dirty_until_snapshot_succeeds():
    manager, client = _make_manager(["http://router-a:9080"])
    client.post.side_effect = RuntimeError("connection refused")

    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    assert manager.is_dirty("http://router-a:9080")

    # A successful snapshot reconciles the endpoint and clears dirty state.
    client.post.side_effect = None
    await manager.push_snapshot("http://router-a:9080", "pod-1.default")
    assert not manager.is_dirty("http://router-a:9080")


@pytest.mark.asyncio
async def test_failed_snapshot_keeps_endpoint_dirty():
    manager, client = _make_manager(["http://router-a:9080"])
    await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    client.post.side_effect = RuntimeError("connection refused")

    await manager.push_snapshot("http://router-a:9080", "pod-1.default")
    assert manager.is_dirty("http://router-a:9080")


@pytest.mark.asyncio
async def test_no_registered_routers_skips_push():
    manager, client = _make_manager([])
    ok = await manager.add_blocks("qwen", [111], "pod-1.default", list(range(16)))
    assert ok
    client.post.assert_not_awaited()


@pytest.mark.asyncio
async def test_add_blocks_with_mismatched_tokens_fails():
    manager, client = _make_manager(["http://router-a:9080"])
    # 10 tokens cannot be split evenly across 3 hashes
    ok = await manager.add_blocks("qwen", [1, 2, 3], "pod-1.default", list(range(10)))
    assert not ok
    client.post.assert_not_awaited()


def test_router_registry_register_and_expire(monkeypatch):
    registry = RouterRegistry()
    clock = {"now": 1000.0}
    monkeypatch.setattr(
        "kthena.runtime.router_registry.time.monotonic", lambda: clock["now"])

    # New registration needs a snapshot.
    assert registry.register("router-a", "http://10.0.0.5:9080", ttl_seconds=60)
    # Renewal before expiry does not.
    clock["now"] += 30
    assert not registry.register("router-a", "http://10.0.0.5:9080", ttl_seconds=60)
    assert registry.active_endpoints() == ["http://10.0.0.5:9080"]

    # Changing the endpoint triggers a snapshot again.
    assert registry.register("router-a", "http://10.0.0.6:9080", ttl_seconds=60)

    # Expired registrations are pruned and re-registration needs a snapshot.
    clock["now"] += 120
    assert registry.active_endpoints() == []
    assert registry.register("router-a", "http://10.0.0.6:9080", ttl_seconds=60)

    # A new process generation (router container restart with the same pod
    # name and endpoint) triggers a snapshot again.
    assert not registry.register(
        "router-a", "http://10.0.0.6:9080", ttl_seconds=60)
    assert registry.register(
        "router-a", "http://10.0.0.6:9080", ttl_seconds=60, generation="gen-2")
    assert not registry.register(
        "router-a", "http://10.0.0.6:9080", ttl_seconds=60, generation="gen-2")


def test_router_registry_rejects_invalid_input():
    registry = RouterRegistry()
    with pytest.raises(ValueError):
        registry.register("", "http://10.0.0.5:9080")
    with pytest.raises(ValueError):
        registry.register("router-a", "ftp://10.0.0.5:9080")
    with pytest.raises(ValueError):
        registry.register("router-a", "not-a-url")
