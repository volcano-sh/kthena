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
import json
import logging
import time
from typing import AsyncIterator, Awaitable, Callable, Dict, List, Optional, Set, Tuple

from kthena.runtime.kv_cache_manager import standardize_block_hashes

logger = logging.getLogger(__name__)

KV_EVENT_STORED = "stored"
KV_EVENT_REMOVED = "removed"
KV_EVENT_CLEARED = "cleared"

# Interval of the empty heartbeat lines sent on an otherwise idle stream. The
# router closes a stream that stays silent for 30s, see kvStreamIdleTimeout in
# pkg/kthena-router/scheduler/plugins/kvcache_subscription.go.
STREAM_HEARTBEAT_SECONDS = 10.0

# Upper bound of messages queued per router stream. When a router cannot keep
# up and its queue overflows, its stream is ended; the router opens a new one,
# which starts with the full block set again.
STREAM_QUEUE_MAXSIZE = 1024

# Upper bound of block hashes per message, which keeps every message far below
# the router's 4 MiB limit per message.
MAX_BLOCKS_PER_MESSAGE = 4096

# Upper bound of concurrently open router streams. Every router replica holds
# one stream. A stream counts until its response has exited, including after
# it was replaced or ended.
MAX_ROUTER_STREAMS = 128

# Time an ended stream's response gets to finish on its own before it is
# cancelled, e.g. because the client does not read it and sending blocks.
STREAM_ABORT_GRACE_SECONDS = 1.0


class RouterStreamUnavailable(Exception):
    """Raised when a router stream cannot be opened: MAX_ROUTER_STREAMS
    streams are open already, or the runtime is shutting down."""


class RouterStream:
    """The KV events stream of one registered router."""

    def __init__(self, router_id: str,
                 initial_blocks: List[Tuple[str, Dict[int, int]]]):
        self.router_id = router_id
        # Copy of the block set when the router registered, sent first.
        self.initial_blocks = initial_blocks
        # Encoded messages queued after the initial block set; None ends the
        # stream.
        self.queue: asyncio.Queue = asyncio.Queue(maxsize=STREAM_QUEUE_MAXSIZE)
        self.closed = False
        # Set by close(); the response is cancelled if it does not end within
        # STREAM_ABORT_GRACE_SECONDS.
        self.aborted = asyncio.Event()

    def close(self) -> None:
        """End the stream right away, discarding what was not sent yet."""
        if self.closed:
            return
        self.closed = True
        self.initial_blocks = []
        while not self.queue.empty():
            self.queue.get_nowait()
        self.queue.put_nowait(None)
        self.aborted.set()


def _encode(model_name: str, events: List[dict]) -> bytes:
    return (json.dumps({"model_name": model_name, "events": events},
                       separators=(",", ":")) + "\n").encode()


def _chunks(items: List):
    size = MAX_BLOCKS_PER_MESSAGE
    for i in range(0, len(items), size):
        yield items[i:i + size]


