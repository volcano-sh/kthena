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

import logging
from datetime import datetime, timezone
from unittest.mock import AsyncMock, MagicMock

import pytest

from kthena.runtime.events import (
    EventType,
    SGLangEventData,
    SGLangBlockStoredEvent,
    SGLangBlockRemovedEvent,
    SGLangAllBlocksClearedEvent,
)
from kthena.runtime.kv_cache_manager import (
    SGLangKVCacheEventHandler,
    SGLangKVCacheRedisManager,
    VLLMKVCacheRedisManager,
    compute_standardized_hash,
)


class _FakePipeline:
    """Captures every call so tests can assert what was written to Redis.

    scard_results holds the values returned for scard calls, in order. When it
    runs out, scard returns 0 (no engine hash left for that block).
    """

    def __init__(self, scard_results=None):
        self.calls = []
        self.scard_results = list(scard_results or [])
        self._executed = 0

    def set(self, key, value):
        self.calls.append(("set", key, value))

    def expire(self, key, ttl):
        self.calls.append(("expire", key, ttl))

    def hset(self, key, field, value):
        self.calls.append(("hset", key, field, value))

    def hdel(self, key, field):
        self.calls.append(("hdel", key, field))

    def delete(self, key):
        self.calls.append(("delete", key))

    def exists(self, key):
        self.calls.append(("exists", key))

    def sadd(self, key, member):
        self.calls.append(("sadd", key, member))

    def srem(self, key, member):
        self.calls.append(("srem", key, member))

    def scard(self, key):
        self.calls.append(("scard", key))

    async def execute(self):
        # Like a real pipeline, return results only for the calls queued since
        # the last execute.
        pending = self.calls[self._executed:]
        self._executed = len(self.calls)
        results = []
        for call in pending:
            if call[0] == "scard":
                results.append(self.scard_results.pop(0) if self.scard_results else 0)
            else:
                results.append(1)
        return results


class _FakeRedisConn:
    def __init__(self, pipeline: _FakePipeline, get_results=None):
        self._pipeline = pipeline
        self._get_results = get_results or {}

    def pipeline(self):
        return self._pipeline

    async def get(self, key):
        return self._get_results.get(key)


def _make_redis_client(pipeline: _FakePipeline, keys_result=None, get_results=None):
    """Build a mock RedisClient compatible with kv_cache_manager."""
    conn = _FakeRedisConn(pipeline, get_results=get_results)
    client = MagicMock()
    client.get_connection = AsyncMock(return_value=conn)
    client.scan_keys = AsyncMock(return_value=keys_result or [])
    return client


@pytest.mark.asyncio
async def test_block_stored_writes_matrix_and_mapping_keys():
    pipeline = _FakePipeline()
    redis_client = _make_redis_client(pipeline)
    manager = SGLangKVCacheRedisManager(redis_client=redis_client)
    handler = SGLangKVCacheEventHandler(kv_manager=manager)

    token_ids = list(range(32))  # 2 blocks of 16
    sglang_block_hashes = [111, 222]
    expected_std_hashes = [
        compute_standardized_hash(token_ids[0:16]),
        compute_standardized_hash(token_ids[16:32]),
    ]

    event_data = SGLangEventData(
        event_type=EventType.SGLANG_BLOCK_STORED,
        timestamp=datetime.now(timezone.utc),
        model_name="qwen",
        pod_identifier="pod-1",
        attn_dp_rank=0,
        sglang_event=SGLangBlockStoredEvent(
            block_hashes=sglang_block_hashes,
            parent_block_hash=None,
            token_ids=token_ids,
            block_size=16,
            lora_id=None,
            medium="GPU",
        ),
    )

    await handler.handle(event_data)

    # Mapping keys written under the SGLang namespace, not vLLM's.
    set_calls = [c for c in pipeline.calls if c[0] == "set"]
    assert len(set_calls) == 2
    for (_, key, value), sglang_hash, std_hash in zip(
        set_calls, sglang_block_hashes, expected_std_hashes
    ):
        assert key == f"sglang:kv:block:pod-1@{sglang_hash}"
        assert value == str(std_hash)

    # Pod registered against each matrix block (engine-agnostic namespace).
    hset_calls = [c for c in pipeline.calls if c[0] == "hset"]
    assert len(hset_calls) == 2
    for (_, key, field, _), std_hash in zip(hset_calls, expected_std_hashes):
        assert key == f"matrix:kv:block:qwen@{std_hash}"
        assert field == "pod-1"

    # Each engine hash is recorded under its standardized hash for this pod.
    sadd_calls = [c for c in pipeline.calls if c[0] == "sadd"]
    assert sadd_calls == [
        ("sadd", f"sglang:kv:ref:pod-1@{std_hash}", str(sglang_hash))
        for sglang_hash, std_hash in zip(sglang_block_hashes, expected_std_hashes)
    ]


