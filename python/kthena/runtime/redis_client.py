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
import os
from typing import List, Optional, Any

import redis.asyncio as redis
from redis.asyncio import Redis, ConnectionPool
from redis.exceptions import RedisError, ConnectionError, TimeoutError

logger = logging.getLogger(__name__)

# COUNT hint per SCAN iteration. Bounds the work the server does per round rather
# than the number of keys returned, which Redis is free to vary.
SCAN_BATCH_SIZE = 500


class RedisConfig:
    def __init__(self):
        self.host = os.getenv("REDIS_HOST", "redis-server")
        self.port = int(os.getenv("REDIS_PORT", "6379"))
        self.db = int(os.getenv("REDIS_DB", "0"))
        self.password = os.getenv("REDIS_PASSWORD")
        self.max_connections = 20
        self.socket_timeout = 5.0
        self.socket_connect_timeout = 5.0
        self.health_check_interval = 30
        self.max_retries = 3
        self.retry_delay = 0.1


class RedisClient:
    def __init__(self, config: Optional[RedisConfig] = None):
        self.config = config or RedisConfig()
        self._client: Optional[Redis] = None
        self._connection_pool: Optional[ConnectionPool] = None
        self._connected = False
        self._connection_lock = asyncio.Lock()

    async def connect(self) -> None:
        if self._connected:
            return

        async with self._connection_lock:
            if self._connected:
                return

            try:
                self._connection_pool = ConnectionPool(
                    host=self.config.host,
                    port=self.config.port,
                    db=self.config.db,
                    password=self.config.password,
                    max_connections=self.config.max_connections,
                    socket_timeout=self.config.socket_timeout,
                    socket_connect_timeout=self.config.socket_connect_timeout,
                    decode_responses=True,
                    health_check_interval=self.config.health_check_interval
                )

                self._client = redis.Redis(connection_pool=self._connection_pool)
                await self._client.ping()
                self._connected = True
                logger.info(f"Connected to Redis at {self.config.host}:{self.config.port}")

            except ConnectionError as e:
                logger.error(f"Failed to connect to Redis: {e}")
                await self._cleanup_connection()
                raise
            except Exception as e:
                logger.error(f"Unexpected error connecting to Redis: {e}")
                await self._cleanup_connection()
                raise

    async def _cleanup_connection(self) -> None:
        if self._connection_pool:
            await self._connection_pool.disconnect()
            self._connection_pool = None
        self._client = None
        self._connected = False

    async def disconnect(self) -> None:
        async with self._connection_lock:
            if self._connected:
                await self._cleanup_connection()
                logger.info("Disconnected from Redis")

    async def _ensure_connected(self) -> None:
        if not self._connected:
            await self.connect()

    async def _execute_with_retry(self, operation, *args, **kwargs) -> Any:
        # operation must resolve self._client when called. A bound method would
        # keep addressing the client that the reconnect below replaces.
        last_error = None
        for attempt in range(self.config.max_retries + 1):
            try:
                await self._ensure_connected()
                return await operation(*args, **kwargs)
            except (ConnectionError, TimeoutError) as e:
                last_error = e
                if attempt == self.config.max_retries:
                    logger.error(f"Redis operation failed after {self.config.max_retries} retries: {e}")
                    raise e
                self._connected = False
                await asyncio.sleep(self.config.retry_delay * (attempt + 1))
            except RedisError as e:
                logger.error(f"Redis error: {e}")
                raise e

        raise last_error or RuntimeError("Unexpected error in retry loop")

    async def keys(self, pattern: str) -> List[str]:
        try:
            result = await self._execute_with_retry(
                lambda *a, **kw: self._client.keys(*a, **kw), pattern
            )
            return result or []
        except (RedisError, ConnectionError, TimeoutError):
            return []

    async def scan_keys(self, pattern: str, count: int = SCAN_BATCH_SIZE) -> List[str]:
        """Return keys matching pattern, walking the keyspace with SCAN.

        KEYS blocks the server until it has walked the whole keyspace, so it is not
        safe on a Redis shared with the router. SCAN bounds the work per round instead.
        A round may return any number of keys, including none while the cursor is still
        non-zero, and a key may be reported more than once, so results are deduped.
        """
        keys: List[str] = []
        cursor = 0
        try:
            while True:
                cursor, batch = await self._execute_with_retry(
                    lambda **kw: self._client.scan(**kw),
                    cursor=cursor, match=pattern, count=count,
                )
                keys.extend(batch)
                if cursor == 0:
                    break
        except (RedisError, ConnectionError, TimeoutError):
            return []
        return list(dict.fromkeys(keys))

    async def get_connection(self):
        await self._ensure_connected()
        return self._client


_redis_client: Optional[RedisClient] = None


def get_redis_client() -> RedisClient:
    global _redis_client
    if _redis_client is None:
        _redis_client = RedisClient()
    return _redis_client
