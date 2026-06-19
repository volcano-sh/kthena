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
from abc import ABC, abstractmethod
from dataclasses import dataclass
from datetime import datetime
from enum import Enum
from typing import Any, Dict, List, Optional

logger = logging.getLogger(__name__)


class EventType(Enum):
    VLLM_BLOCK_STORED = "vllm_block_stored"
    VLLM_BLOCK_REMOVED = "vllm_block_removed"
    VLLM_ALL_BLOCKS_CLEARED = "vllm_all_blocks_cleared"
    SGLANG_BLOCK_STORED = "sglang_block_stored"
    SGLANG_BLOCK_REMOVED = "sglang_block_removed"
    SGLANG_ALL_BLOCKS_CLEARED = "sglang_all_blocks_cleared"


@dataclass
class EventData:
    event_type: EventType
    timestamp: datetime
    model_name: str


@dataclass
class VLLMBlockStoredEvent:
    block_hashes: List[int]
    parent_block_hash: Optional[int] = None
    token_ids: Optional[List[int]] = None
    block_size: Optional[int] = None
    lora_id: Optional[int] = None


@dataclass
class VLLMBlockRemovedEvent:
    block_hashes: List[int]


@dataclass
class VLLMAllBlocksClearedEvent:
    pass


@dataclass
class VLLMEventData(EventData):
    pod_identifier: Optional[str] = None
    data_parallel_rank: Optional[int] = None
    vllm_event: Optional[Any] = None


@dataclass
class SGLangBlockStoredEvent:
    block_hashes: List[int]
    parent_block_hash: Optional[int] = None
    token_ids: Optional[List[int]] = None
    block_size: Optional[int] = None
    lora_id: Optional[int] = None
    medium: Optional[str] = None


@dataclass
class SGLangBlockRemovedEvent:
    block_hashes: List[int]
    medium: Optional[str] = None


@dataclass
class SGLangAllBlocksClearedEvent:
    pass


@dataclass
class SGLangEventData(EventData):
    pod_identifier: Optional[str] = None
    attn_dp_rank: Optional[int] = None
    sglang_event: Optional[Any] = None


class EventHandler(ABC):

    @abstractmethod
    async def handle(self, event_data: EventData) -> None:
        pass


class EventPublisher:

    def __init__(self):
        self._subscribers: Dict[EventType, List[EventHandler]] = {}
        self._global_subscribers: List[EventHandler] = []
        self._event_queue: asyncio.Queue = asyncio.Queue()
        self._running = False
        self._worker_task: Optional[asyncio.Task] = None

    def subscribe(self, event_type: EventType, handler: EventHandler) -> None:
        if event_type not in self._subscribers:
            self._subscribers[event_type] = []
        self._subscribers[event_type].append(handler)

    async def publish(self, event_data: EventData) -> None:
        if not self._running:
            await self._process_event(event_data)
            return

        await self._event_queue.put(event_data)

    async def start(self) -> None:
        if self._running:
            return

        self._running = True
        loop = asyncio.get_running_loop()
        self._worker_task = loop.create_task(self._worker())

    async def stop(self) -> None:
        if not self._running:
            return

        self._running = False

        try:
            await self._event_queue.put(None)
        except (asyncio.QueueFull, RuntimeError):
            pass

        if self._worker_task:
            try:
                await asyncio.wait_for(self._worker_task, timeout=2.0)
            except asyncio.TimeoutError:
                self._worker_task.cancel()
                try:
                    await self._worker_task
                except asyncio.CancelledError:
                    pass

    async def _worker(self) -> None:
        while self._running:
            try:
                if self._event_queue.qsize() > 0:
                    while not self._event_queue.empty() and self._running:
                        try:
                            event_data = self._event_queue.get_nowait()

                            if event_data is None:
                                self._event_queue.task_done()
                                return

                            await self._process_event(event_data)
                            self._event_queue.task_done()

                        except asyncio.QueueEmpty:
                            break
                        except (RuntimeError, ValueError, TypeError) as e:
                            logger.error(f"Error processing event: {e}")
                        except Exception as e:
                            logger.error(f"Unexpected error processing event: {e}", exc_info=True)
                else:
                    await asyncio.sleep(0.01)

            except asyncio.CancelledError:
                raise
            except (RuntimeError, ValueError, TypeError) as e:
                logger.error(f"Error in worker: {e}")
                await asyncio.sleep(0.01)
            except Exception as e:
                logger.error(f"Unexpected error in worker: {e}", exc_info=True)
                await asyncio.sleep(0.01)

    async def _process_event(self, event_data: EventData) -> None:
        handlers = []

        if event_data.event_type in self._subscribers:
            handlers.extend(self._subscribers[event_data.event_type])

        handlers.extend(self._global_subscribers)

        if handlers:
            tasks = [handler.handle(event_data) for handler in handlers]
            try:
                results = await asyncio.gather(*tasks, return_exceptions=True)
                for i, result in enumerate(results):
                    if isinstance(result, Exception):
                        logger.error(f"Handler {i} failed for event {event_data.event_type.value}: {result}")
            except (RuntimeError, ValueError, TypeError) as e:
                logger.error(f"Error in event handlers: {e}")
            except Exception as e:
                logger.error(f"Unexpected error in event handlers: {e}", exc_info=True)


_event_publisher: Optional[EventPublisher] = None


def get_event_publisher() -> EventPublisher:
    global _event_publisher
    if _event_publisher is None:
        _event_publisher = EventPublisher()
    return _event_publisher
