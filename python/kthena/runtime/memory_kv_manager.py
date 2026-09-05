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
import logging
import time
from typing import Dict, List, Optional

import httpx

from kthena.runtime.kv_cache_manager import standardize_block_hashes
from kthena.runtime.router_registry import RouterRegistry, get_router_registry

logger = logging.getLogger(__name__)

KV_EVENTS_PATH = "/kvcache/events"

KV_EVENT_STORED = "stored"
KV_EVENT_REMOVED = "removed"
KV_EVENT_CLEARED = "cleared"
KV_EVENT_SNAPSHOT = "snapshot"

PUSH_TIMEOUT_SECONDS = 2.0


class MemoryKVCacheManager:
    """KV cache manager that pushes standardized block hashes directly to
    registered router instances instead of writing them to Redis.

    Implements the same interface as VLLMKVCacheRedisManager
    (add_blocks / remove_blocks / clear_all_blocks) so it can back
    VLLMKVCacheEventHandler and SGLangKVCacheEventHandler.

    Unlike Redis mode, there is no shared, durable store here: every router
    process keeps its own private in-memory copy of the index. In Redis mode
    the sidecar writes each event once into Redis and can forget it, because
    Redis itself is the authoritative state that any router — including one
    that just (re)started or briefly lost connectivity — reads on demand. In
    memory mode this class has to retain that authoritative state (_blocks)
    itself, so it can replay it as a full snapshot to any router that starts
    fresh, restarts, or missed a delta push.
    """

    def __init__(self, registry: Optional[RouterRegistry] = None,
                 client: Optional[httpx.AsyncClient] = None):
        self.registry = registry or get_router_registry()
        self._client = client
        # engine hash -> standardized hash, needed because removal events only
        # carry engine hashes.
        self.hash_mapping: Dict[int, int] = {}
        # model name -> {std_hash: unix seconds when stored}, mirrors what has
        # been pushed so a newly registered router can receive a snapshot.
        # Models stay present (with an empty dict) after being cleared so a
        # snapshot can authoritatively represent an empty cache.
        self._blocks: Dict[str, Dict[int, int]] = {}
        # Per-endpoint locks serializing snapshot and delta delivery so an
        # older replace-style snapshot cannot erase a newer delta.
        self._endpoint_locks: Dict[str, asyncio.Lock] = {}
        # Endpoints whose last push failed; they receive a fresh snapshot on
        # their next registration heartbeat instead of staying divergent.
        self._dirty_endpoints: set = set()

    def _endpoint_lock(self, endpoint: str) -> asyncio.Lock:
        lock = self._endpoint_locks.get(endpoint)
        if lock is None:
            lock = asyncio.Lock()
            self._endpoint_locks[endpoint] = lock
        return lock

    def is_dirty(self, endpoint: str) -> bool:
        """Whether the last push to this endpoint failed and its index may
        have diverged, requiring a fresh snapshot."""
        return endpoint in self._dirty_endpoints

    def _get_client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = httpx.AsyncClient(
                timeout=httpx.Timeout(PUSH_TIMEOUT_SECONDS))
        return self._client

    async def close(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    async def add_blocks(self, model_name: str, block_hashes: List[int],
                         pod_identifier: str, token_ids: Optional[List[int]] = None) -> bool:
        if not block_hashes or not model_name or not pod_identifier:
            return not block_hashes
        if not token_ids:
            return True

        pairs = standardize_block_hashes(block_hashes, token_ids)
        if pairs is None:
            return False

        timestamp = int(time.time())
        model_blocks = self._blocks.setdefault(model_name, {})
        std_hashes = []
        for engine_hash, std_hash in pairs:
            self.hash_mapping[engine_hash] = std_hash
            model_blocks[std_hash] = timestamp
            std_hashes.append(std_hash)

        await self._push_to_all(pod_identifier, model_name, [{
            "type": KV_EVENT_STORED,
            "block_hashes": std_hashes,
            "timestamp": timestamp,
        }])
        logger.info(
            f"Runtime memory push - Model: {model_name}, Pod: {pod_identifier}, "
            f"Count: {len(std_hashes)}")
        return True

    async def remove_blocks(self, model_name: str, block_hashes: List[int],
                            pod_identifier: str) -> int:
        if not block_hashes or not model_name or not pod_identifier:
            return 0

        model_blocks = self._blocks.get(model_name, {})
        std_hashes = []
        for engine_hash in block_hashes:
            std_hash = self.hash_mapping.pop(engine_hash, None)
            if std_hash is None:
                continue
            model_blocks.pop(std_hash, None)
            std_hashes.append(std_hash)

        if not std_hashes:
            return 0

        await self._push_to_all(pod_identifier, model_name, [{
            "type": KV_EVENT_REMOVED,
            "block_hashes": std_hashes,
        }])
        logger.info(
            f"Removed {len(std_hashes)} blocks for model {model_name}, pod {pod_identifier}")
        return len(std_hashes)

    async def clear_all_blocks(self, model_name: str, pod_identifier: str) -> int:
        if not model_name or not pod_identifier:
            return 0

        cleared = len(self._blocks.get(model_name, {}))
        # Keep the model key so later snapshots can still represent the empty
        # cache for this model and remove stale router entries.
        self._blocks[model_name] = {}
        self.hash_mapping.clear()

        await self._push_to_all(pod_identifier, model_name, [{
            "type": KV_EVENT_CLEARED,
        }])
        logger.info(
            f"Cleared {cleared} blocks for model {model_name}, pod {pod_identifier}")
        return cleared

    async def push_snapshot(self, endpoint: str, pod_identifier: str) -> None:
        """Send the full current block index to a single router endpoint.

        Called when a router registers for the first time, re-registers after
        its previous registration expired, or heartbeats while marked dirty
        after a failed push, so it can rebuild its in-memory index.

        The endpoint lock serializes the snapshot with concurrent delta
        pushes: the snapshot payload is built while holding the lock, so it
        reflects every mutation whose delta was already delivered and cannot
        be overtaken by a newer delta it does not contain.
        """
        ok = True
        async with self._endpoint_lock(endpoint):
            for model_name, model_blocks in self._blocks.items():
                # Preserve original per-block store times; re-stamping them at
                # snapshot time would defeat the engine-restart freshness filter.
                hashes = list(model_blocks.keys())
                if not await self._push_locked(endpoint, pod_identifier, model_name, [{
                    "type": KV_EVENT_SNAPSHOT,
                    "block_hashes": hashes,
                    "timestamps": [model_blocks[h] for h in hashes],
                }]):
                    ok = False
        if ok:
            self._dirty_endpoints.discard(endpoint)
            logger.info(f"Pushed KV snapshot to router endpoint {endpoint}")
        else:
            logger.warning(
                f"KV snapshot to router endpoint {endpoint} failed; will retry "
                f"on its next registration heartbeat")

    async def _push_to_all(self, pod_identifier: str, model_name: str,
                           events: List[dict]) -> None:
        endpoints = self.registry.active_endpoints()
        if not endpoints:
            logger.debug("No routers registered, skipping KV event push")
            return
        await asyncio.gather(
            *(self._push(endpoint, pod_identifier, model_name, events)
              for endpoint in endpoints),
        )

    async def _push(self, endpoint: str, pod_identifier: str, model_name: str,
                    events: List[dict]) -> bool:
        async with self._endpoint_lock(endpoint):
            return await self._push_locked(
                endpoint, pod_identifier, model_name, events)

    async def _push_locked(self, endpoint: str, pod_identifier: str,
                           model_name: str, events: List[dict]) -> bool:
        payload = {
            "pod_identifier": pod_identifier,
            "model_name": model_name,
            "events": events,
        }
        try:
            response = await self._get_client().post(
                f"{endpoint}{KV_EVENTS_PATH}", json=payload)
            response.raise_for_status()
            return True
        except Exception as e:
            # Mark the endpoint dirty so its next registration heartbeat
            # triggers a full snapshot instead of leaving it divergent.
            self._dirty_endpoints.add(endpoint)
            logger.warning(f"Failed to push KV events to router {endpoint}: {e}")
            return False


_memory_kv_manager: Optional[MemoryKVCacheManager] = None


def get_memory_kv_manager() -> MemoryKVCacheManager:
    global _memory_kv_manager
    if _memory_kv_manager is None:
        _memory_kv_manager = MemoryKVCacheManager()
    return _memory_kv_manager
