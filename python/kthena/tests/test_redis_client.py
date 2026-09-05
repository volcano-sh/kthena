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

import pytest
from redis.exceptions import ConnectionError

from kthena.runtime.redis_client import RedisClient


class _FakeRedis:
    """One instance per connection, so tests can tell which one was called."""

    def __init__(self, generation, fail=False):
        self.generation = generation
        self.fail = fail
        self.scan_calls = 0
        self.keys_calls = 0

    async def scan(self, cursor=0, match=None, count=None):
        self.scan_calls += 1
        if self.fail:
            raise ConnectionError(f"generation {self.generation} is down")
        return 0, [f"key-{self.generation}"]

    async def keys(self, pattern):
        self.keys_calls += 1
        if self.fail:
            raise ConnectionError(f"generation {self.generation} is down")
        return [f"key-{self.generation}"]


def _client_failing_first_connection():
    """A RedisClient whose connect() installs a new _FakeRedis each time.

    The first connection always fails, later ones succeed, which is what a
    dropped connection followed by a successful reconnect looks like.
    """
    client = RedisClient()
    client.config.retry_delay = 0
    clients = []

    async def fake_connect():
        if client._connected:
            return
        redis = _FakeRedis(len(clients), fail=len(clients) == 0)
        clients.append(redis)
        client._client = redis
        client._connected = True

    client.connect = fake_connect
    return client, clients


@pytest.mark.asyncio
async def test_scan_keys_retries_against_the_reconnected_client():
    client, clients = _client_failing_first_connection()
    await client.connect()

    result = await client.scan_keys("prefix*")

    assert result == ["key-1"]
    assert clients[0].scan_calls == 1
    assert clients[1].scan_calls == 1


@pytest.mark.asyncio
async def test_keys_retries_against_the_reconnected_client():
    client, clients = _client_failing_first_connection()
    await client.connect()

    result = await client.keys("prefix*")

    assert result == ["key-1"]
    assert clients[0].keys_calls == 1
    assert clients[1].keys_calls == 1


@pytest.mark.asyncio
async def test_scan_keys_connects_on_demand():
    client, clients = _client_failing_first_connection()

    result = await client.scan_keys("prefix*")

    assert result == ["key-1"]


@pytest.mark.asyncio
async def test_keys_connects_on_demand():
    client, clients = _client_failing_first_connection()

    result = await client.keys("prefix*")

    assert result == ["key-1"]


@pytest.mark.asyncio
async def test_scan_keys_returns_empty_when_every_attempt_fails():
    client = RedisClient()
    client.config.retry_delay = 0

    async def fake_connect():
        client._client = _FakeRedis(0, fail=True)
        client._connected = True

    client.connect = fake_connect

    assert await client.scan_keys("prefix*") == []
    assert await client.keys("prefix*") == []
