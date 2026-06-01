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
    compute_standardized_hash,
)


class _FakePipeline:
    """Captures every call so tests can assert what was written to Redis."""

    def __init__(self):
        self.calls = []

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

    async def execute(self):
        return [1] * len(self.calls)


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
    client.keys = AsyncMock(return_value=keys_result or [])
    return client


@pytest.mark.asyncio
async def test_block_stored_writes_matrix_and_mapping_keys():
    pipeline = _FakePipeline()
    redis_client = _make_redis_client(pipeline)
    manager = SGLangKVCacheRedisManager(redis_client=redis_client)
    handler = SGLangKVCacheEventHandler(redis_manager=manager)

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


@pytest.mark.asyncio
async def test_block_removed_uses_in_memory_mapping():
    pipeline = _FakePipeline()
    redis_client = _make_redis_client(pipeline)
    manager = SGLangKVCacheRedisManager(redis_client=redis_client)

    # Pre-populate the mapping (would normally come from a previous BlockStored).
    manager.hash_mapping[111] = 999
    manager.hash_mapping[222] = 888

    handler = SGLangKVCacheEventHandler(redis_manager=manager)
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
async def test_all_blocks_cleared_clears_pod_from_matrix():
    pipeline = _FakePipeline()
    matrix_keys = ["matrix:kv:block:qwen@1", "matrix:kv:block:qwen@2"]
    mapping_keys = ["sglang:kv:block:pod-1@111", "sglang:kv:block:pod-1@222"]
    redis_client = _make_redis_client(
        pipeline,
        keys_result=matrix_keys + mapping_keys,
    )

    # `keys()` is called twice (matrix, then mapping). Stub it to return each
    # set in order.
    redis_client.keys = AsyncMock(side_effect=[matrix_keys, mapping_keys])

    manager = SGLangKVCacheRedisManager(redis_client=redis_client)
    handler = SGLangKVCacheEventHandler(redis_manager=manager)
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
    assert {c[1] for c in delete_calls} == set(mapping_keys)


def test_sglang_and_vllm_managers_use_distinct_namespaces():
    """Regression: a shared Redis must keep engine namespaces separated."""
    from kthena.runtime.kv_cache_manager import VLLMKVCacheRedisManager
    assert VLLMKVCacheRedisManager.MAPPING_KEY_PREFIX == "vllm:kv:block"
    assert SGLangKVCacheRedisManager.MAPPING_KEY_PREFIX == "sglang:kv:block"
    assert (
        VLLMKVCacheRedisManager.MAPPING_KEY_PREFIX
        != SGLangKVCacheRedisManager.MAPPING_KEY_PREFIX
    )