class MemoryKVCacheManager:
    """KV cache manager that streams standardized block hashes directly to
    registered router instances instead of writing them to Redis.

    Implements the same interface as VLLMKVCacheRedisManager
    (add_blocks / remove_blocks / clear_all_blocks) so it can back
    VLLMKVCacheEventHandler and SGLangKVCacheEventHandler.

    Unlike Redis mode, there is no shared, durable store here: every router
    process keeps its own private in-memory copy of the index. In Redis mode
    the sidecar writes each event once into Redis and can forget it, because
    Redis itself is the authoritative state that any router reads on demand.
    In memory mode this class has to retain that authoritative state (_blocks)
    itself, so it can send it to every router that registers.

    A router registers by opening a long-lived request; the response is its
    stream of KV events (see register in app.py). The stream starts with the
    full block set and continues with the stored/removed/cleared deltas, so
    the router knows which pod the events belong to from the connection
    alone. Each stream has its own bounded queue, so a slow router delays
    neither the other routers nor the engine event consumer; a stream whose
    queue overflows is ended and the router registers again.
    """

    def __init__(self):
        # engine hash -> standardized hash for continuations and removals.
        self.hash_mapping: Dict[int, int] = {}
        # model name -> {std_hash: number of engine hashes currently mapped to
        # it}. Identical prefixes under different LoRA adapters can have
        # distinct engine hashes yet the same standardized hash, so a
        # standardized hash may only be dropped once its last engine-hash
        # reference is removed.
        self._std_refs: Dict[str, Dict[int, int]] = {}
        # model name -> {std_hash: unix seconds when stored}, the block set
        # sent to every router that registers.
        self._blocks: Dict[str, Dict[int, int]] = {}
        # router id -> its open stream.
        self._streams: Dict[str, RouterStream] = {}
        # Streams whose responses have not exited yet, counted against
        # MAX_ROUTER_STREAMS.
        self._live: Set[RouterStream] = set()
        # Set by close(); no stream is opened afterwards.
        self._closed = False

    def open_stream(self, router_id: str) -> RouterStream:
        """Register a router and return its stream, whose response must be
        sent through serve().

        A router that registers again, e.g. after its connection broke,
        replaces its previous stream. The block set is captured here, together
        with registering the stream for deltas, so the stream neither misses
        nor repeats a mutation.
        """
        if self._closed:
            raise RouterStreamUnavailable("the runtime is shutting down")
        previous = self._streams.pop(router_id, None)
        if previous is not None:
            previous.close()
        if len(self._live) >= MAX_ROUTER_STREAMS:
            raise RouterStreamUnavailable(
                f"{MAX_ROUTER_STREAMS} router streams are open already")
        stream = RouterStream(router_id, [
            (model_name, dict(blocks))
            for model_name, blocks in self._blocks.items() if blocks])
        self._streams[router_id] = stream
        self._live.add(stream)
        logger.info(f"Router registered: id={router_id}")
        return stream

    def _close_stream(self, stream: RouterStream) -> None:
        stream.close()
        if self._streams.get(stream.router_id) is stream:
            del self._streams[stream.router_id]
            logger.info(f"Router stream ended: id={stream.router_id}")

    async def serve(self, stream: RouterStream,
                    respond: Callable[[], Awaitable[None]]) -> None:
        """Run respond, which sends the response of stream, and cancel it if
        it does not end within STREAM_ABORT_GRACE_SECONDS after the stream was
        closed. Closing a stream alone cannot end a response whose client
        does not read, because sending it blocks."""
        response = asyncio.ensure_future(respond())
        aborted = asyncio.ensure_future(stream.aborted.wait())
        try:
            await asyncio.wait({response, aborted},
                               return_when=asyncio.FIRST_COMPLETED)
            if not response.done():
                await asyncio.wait({response}, timeout=STREAM_ABORT_GRACE_SECONDS)
        finally:
            aborted.cancel()
            cancelled = not response.done()
            if cancelled:
                logger.warning(
                    f"Router {stream.router_id} does not read its stream; "
                    f"aborting the response")
                response.cancel()
            try:
                await response
            except asyncio.CancelledError:
                if not cancelled:
                    raise
            finally:
                self._close_stream(stream)
                self._live.discard(stream)

    async def iter_stream(self, stream: RouterStream) -> AsyncIterator[bytes]:
        """Yield the encoded messages of stream until it is closed."""
        initial_blocks, stream.initial_blocks = stream.initial_blocks, []
        for model_name, blocks in initial_blocks:
            # Preserve original per-block store times; re-stamping them
            # would defeat the engine-restart freshness filter.
            for hashes in _chunks(list(blocks)):
                if stream.closed:
                    return
                yield _encode(model_name, [{
                    "type": KV_EVENT_STORED,
                    "block_hashes": hashes,
                    "timestamps": [blocks[h] for h in hashes],
                }])
        # The stream lives on; do not keep the block set copy with it.
        initial_blocks = blocks = hashes = None
        while True:
            try:
                message = await asyncio.wait_for(
                    stream.queue.get(), STREAM_HEARTBEAT_SECONDS)
            except asyncio.TimeoutError:
                yield b"\n"
                continue
            if message is None:
                return
            yield message

    async def close(self) -> None:
        """End all router streams; called on shutdown."""
        self._closed = True
        for stream in list(self._live):
            self._close_stream(stream)

    async def add_blocks(self, model_name: str, block_hashes: List[int],
                         pod_identifier: str, token_ids: Optional[List[int]] = None,
                         parent_block_hash: Optional[int] = None) -> bool:
        if not block_hashes or not model_name or not pod_identifier:
            return not block_hashes
        if not token_ids:
            return True

        parent_std_hash = None
        if parent_block_hash is not None:
            parent_std_hash = self.hash_mapping.get(parent_block_hash)
            if parent_std_hash is None:
                logger.warning("Skipping BlockStored for %s on %s: unknown parent %s",
                               model_name, pod_identifier, parent_block_hash)
                return False
        pairs = standardize_block_hashes(block_hashes, token_ids, parent_std_hash)
        if pairs is None:
            return False

        timestamp = int(time.time())
        model_blocks = self._blocks.setdefault(model_name, {})
        model_refs = self._std_refs.setdefault(model_name, {})
        std_hashes = []
        for engine_hash, std_hash in pairs:
            previous = self.hash_mapping.get(engine_hash)
            if previous != std_hash:
                if previous is not None:
                    self._release_std_hash(model_name, previous)
                model_refs[std_hash] = model_refs.get(std_hash, 0) + 1
            self.hash_mapping[engine_hash] = std_hash
            model_blocks[std_hash] = timestamp
            if std_hash not in std_hashes:
                std_hashes.append(std_hash)

        self._publish(model_name, KV_EVENT_STORED, std_hashes, timestamp)
        logger.info(
            f"Runtime memory stream - Model: {model_name}, Pod: {pod_identifier}, "
            f"Count: {len(std_hashes)}")
        return True

    def _release_std_hash(self, model_name: str, std_hash: int) -> bool:
        """Drop one engine-hash reference; returns True when it was the last
        one and the standardized hash left the index."""
        model_refs = self._std_refs.get(model_name, {})
        remaining = model_refs.get(std_hash, 0) - 1
        if remaining > 0:
            model_refs[std_hash] = remaining
            return False
        model_refs.pop(std_hash, None)
        self._blocks.get(model_name, {}).pop(std_hash, None)
        return True

    async def remove_blocks(self, model_name: str, block_hashes: List[int],
                            pod_identifier: str) -> int:
        if not block_hashes or not model_name or not pod_identifier:
            return 0

        removed = 0
        std_hashes = []
        for engine_hash in block_hashes:
            std_hash = self.hash_mapping.pop(engine_hash, None)
            if std_hash is None:
                continue
            removed += 1
            # Only announce the removal once the last engine hash referencing
            # this standardized hash is gone; other cached blocks with the
            # same token content may still exist.
            if self._release_std_hash(model_name, std_hash):
                std_hashes.append(std_hash)

        if not removed:
            return 0

        if std_hashes:
            self._publish(model_name, KV_EVENT_REMOVED, std_hashes)
        logger.info(
            f"Removed {len(std_hashes)} blocks for model {model_name}, pod {pod_identifier}")
        return removed

    async def clear_all_blocks(self, model_name: str, pod_identifier: str) -> int:
        if not model_name or not pod_identifier:
            return 0

        cleared = len(self._blocks.get(model_name, {}))
        self._blocks.pop(model_name, None)
        self._std_refs.clear()
        self.hash_mapping.clear()

        self._publish(model_name, KV_EVENT_CLEARED)
        logger.info(
            f"Cleared {cleared} blocks for model {model_name}, pod {pod_identifier}")
        return cleared

    def _publish(self, model_name: str, event_type: str,
                 block_hashes: Optional[List[int]] = None,
                 timestamp: Optional[int] = None) -> None:
        """Queue a delta on every open router stream."""
        if not self._streams:
            logger.debug("No routers registered, skipping KV event stream")
            return
        if block_hashes is None:
            messages = [_encode(model_name, [{"type": event_type}])]
        else:
            messages = []
            for hashes in _chunks(block_hashes):
                event = {"type": event_type, "block_hashes": hashes}
                if timestamp is not None:
                    event["timestamp"] = timestamp
                messages.append(_encode(model_name, [event]))
        for stream in list(self._streams.values()):
            for message in messages:
                try:
                    stream.queue.put_nowait(message)
                except asyncio.QueueFull:
                    # The router cannot keep up; end its stream so it
                    # registers again and receives the full block set.
                    logger.warning(
                        f"KV event queue of router {stream.router_id} is "
                        f"full; ending its stream")
                    self._close_stream(stream)
                    break


_memory_kv_manager: Optional[MemoryKVCacheManager] = None


def get_memory_kv_manager() -> MemoryKVCacheManager:
    global _memory_kv_manager
    if _memory_kv_manager is None:
        _memory_kv_manager = MemoryKVCacheManager()
    return _memory_kv_manager