@pytest.mark.asyncio
async def test_block_removed_uses_in_memory_mapping():
    pipeline = _FakePipeline()
    redis_client = _make_redis_client(pipeline)
    manager = SGLangKVCacheRedisManager(redis_client=redis_client)

    # Pre-populate the mapping (would normally come from a previous BlockStored).
    manager.hash_mapping[111] = 999
    manager.hash_mapping[222] = 888

    handler = SGLangKVCacheEventHandler(kv_manager=manager)
    event_data = SGLangEventData(
        event_type=EventType.SGLANG_BLOCK_REMOVED,
        timestamp=datetime.now(timezone.utc),
        model_name="qwen",
        pod_identifier="pod-1",
        attn_dp_rank=None,
        sglang_event=SGLangBlockRemovedEvent(
            block_hashes=[111, 222], medium="DISK"
        ),
    )

    await handler.handle(event_data)

    hdel_calls = [c for c in pipeline.calls if c[0] == "hdel"]
    delete_calls = [c for c in pipeline.calls if c[0] == "delete"]
    assert {(c[1], c[2]) for c in hdel_calls} == {
        ("matrix:kv:block:qwen@999", "pod-1"),
        ("matrix:kv:block:qwen@888", "pod-1"),
    }
    assert {c[1] for c in delete_calls} == {
        "sglang:kv:block:pod-1@111",
        "sglang:kv:block:pod-1@222",
    }


@pytest.mark.asyncio
async def test_block_removed_keeps_pod_while_same_tokens_are_cached():
    # Same tokens after two different prefixes: two engine hashes, one
    # standardized hash. Evicting one copy must keep the pod in the matrix.
    pipeline = _FakePipeline(scard_results=[1])
    manager = VLLMKVCacheRedisManager(redis_client=_make_redis_client(pipeline))
    manager.hash_mapping[111] = 999
    manager.hash_mapping[222] = 999

    assert await manager.remove_blocks("qwen", [111], "pod-1") == 1
    assert ("srem", "vllm:kv:ref:pod-1@999", "111") in pipeline.calls
    assert ("delete", "vllm:kv:block:pod-1@111") in pipeline.calls
    assert [c for c in pipeline.calls if c[0] == "hdel"] == []

    # The last copy is gone, so now the pod leaves the matrix.
    assert await manager.remove_blocks("qwen", [222], "pod-1") == 1
    assert [c for c in pipeline.calls if c[0] == "hdel"] == [
        ("hdel", "matrix:kv:block:qwen@999", "pod-1"),
    ]


@pytest.mark.asyncio
async def test_block_removed_drops_shared_block_once_when_both_copies_go():
    pipeline = _FakePipeline(scard_results=[1, 0])
    manager = VLLMKVCacheRedisManager(redis_client=_make_redis_client(pipeline))
    manager.hash_mapping[111] = 999
    manager.hash_mapping[222] = 999

    assert await manager.remove_blocks("qwen", [111, 222], "pod-1") == 2
    assert [c for c in pipeline.calls if c[0] == "hdel"] == [
        ("hdel", "matrix:kv:block:qwen@999", "pod-1"),
    ]


@pytest.mark.asyncio
async def test_all_blocks_cleared_clears_pod_from_matrix():
    pipeline = _FakePipeline()
    matrix_keys = ["matrix:kv:block:qwen@1", "matrix:kv:block:qwen@2"]
    mapping_keys = ["sglang:kv:block:pod-1@111", "sglang:kv:block:pod-1@222"]
    ref_keys = ["sglang:kv:ref:pod-1@1", "sglang:kv:ref:pod-1@2"]
    redis_client = _make_redis_client(
        pipeline,
        keys_result=matrix_keys + mapping_keys + ref_keys,
    )

    # `scan_keys()` is called three times (matrix, mapping, then ref). Stub it to
    # return each set in order.
    redis_client.scan_keys = AsyncMock(side_effect=[matrix_keys, mapping_keys, ref_keys])

    manager = SGLangKVCacheRedisManager(redis_client=redis_client)
    handler = SGLangKVCacheEventHandler(kv_manager=manager)
    event_data = SGLangEventData(
        event_type=EventType.SGLANG_ALL_BLOCKS_CLEARED,
        timestamp=datetime.now(timezone.utc),
        model_name="qwen",
        pod_identifier="pod-1",
        attn_dp_rank=None,
        sglang_event=SGLangAllBlocksClearedEvent(),
    )

    await handler.handle(event_data)

    hdel_calls = [c for c in pipeline.calls if c[0] == "hdel"]
    delete_calls = [c for c in pipeline.calls if c[0] == "delete"]
    assert {(c[1], c[2]) for c in hdel_calls} == {
        ("matrix:kv:block:qwen@1", "pod-1"),
        ("matrix:kv:block:qwen@2", "pod-1"),
    }
    assert {c[1] for c in delete_calls} == set(mapping_keys + ref_keys)


