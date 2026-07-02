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

import hashlib
import logging
import time
from typing import List, Optional, Dict

from kthena.runtime.events import (
    EventHandler, EventType, EventData, VLLMEventData,
    VLLMBlockStoredEvent, VLLMBlockRemovedEvent,
    SGLangEventData, SGLangBlockStoredEvent, SGLangBlockRemovedEvent,
)
from kthena.runtime.redis_client import RedisClient, get_redis_client

logger = logging.getLogger(__name__)


def get_matrix_key_prefix() -> str:
    return "matrix:kv:block"


def get_vllm_mapping_key_prefix() -> str:
    return "vllm:kv:block"


def get_sglang_mapping_key_prefix() -> str:
    return "sglang:kv:block"


def compute_standardized_hash(token_ids: List[int]) -> int:
    if not token_ids:
        return 0
    token_bytes = b''.join(token_id.to_bytes(4, byteorder='big') for token_id in token_ids)
    hash_obj = hashlib.sha256(token_bytes)
    full_hash = int.from_bytes(hash_obj.digest()[:8], byteorder='big')
    result = full_hash & 0x7FFFFFFFFFFFFFFF
    logger.info(f"KVCacheManager: compute standardized hash={result}, token_ids={token_ids}")
    return result


class VLLMKVCacheRedisManager:

    MAPPING_KEY_PREFIX: str = "vllm:kv:block"

    def __init__(self, redis_client: Optional[RedisClient] = None):
        self.redis_client = redis_client or get_redis_client()
        self.hash_mapping: Dict[int, int] = {}

    @staticmethod
    def _get_matrix_block_key(model_name: str, chunk_hash: int) -> str:
        return f"{get_matrix_key_prefix()}:{model_name}@{chunk_hash}"

    @classmethod
    def _get_hash_mapping_key(cls, engine_hash: int, pod_identifier: str) -> str:
        return f"{cls.MAPPING_KEY_PREFIX}:{pod_identifier}@{engine_hash}"

    async def add_blocks(self, model_name: str, block_hashes: List[int],
                         pod_identifier: str, token_ids: Optional[List[int]] = None) -> bool:
        if not block_hashes or not model_name or not pod_identifier:
            return not block_hashes

        if not token_ids:
            return True

        try:
            client = await self.redis_client.get_connection()
            pipe = client.pipeline()
            timestamp = str(int(time.time()))

            success = await self._process_token_blocks_with_mapping(
                model_name, block_hashes, token_ids, pod_identifier, timestamp, pipe)

            if not success:
                return False

            await pipe.execute()

            logger.info(
                f"Runtime Redis Write - Model: {model_name}, Pod: {pod_identifier}, "
                f"BlockHashes: {block_hashes}, Count: {len(block_hashes)}")
            return True

        except Exception as e:
            logger.error(f"Error adding blocks to Redis: {e}")
            return False

    async def _process_token_blocks_with_mapping(self, model_name: str, engine_hashes: List[int],
                                                 token_ids: List[int], pod_identifier: str,
                                                 timestamp: str, pipe) -> bool:
        if not engine_hashes:
            return False

        # Calculate block size: len(token_ids) / len(block_hashes)
        block_size = len(token_ids) // len(engine_hashes)
        if len(token_ids) % len(engine_hashes) != 0:
            logger.error(f"Token count ({len(token_ids)}) cannot match block size with {len(engine_hashes)} hashes")
            return False

        for i, engine_hash in enumerate(engine_hashes):
            start_idx = i * block_size
            end_idx = min(start_idx + block_size, len(token_ids))
            block_tokens = token_ids[start_idx:end_idx]
            if len(block_tokens) == 0:
                continue
            std_hash = compute_standardized_hash(block_tokens)
            matrix_block_key = self._get_matrix_block_key(model_name, std_hash)
            self.hash_mapping[engine_hash] = std_hash
            mapping_key = self._get_hash_mapping_key(engine_hash, pod_identifier)
            pipe.set(mapping_key, str(std_hash))
            pipe.expire(mapping_key, 86400)
            pipe.hset(matrix_block_key, pod_identifier, timestamp)
        return True

    async def remove_blocks(self, model_name: str, block_hashes: List[int],
                            pod_identifier: str) -> int:
        if not block_hashes:
            return 0

        if not model_name or not pod_identifier:
            return 0

        try:
            client = await self.redis_client.get_connection()
            pipe = client.pipeline()

            removed_count = 0

            for engine_hash in block_hashes:
                std_hash = await self._get_std_hash(client, engine_hash, pod_identifier)

                if std_hash is not None:
                    matrix_block_key = self._get_matrix_block_key(model_name, std_hash)
                    mapping_key = self._get_hash_mapping_key(engine_hash, pod_identifier)

                    pipe.hdel(matrix_block_key, pod_identifier)
                    pipe.delete(mapping_key)

                    self.hash_mapping.pop(engine_hash, None)
                    removed_count += 1

            await pipe.execute()
            logger.info(f"Removed {removed_count} blocks for model {model_name}, pod {pod_identifier}")
            return removed_count

        except Exception as e:
            logger.error(f"Error removing blocks from Redis: {e}")
            return 0

    async def _get_std_hash(self, client, engine_hash: int, pod_identifier: str = None) -> Optional[int]:
        std_hash = self.hash_mapping.get(engine_hash)

        if std_hash is None:
            if pod_identifier:
                mapping_key = self._get_hash_mapping_key(engine_hash, pod_identifier)
                std_hash_str = await client.get(mapping_key)
                if std_hash_str:
                    std_hash = int(std_hash_str)
                    self.hash_mapping[engine_hash] = std_hash
            else:
                pattern = f"{self.MAPPING_KEY_PREFIX}:*@{engine_hash}"
                keys = await self.redis_client.keys(pattern)
                if keys:
                    std_hash_str = await client.get(keys[0])
                    if std_hash_str:
                        std_hash = int(std_hash_str)
                        self.hash_mapping[engine_hash] = std_hash

        return std_hash

    @staticmethod
    async def get_standardized_hashes(token_blocks: List[List[int]]) -> List[int]:
        std_hashes = []
        for token_block in token_blocks:
            if len(token_block) > 0:
                std_hash = compute_standardized_hash(token_block)
                std_hashes.append(std_hash)

        return std_hashes

    async def check_blocks_exist(self, model_name: str, std_hashes: List[int]) -> List[bool]:
        if not std_hashes:
            return []

        try:
            client = await self.redis_client.get_connection()
            pipe = client.pipeline()

            for std_hash in std_hashes:
                matrix_block_key = self._get_matrix_block_key(model_name, std_hash)
                pipe.exists(matrix_block_key)

            results = await pipe.execute()
            return [bool(result) for result in results]

        except Exception as e:
            logger.error(f"Error checking block existence: {e}")
            return [False] * len(std_hashes)

    async def clear_all_blocks(self, model_name: str, pod_identifier: str) -> int:
        if not model_name or not pod_identifier:
            return 0

        try:
            pod_field = pod_identifier
            matrix_pattern = f"{get_matrix_key_prefix()}:{model_name}@*"
            mapping_pattern = f"{self.MAPPING_KEY_PREFIX}:{pod_identifier}@*"

            matrix_keys = await self.redis_client.keys(matrix_pattern)
            mapping_keys = await self.redis_client.keys(mapping_pattern)

            if not matrix_keys:
                if mapping_keys:
                    client = await self.redis_client.get_connection()
                    pipe = client.pipeline()
                    for mapping_key in mapping_keys:
                        pipe.delete(mapping_key)
                    await pipe.execute()
                    logger.info(f"Cleared {len(mapping_keys)} mapping keys for pod {pod_identifier}")
                return 0

            client = await self.redis_client.get_connection()
            pipe = client.pipeline()

            for key in matrix_keys:
                pipe.hdel(key, pod_field)

            for mapping_key in mapping_keys:
                pipe.delete(mapping_key)

            results = await pipe.execute()
            deleted_count = sum(1 for result in results[:len(matrix_keys)] if result > 0)

            logger.info(
                f"Cleared {deleted_count} matrix blocks and {len(mapping_keys)} mapping keys "
                f"for model {model_name}, pod {pod_identifier}")
            return deleted_count

        except Exception as e:
            logger.error(f"Error clearing blocks from Redis: {e}")
            return 0


