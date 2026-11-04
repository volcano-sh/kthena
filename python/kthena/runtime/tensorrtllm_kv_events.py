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

Polling cadence follows NVIDIA Dynamo's TRT-LLM publisher: when the drain
returns events, sleep resets to a short minimum; when empty, sleep grows
with exponential backoff up to a cap (default 5ms → 20ms, factor 1.5).
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

# Aligned with ai-dynamo/dynamo trtllm publisher.py KV event polling.
DEFAULT_MIN_SLEEP_SEC = 0.005
DEFAULT_MAX_SLEEP_SEC = 0.02
DEFAULT_BACKOFF_FACTOR = 1.5
DEFAULT_EVENTS_PATH = "/kv_cache_events"


def _env_float(name: str, default: float) -> float:
    raw = os.getenv(name)
    if raw is None or raw == "":
        return default
    return float(raw)


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
        min_sleep: Optional[float] = None,
        max_sleep: Optional[float] = None,
        backoff_factor: Optional[float] = None,
        events_path: Optional[str] = None,
    ):
        self.engine_base_url = engine_base_url.rstrip("/")
        self.pod_identifier = pod_identifier
        self.model_name = model_name
        self.events_path = events_path or os.getenv(
            "TRTLLM_KV_EVENTS_PATH", DEFAULT_EVENTS_PATH
        )

        self.min_sleep = (
            min_sleep
            if min_sleep is not None
            else _env_float("TRTLLM_KV_EVENTS_MIN_SLEEP_SEC", DEFAULT_MIN_SLEEP_SEC)
        )
        self.max_sleep = (
            max_sleep
            if max_sleep is not None
            else _env_float("TRTLLM_KV_EVENTS_MAX_SLEEP_SEC", DEFAULT_MAX_SLEEP_SEC)
        )
        self.backoff_factor = (
            backoff_factor
            if backoff_factor is not None
            else _env_float(
                "TRTLLM_KV_EVENTS_BACKOFF_FACTOR", DEFAULT_BACKOFF_FACTOR
            )
        )
        if self.min_sleep <= 0 or self.max_sleep < self.min_sleep:
            raise ValueError(
                f"invalid KV poll sleep bounds: min={self.min_sleep}, max={self.max_sleep}"
            )
        if self.backoff_factor < 1.0:
            raise ValueError(
                f"invalid KV poll backoff_factor={self.backoff_factor}, must be >= 1"
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
            "TensorRT-LLM KV event poller started (%s%s, "
            "min_sleep=%.3fs max_sleep=%.3fs backoff=%.2f)",
            self.engine_base_url,
            self.events_path,
            self.min_sleep,
            self.max_sleep,
            self.backoff_factor,
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

    def _next_idle_sleep(self, sleep_s: float) -> float:
        """Grow idle sleep toward max_sleep (Dynamo backoff)."""
        return min(self.max_sleep, sleep_s * self.backoff_factor)

    async def _run(self) -> None:
        url = f"{self.engine_base_url}{self.events_path}"
        sleep_s = self.min_sleep
        while self.running:
            had_data = False
            try:
                resp = await self.client.post(url, json={})
                resp.raise_for_status()
                events = resp.json()
                if isinstance(events, list) and events:
                    had_data = True
                    await self._handle_events(events)
            except asyncio.CancelledError:
                break
            except Exception as e:
                logger.warning("TensorRT-LLM KV event poll failed: %s", e)

            if had_data:
                # Dynamo: data resets backoff to the minimum.
                sleep_s = self.min_sleep
                next_sleep = sleep_s
            else:
                next_sleep = sleep_s
                sleep_s = self._next_idle_sleep(sleep_s)

            try:
                await asyncio.sleep(next_sleep)
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