def test_sglang_and_vllm_managers_use_distinct_namespaces():
    """Regression: a shared Redis must keep engine namespaces separated."""
    assert VLLMKVCacheRedisManager.MAPPING_KEY_PREFIX == "vllm:kv:block"
    assert SGLangKVCacheRedisManager.MAPPING_KEY_PREFIX == "sglang:kv:block"
    assert (
        VLLMKVCacheRedisManager.MAPPING_KEY_PREFIX
        != SGLangKVCacheRedisManager.MAPPING_KEY_PREFIX
    )
    assert VLLMKVCacheRedisManager.REF_KEY_PREFIX == "vllm:kv:ref"
    assert SGLangKVCacheRedisManager.REF_KEY_PREFIX == "sglang:kv:ref"


class _FakeScanClient:
    """Serves SCAN batches and records the cursors it was called with."""

    def __init__(self, batches):
        self._batches = list(batches)
        self.cursors = []

    async def scan(self, cursor=0, match=None, count=None):
        self.cursors.append(cursor)
        return self._batches.pop(0)


def _make_scanning_redis_client(batches):
    from kthena.runtime.redis_client import RedisClient

    client = RedisClient()
    client._client = _FakeScanClient(batches)
    client._connected = True
    return client


@pytest.mark.asyncio
async def test_scan_keys_walks_cursor_until_exhausted():
    """A single SCAN round only covers part of the keyspace, so it must be followed."""
    client = _make_scanning_redis_client(
        [(7, ["matrix:kv:block:qwen@1"]), (0, ["matrix:kv:block:qwen@2"])]
    )

    keys = await client.scan_keys("matrix:kv:block:qwen@*")

    assert keys == ["matrix:kv:block:qwen@1", "matrix:kv:block:qwen@2"]
    assert client._client.cursors == [0, 7]


@pytest.mark.asyncio
async def test_scan_keys_dedupes_repeated_keys():
    """SCAN may report the same key in more than one round."""
    client = _make_scanning_redis_client(
        [(3, ["matrix:kv:block:qwen@1"]), (0, ["matrix:kv:block:qwen@1"])]
    )

    assert await client.scan_keys("matrix:kv:block:qwen@*") == ["matrix:kv:block:qwen@1"]


@pytest.mark.asyncio
async def test_scan_keys_returns_empty_on_redis_error():
    """Matches the previous keys() behaviour so callers need no new error handling."""
    from redis.exceptions import RedisError

    client = _make_scanning_redis_client([])

    async def failing_scan(cursor=0, match=None, count=None):
        raise RedisError("boom")

    client._client.scan = failing_scan

    assert await client.scan_keys("matrix:kv:block:qwen@*") == []


@pytest.mark.asyncio
async def test_scan_keys_handles_empty_round_before_cursor_returns_to_zero():
    """SCAN may return nothing for a round while the cursor is still non-zero."""
    client = _make_scanning_redis_client([(9, []), (0, ["matrix:kv:block:qwen@1"])])

    assert await client.scan_keys("matrix:kv:block:qwen@*") == [
        "matrix:kv:block:qwen@1"
    ]
    assert client._client.cursors == [0, 9]


def test_standardized_hash_keeps_token_ids_out_of_info_logs(caplog):
    """Token ids detokenize back to prompt text, so INFO must never carry them."""
    with caplog.at_level(logging.INFO, logger="kthena.runtime.kv_cache_manager"):
        compute_standardized_hash([101, 202, 303])
    assert caplog.records == []

    caplog.clear()
    with caplog.at_level(logging.DEBUG, logger="kthena.runtime.kv_cache_manager"):
        compute_standardized_hash([101, 202, 303])
    assert len(caplog.records) == 1
    message = caplog.records[0].getMessage()
    assert "tokens=3" in message
    assert "token_ids" not in message
    assert "101" not in message