class VLLMKVCacheEventHandler(EventHandler):

    def __init__(self, redis_manager: Optional[VLLMKVCacheRedisManager] = None):
        self.redis_manager = redis_manager or VLLMKVCacheRedisManager()

    async def handle(self, event_data: EventData) -> None:
        if not event_data:
            return

        try:
            if not isinstance(event_data, VLLMEventData):
                return

            logger.info(
                f"Handling vLLM event: {event_data.event_type.value} "
                f"for model={event_data.model_name}, pod={event_data.pod_identifier}")

            if event_data.event_type == EventType.VLLM_BLOCK_STORED:
                await self._handle_block_stored(event_data)
            elif event_data.event_type == EventType.VLLM_BLOCK_REMOVED:
                await self._handle_block_removed(event_data)
            elif event_data.event_type == EventType.VLLM_ALL_BLOCKS_CLEARED:
                await self._handle_all_blocks_cleared(event_data)

            logger.info(f"Successfully handled vLLM event: {event_data.event_type.value}")

        except Exception as e:
            event_type = getattr(event_data, 'event_type', 'unknown')
            logger.error(f"Error handling vLLM event {event_type}: {e}")

    async def _handle_block_stored(self, event_data: VLLMEventData) -> None:
        if not isinstance(event_data.vllm_event, VLLMBlockStoredEvent):
            return

        block_event = event_data.vllm_event
        await self.redis_manager.add_blocks(
            model_name=event_data.model_name,
            block_hashes=block_event.block_hashes,
            pod_identifier=event_data.pod_identifier,
            token_ids=block_event.token_ids
        )

    async def _handle_block_removed(self, event_data: VLLMEventData) -> None:
        if not isinstance(event_data.vllm_event, VLLMBlockRemovedEvent):
            return

        block_event = event_data.vllm_event
        await self.redis_manager.remove_blocks(
            model_name=event_data.model_name,
            block_hashes=block_event.block_hashes,
            pod_identifier=event_data.pod_identifier
        )

    async def _handle_all_blocks_cleared(self, event_data: VLLMEventData) -> None:
        await self.redis_manager.clear_all_blocks(
            model_name=event_data.model_name,
            pod_identifier=event_data.pod_identifier
        )


