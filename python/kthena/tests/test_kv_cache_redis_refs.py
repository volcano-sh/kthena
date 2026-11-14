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

import fakeredis
import pytest
from redis.exceptions import ConnectionError as RedisConnectionError

from kthena.runtime.events import EventType, SGLangEventData, VLLMEventData
from kthena.runtime.kv_cache_manager import (
    SGLangKVCacheEventHandler,
    SGLangKVCacheRedisManager,
    VLLMKVCacheEventHandler,
    VLLMKVCacheRedisManager,
    compute_standardized_hash,
)
from kthena.runtime.memory_kv_manager import MemoryKVCacheManager

MODEL = "qwen"
POD = "pod-1"
# Blocks of 4 tokens. C follows two different first blocks, so the engine
# gives it two hashes while its standardized hash is the same.
P1 = [1, 2, 3, 4]
P2 = [5, 6, 7, 8]
C = [9, 10, 11, 12]
STD_C = compute_standardized_hash(C)


class _FakeRedisClient:
    """RedisClient on top of fakeredis, which also runs the Lua script."""

    def __init__(self):
        self.redis = fakeredis.FakeAsyncRedis(decode_responses=True)

    async def get_connection(self):
        return self.redis

    async def scan_keys(self, pattern):
        return [key async for key in self.redis.scan_iter(match=pattern)]


async def _owners(redis, std_hash):
    return set(await redis.hkeys(f"matrix:kv:block:{MODEL}@{std_hash}"))


@pytest.mark.asyncio
@pytest.mark.parametrize("manager_cls,prefix", [
    (VLLMKVCacheRedisManager, "vllm"),
    (SGLangKVCacheRedisManager, "sglang"),
])
async def test_pod_stays_listed_until_last_copy_is_evicted(manager_cls, prefix):
    client = _FakeRedisClient()
    manager = manager_cls(redis_client=client)
    await manager.add_blocks(MODEL, [11, 12], POD, P1 + C)
    await manager.add_blocks(MODEL, [21, 22], POD, P2 + C)

    assert await manager.remove_blocks(MODEL, [12], POD) == 1
    assert await _owners(client.redis, STD_C) == {POD}
    assert not await client.redis.exists(f"{prefix}:kv:block:{POD}@12")

    assert await manager.remove_blocks(MODEL, [22], POD) == 1
    assert await _owners(client.redis, STD_C) == set()
    assert not await client.redis.exists(f"{prefix}:kv:ref:{POD}@{STD_C}")


@pytest.mark.asyncio
async def test_both_copies_evicted_in_one_event():
    client = _FakeRedisClient()
    manager = VLLMKVCacheRedisManager(redis_client=client)
    await manager.add_blocks(MODEL, [11, 12], POD, P1 + C)
    await manager.add_blocks(MODEL, [21, 22], POD, P2 + C)

    assert await manager.remove_blocks(MODEL, [12, 22], POD) == 2
    assert await _owners(client.redis, STD_C) == set()
    assert await _owners(client.redis, compute_standardized_hash(P1)) == {POD}


@pytest.mark.asyncio
async def test_block_stored_without_ref_set_is_removed():
    # Written by a runtime from before the ref sets: mapping and matrix only.
    client = _FakeRedisClient()
    await client.redis.set(f"vllm:kv:block:{POD}@12", str(STD_C))
    await client.redis.hset(f"matrix:kv:block:{MODEL}@{STD_C}", POD, "1")
    manager = VLLMKVCacheRedisManager(redis_client=client)

    assert await manager.remove_blocks(MODEL, [12], POD) == 1
    assert await _owners(client.redis, STD_C) == set()
    assert not await client.redis.exists(f"vllm:kv:block:{POD}@12")


@pytest.mark.asyncio
async def test_failed_removal_leaves_state_unchanged():
    client = _FakeRedisClient()
    manager = VLLMKVCacheRedisManager(redis_client=client)
    await manager.add_blocks(MODEL, [11, 12], POD, P1 + C)

    real_eval = client.redis.eval
    client.redis.eval = AsyncMock(side_effect=RedisConnectionError("connection lost"))
    assert await manager.remove_blocks(MODEL, [12], POD) == 0
    assert manager.hash_mapping[12] == STD_C
    assert await client.redis.exists(f"vllm:kv:block:{POD}@12")
    assert await _owners(client.redis, STD_C) == {POD}

    # The local mapping is still there, so the same removal works later.
    client.redis.eval = real_eval
    assert await manager.remove_blocks(MODEL, [12], POD) == 1
    assert await _owners(client.redis, STD_C) == set()


@pytest.mark.asyncio
@pytest.mark.parametrize("manager_cls,handler_cls,event_cls,event_type", [
    (VLLMKVCacheRedisManager, VLLMKVCacheEventHandler, VLLMEventData, EventType.VLLM_STREAM_STARTED),
    (SGLangKVCacheRedisManager, SGLangKVCacheEventHandler, SGLangEventData, EventType.SGLANG_STREAM_STARTED),
])
async def test_new_stream_drops_refs_of_restarted_engine(manager_cls, handler_cls, event_cls, event_type):
    # The engine caches C after P1 and after P2, then restarts and loses its
    # cache without removal events. Only P2 + C is stored again, then evicted.
    client = _FakeRedisClient()
    manager = manager_cls(redis_client=client)
    handler = handler_cls(kv_manager=manager)
    await manager.add_blocks(MODEL, [11, 12], POD, P1 + C)
    await manager.add_blocks(MODEL, [21, 22], POD, P2 + C)

    await handler.handle(event_cls(
        event_type=event_type,
        timestamp=datetime.now(timezone.utc),
        model_name=MODEL,
        pod_identifier=POD,
    ))
    await manager.add_blocks(MODEL, [31, 32], POD, P2 + C)
    await manager.remove_blocks(MODEL, [32], POD)

    assert await _owners(client.redis, STD_C) == set()


@pytest.mark.asyncio
async def test_reset_only_touches_the_pods_ref_sets():
    client = _FakeRedisClient()
    manager = VLLMKVCacheRedisManager(redis_client=client)
    await manager.add_blocks(MODEL, [11, 12], POD, P1 + C)
    await manager.add_blocks(MODEL, [41, 42], "pod-2", P1 + C)

    assert await manager.reset_block_refs(POD) == 2
    assert [key async for key in client.redis.scan_iter(match=f"vllm:kv:ref:{POD}@*")] == []
    assert await client.redis.smembers(f"vllm:kv:ref:pod-2@{STD_C}") == {"42"}
    assert await _owners(client.redis, STD_C) == {POD, "pod-2"}
    assert await client.redis.exists(f"vllm:kv:block:{POD}@12")


@pytest.mark.asyncio
async def test_stream_started_is_ignored_in_memory_mode():
    manager = MagicMock(spec=MemoryKVCacheManager)
    handler = SGLangKVCacheEventHandler(kv_manager=manager)

    await handler.handle(SGLangEventData(
        event_type=EventType.SGLANG_STREAM_STARTED,
        timestamp=datetime.now(timezone.utc),
        model_name=MODEL,
        pod_identifier=POD,
    ))

    assert manager.mock_calls == []
