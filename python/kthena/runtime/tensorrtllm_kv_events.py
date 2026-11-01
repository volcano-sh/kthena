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

"""Poll trtllm-serve buffered KV events (POST /kv_cache_events) and publish
them through the existing vLLM KV-cache handlers.

TensorRT-LLM 1.0.0 exposes the buffered HTTP path when
`kv_cache_config.enable_block_reuse=true` and
`kv_cache_config.event_buffer_max_size>0`. Streaming ZMQ KVEventsConfig is
not required for this path.
"""

from __future__ import annotations

import asyncio
import logging
import os
from datetime import datetime
from typing import Any, Dict, List, Optional

import httpx

from kthena.runtime.events import (
    EventType,
    VLLMAllBlocksClearedEvent,
    VLLMBlockRemovedEvent,
    VLLMBlockStoredEvent,
    VLLMEventData,
    get_event_publisher,
)

logger = logging.getLogger(__name__)

DEFAULT_POLL_INTERVAL = 1.0
DEFAULT_EVENTS_PATH = "/kv_cache_events"


def _token_ids_from_block(block: Dict[str, Any]) -> List[int]:
    tokens = block.get("tokens") or []
    ids: List[int] = []
    for tok in tokens:
        if isinstance(tok, dict) and "token_id" in tok:
            ids.append(int(tok["token_id"]))
        elif isinstance(tok, int):
            ids.append(tok)
    return ids


class TensorRTLLMKVEventPoller:
    """Background poller for trtllm-serve POST /kv_cache_events."""

    def __init__(
        self,
        engine_base_url: str,
        pod_identifier: str,
        model_name: str,
        client: Optional[httpx.AsyncClient] = None,
        poll_interval: Optional[float] = None,
        events_path: Optional[str] = None,
    ):
        self.engine_base_url = engine_base_url.rstrip("/")
        self.pod_identifier = pod_identifier
        self.model_name = model_name
        self.events_path = events_path or os.getenv(
            "TRTLLM_KV_EVENTS_PATH", DEFAULT_EVENTS_PATH
        )
        self.poll_interval = (
            poll_interval
            if poll_interval is not None
            else float(os.getenv("TRTLLM_KV_EVENTS_POLL_INTERVAL", DEFAULT_POLL_INTERVAL))
        )
        self._owns_client = client is None
        self.client = client or httpx.AsyncClient(timeout=httpx.Timeout(5.0))
        self.event_publisher = get_event_publisher()
        self.running = False
        self._task: Optional[asyncio.Task] = None

    async def start(self) -> None:
        if self.running:
            return
        self.running = True
        self._task = asyncio.create_task(self._run())
        logger.info(
            "TensorRT-LLM KV event poller started (%s%s, interval=%.2fs)",
            self.engine_base_url,
            self.events_path,
            self.poll_interval,
        )
        await self._task

    async def stop(self) -> None:
        self.running = False
        if self._task:
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            self._task = None
        if self._owns_client:
            await self.client.aclose()
        logger.info("TensorRT-LLM KV event poller stopped")

    async def _run(self) -> None:
        url = f"{self.engine_base_url}{self.events_path}"
        while self.running:
            try:
                resp = await self.client.post(url, json={})
                resp.raise_for_status()
                events = resp.json()
                if isinstance(events, list) and events:
                    await self._handle_events(events)
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warning("TensorRT-LLM KV event poll failed: %s", e)
            try:
                await asyncio.sleep(self.poll_interval)
            except asyncio.CancelledError:
                break

    async def _handle_events(self, events: List[Dict[str, Any]]) -> None:
        now = datetime.now()
        for item in events:
            data = item.get("data") or {}
            event_type = data.get("type")
            try:
                if event_type == "stored":
                    await self._publish_stored(data, now)
                elif event_type == "removed":
                    await self._publish_removed(data, now)
                elif event_type in ("created", "updated"):
                    # created/updated do not map to redis block presence; ignore.
                    continue
                else:
                    logger.debug("Ignoring TensorRT-LLM KV event type=%s", event_type)
            except Exception as e:
                logger.error("Failed to handle TensorRT-LLM KV event %s: %s", event_type, e)

    async def _publish_stored(self, data: Dict[str, Any], timestamp: datetime) -> None:
        blocks = data.get("blocks") or []
        parent = data.get("parent_hash")
        # Publish one vLLM-compatible stored event per block so uneven last
        # blocks still standardize correctly.
        for i, block in enumerate(blocks):
            if not isinstance(block, dict):
                continue
            block_hash = block.get("block_hash")
            token_ids = _token_ids_from_block(block)
            if block_hash is None or not token_ids:
                continue
            vllm_event = VLLMBlockStoredEvent(
                block_hashes=[int(block_hash)],
                parent_block_hash=int(parent) if (i == 0 and parent is not None) else None,
                token_ids=token_ids,
                block_size=len(token_ids),
            )
            await self.event_publisher.publish(
                VLLMEventData(
                    event_type=EventType.VLLM_BLOCK_STORED,
                    timestamp=timestamp,
                    model_name=self.model_name,
                    pod_identifier=self.pod_identifier,
                    vllm_event=vllm_event,
                )
            )

    async def _publish_removed(self, data: Dict[str, Any], timestamp: datetime) -> None:
        # TRT-LLM removed payloads may list block hashes directly or nested.
        hashes: List[int] = []
        if isinstance(data.get("block_hashes"), list):
            hashes = [int(h) for h in data["block_hashes"]]
        else:
            for block in data.get("blocks") or []:
                if isinstance(block, dict) and block.get("block_hash") is not None:
                    hashes.append(int(block["block_hash"]))
                elif isinstance(block, int):
                    hashes.append(block)
        if not hashes:
            # Some builds clear via a dedicated cleared event; treat empty removed
            # with explicit clear flag if present.
            if data.get("type") == "cleared" or data.get("all_blocks"):
                await self.event_publisher.publish(
                    VLLMEventData(
                        event_type=EventType.VLLM_ALL_BLOCKS_CLEARED,
                        timestamp=timestamp,
                        model_name=self.model_name,
                        pod_identifier=self.pod_identifier,
                        vllm_event=VLLMAllBlocksClearedEvent(),
                    )
                )
            return
        await self.event_publisher.publish(
            VLLMEventData(
                event_type=EventType.VLLM_BLOCK_REMOVED,
                timestamp=timestamp,
                model_name=self.model_name,
                pod_identifier=self.pod_identifier,
                vllm_event=VLLMBlockRemovedEvent(block_hashes=hashes),
            )
        )


def get_tensorrtllm_kv_event_poller(
    engine_base_url: str,
    pod_identifier: str,
    model_name: str,
    client: Optional[httpx.AsyncClient] = None,
) -> TensorRTLLMKVEventPoller:
    return TensorRTLLMKVEventPoller(
        engine_base_url=engine_base_url,
        pod_identifier=pod_identifier,
        model_name=model_name,
        client=client,
    )