def get_vllm_kv_cache_handler() -> VLLMKVCacheEventHandler:
    return VLLMKVCacheEventHandler()


class SGLangKVCacheRedisManager(VLLMKVCacheRedisManager):
    "KV cache Redis manager for SGLang. Reuses the vLLM logic."

    MAPPING_KEY_PREFIX: str = "sglang:kv:block"


class SGLangKVCacheEventHandler(EventHandler):

    def __init__(self, redis_manager: Optional[SGLangKVCacheRedisManager] = None):
        self.redis_manager = redis_manager or SGLangKVCacheRedisManager()

    async def handle(self, event_data: EventData) -> None:
        if not event_data:
            return

        try:
            if not isinstance(event_data, SGLangEventData):
                return

            logger.info(
                f"Handling SGLang event: {event_data.event_type.value} "
                f"for model={event_data.model_name}, pod={event_data.pod_identifier}")

            if event_data.event_type == EventType.SGLANG_BLOCK_STORED:
                await self._handle_block_stored(event_data)
            elif event_data.event_type == EventType.SGLANG_BLOCK_REMOVED:
                await self._handle_block_removed(event_data)
            elif event_data.event_type == EventType.SGLANG_ALL_BLOCKS_CLEARED:
                await self._handle_all_blocks_cleared(event_data)

            logger.info(f"Successfully handled SGLang event: {event_data.event_type.value}")

        except Exception as e:
            event_type = getattr(event_data, 'event_type', 'unknown')
            logger.error(f"Error handling SGLang event {event_type}: {e}")

    async def _handle_block_stored(self, event_data: SGLangEventData) -> None:
        if not isinstance(event_data.sglang_event, SGLangBlockStoredEvent):
            return

        block_event = event_data.sglang_event
        await self.redis_manager.add_blocks(
            model_name=event_data.model_name,
            block_hashes=block_event.block_hashes,
            pod_identifier=event_data.pod_identifier,
            token_ids=block_event.token_ids,
        )

    async def _handle_block_removed(self, event_data: SGLangEventData) -> None:
        if not isinstance(event_data.sglang_event, SGLangBlockRemovedEvent):
            return

        block_event = event_data.sglang_event
        await self.redis_manager.remove_blocks(
            model_name=event_data.model_name,
            block_hashes=block_event.block_hashes,
            pod_identifier=event_data.pod_identifier,
        )

    async def _handle_all_blocks_cleared(self, event_data: SGLangEventData) -> None:
        await self.redis_manager.clear_all_blocks(
            model_name=event_data.model_name,
            pod_identifier=event_data.pod_identifier,
        )


def get_sglang_kv_cache_handler() -> SGLangKVCacheEventHandler:
    return SGLangKVCacheEventHandler()
