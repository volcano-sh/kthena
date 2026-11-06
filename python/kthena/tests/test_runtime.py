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
import asyncio
import json
import socket
import threading
import time
from types import SimpleNamespace

import httpx
import pytest
import uvicorn
from fastapi.testclient import TestClient

from kthena.downloader.app import load_config
from kthena.runtime import app as runtime_app
from kthena.runtime import memory_kv_manager as mkm
from kthena.runtime.app import (
    KV_SYNC_MODE_MEMORY,
    _Server,
    create_application,
    get_app_state,
)
from kthena.runtime.kv_cache_manager import compute_standardized_hash
from kthena.runtime.memory_kv_manager import MemoryKVCacheManager


class _FakeAsyncResponse:
    def __init__(self, status_code: int = 200, payload: dict | None = None, text: str = "OK"):
        self.status_code = status_code
        self._payload = payload or {"status": "ok"}
        self.text = text

    def json(self):
        return self._payload


class _FakeAsyncClient:
    def __init__(self, *args, **kwargs):
        pass

    async def post(self, url: str, json: dict | None = None):
        return _FakeAsyncResponse(status_code=200, payload={"url": url, "json": json})

    async def aclose(self):
        return None


class _FakeSyncResponse:
    def __init__(self, status_code: int = 200, payload: dict | None = None, text: str = "OK"):
        self.status_code = status_code
        self._payload = payload or {"status": "ok"}
        self.text = text

    def json(self):
        return self._payload


class _FakeSyncClient:
    def __init__(self, *args, **kwargs):
        pass

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc, tb):
        return False

    def post(self, url: str, json: dict | None = None):
        # Simulate engine endpoint success
        return _FakeSyncResponse(status_code=200, payload={"url": url, "json": json})


def _make_args():
    return SimpleNamespace(
        engine="vllm",
        host="127.0.0.1",
        port=0,
        engine_base_url="http://engine.local:8000",
        engine_metrics_path="/metrics",
        pod="pod-1.ns",
        model="test-model",
    )


def test_load_lora_adapter_success(monkeypatch):
    # Arrange: create app and inject fake async client and downloader
    app = create_application(_make_args())

    state = get_app_state(app)
    monkeypatch.setattr("httpx.AsyncClient", _FakeAsyncClient)
    state.client = _FakeAsyncClient()

    monkeypatch.setattr("kthena.runtime.app.download_model", lambda *args, **kwargs: None)
    monkeypatch.setattr("httpx.Client", _FakeSyncClient)

    with TestClient(app) as client:
        body = {
            "lora_name": "lora-sql",
            "source": "s3://bucket/lora-sql",
            "output_dir": "/tmp/lora",
            "config": {},
            "max_workers": 1,
            "async_download": False,
        }

        # Act
        resp = client.post("/v1/load_lora_adapter", json=body)

        # Assert
        assert resp.status_code == 200
        data = resp.json()
        assert data


def test_unload_lora_adapter_success(monkeypatch):
    # Arrange: create app and inject fake async client
    app = create_application(_make_args())

    state = get_app_state(app)
    monkeypatch.setattr("httpx.AsyncClient", _FakeAsyncClient)
    state.client = _FakeAsyncClient()

    with TestClient(app) as client:
        body = {"lora_name": "lora-sql"}

        # Act
        resp = client.post("/v1/unload_lora_adapter", json=body)

        # Assert
        assert resp.status_code == 200
        data = resp.json()
        assert data


def test_load_lora_adapter_async_download_success(monkeypatch):
    # Arrange: create app, inject clients, and patch download + sync client
    app = create_application(_make_args())
    state = get_app_state(app)
    monkeypatch.setattr("httpx.AsyncClient", _FakeAsyncClient)
    state.client = _FakeAsyncClient()

    monkeypatch.setattr("kthena.runtime.app.download_model", lambda *args, **kwargs: None)
    monkeypatch.setattr("httpx.Client", _FakeSyncClient)

    with TestClient(app) as client:
        body = {
            "lora_name": "lora-sql",
            "source": "s3://bucket/lora-sql",
            "output_dir": "/tmp/lora",
            "config": {},
            "max_workers": 1,
            "async_download": True,
        }

        # Act
        resp = client.post("/v1/load_lora_adapter", json=body)

        # Assert (background task returns 202 Accepted)
        assert resp.status_code == 202


@pytest.mark.parametrize("path, extra_body", [
    ("/v1/download_model", {}),
    ("/v1/load_lora_adapter", {"lora_name": "lora-sql"}),
])
def test_download_endpoints_pass_object_config_to_downloader(monkeypatch, path, extra_body):
    # Arrange: record the config the endpoint hands to the downloader
    monkeypatch.delenv("HF_AUTH_TOKEN", raising=False)
    app = create_application(_make_args())
    state = get_app_state(app)
    monkeypatch.setattr("httpx.AsyncClient", _FakeAsyncClient)
    state.client = _FakeAsyncClient()
    monkeypatch.setattr("httpx.Client", _FakeSyncClient)

    received = []
    monkeypatch.setattr(
        "kthena.runtime.app.download_model",
        lambda source, output_dir, config, max_workers: received.append(config),
    )

    with TestClient(app) as client:
        body = {
            "source": "hf://org/repo",
            "output_dir": "/tmp/model",
            "config": {"hf_token": "from-request"},
            **extra_body,
        }

        # Act
        resp = client.post(path, json=body)

    # Assert
    assert resp.status_code == 200
    assert received == [{"hf_token": "from-request"}]


@pytest.mark.parametrize("config, expected", [
    (None, {}),
    ("", {}),
    ({}, {}),
    ({"hf_token": "from-config"}, {"hf_token": "from-config"}),
    ('{"hf_token": "from-config"}', {"hf_token": "from-config"}),
])
def test_load_config_accepts_supported_forms(monkeypatch, config, expected):
    monkeypatch.delenv("HF_AUTH_TOKEN", raising=False)

    assert load_config(config) == expected


@pytest.mark.parametrize("config", [
    {"hf_token": "from-config"},
    '{"hf_token": "from-config"}',
])
def test_load_config_prefers_explicit_values_over_environment(monkeypatch, config):
    monkeypatch.setenv("HF_AUTH_TOKEN", "from-env")

    assert load_config(config) == {"hf_token": "from-config"}


def test_load_config_fills_missing_values_without_mutating_input(monkeypatch):
    monkeypatch.setenv("HF_AUTH_TOKEN", "from-env")
    config = {"endpoint": "http://storage.local"}

    result = load_config(config)

    assert result == {"endpoint": "http://storage.local", "hf_token": "from-env"}
    assert config == {"endpoint": "http://storage.local"}


@pytest.mark.parametrize("config", [123, ["hf_token"], "[1, 2]", "not json"])
def test_load_config_rejects_values_that_are_not_objects(config):
    with pytest.raises(ValueError):
        load_config(config)


@pytest.mark.asyncio
@pytest.mark.parametrize("mode, body, expected_status", [
    ("redis", {"router_id": "router-a"}, 409),
    (KV_SYNC_MODE_MEMORY, {}, 400),
    (KV_SYNC_MODE_MEMORY, {"router_id": ""}, 400),
    (KV_SYNC_MODE_MEMORY, {"router_id": ["router-a"]}, 400),
    (KV_SYNC_MODE_MEMORY, ["router-a"], 400),
])
async def test_register_router_rejects_invalid_requests(mode, body, expected_status):
    app = create_application(_make_args())
    state = get_app_state(app)
    state.kv_sync_mode = mode
    state.memory_kv_manager = MemoryKVCacheManager()

    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://runtime") as client:
        resp = await client.post("/kvcache/routers/register", json=body)

    assert resp.status_code == expected_status, resp.text
    assert state.memory_kv_manager._streams == {}


def _start_memory_mode_server(monkeypatch):
    """Start the runtime in memory mode on a real server in a background
    thread; returns the server, its loop, thread, port and memory manager."""
    monkeypatch.setenv("KV_EVENT_SYNC_MODE", KV_SYNC_MODE_MEMORY)
    monkeypatch.setattr(mkm, "_memory_kv_manager", None)
    args = _make_args()
    args.pod = None  # no KV event subscribers
    app = create_application(args)
    server = _Server(uvicorn.Config(app, host="127.0.0.1", port=0, log_level="warning"))
    loop = asyncio.new_event_loop()
    thread = threading.Thread(
        target=loop.run_until_complete, args=(server.serve(),), daemon=True)
    thread.start()
    deadline = time.monotonic() + 10
    while not server.started:
        assert thread.is_alive() and time.monotonic() < deadline, "server did not start"
        time.sleep(0.01)
    port = server.servers[0].sockets[0].getsockname()[1]
    return server, loop, thread, port, get_app_state(app).memory_kv_manager


def _register_without_reading(port, router_id):
    """Register a router on a connection whose response is never read."""
    sock = socket.create_connection(("127.0.0.1", port))
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
    body = json.dumps({"router_id": router_id}).encode()
    sock.sendall(
        b"POST /kvcache/routers/register HTTP/1.1\r\nHost: runtime\r\n"
        b"Content-Type: application/json\r\n"
        b"Content-Length: %d\r\n\r\n" % len(body) + body)
    return sock


def _wait_until(condition, timeout=10):
    deadline = time.monotonic() + timeout
    while not condition():
        assert time.monotonic() < deadline, "condition not met in time"
        time.sleep(0.01)


def test_register_router_streams_kv_events_until_shutdown(monkeypatch):
    server, loop, thread, port, manager = _start_memory_mode_server(monkeypatch)

    def run_in_server_loop(coro):
        return asyncio.run_coroutine_threadsafe(coro, loop).result(timeout=5)

    with httpx.Client(base_url=f"http://127.0.0.1:{port}", timeout=5) as client:
        with client.stream("POST", "/kvcache/routers/register",
                           json={"router_id": "router-a"}) as resp:
            assert resp.status_code == 200
            assert resp.headers["content-type"] == "application/x-ndjson"
            lines = resp.iter_lines()
            run_in_server_loop(manager.add_blocks(
                "qwen", [111], "pod-1.ns", list(range(16))))
            message = json.loads(next(line for line in lines if line))
            assert message["model_name"] == "qwen"
            assert message["events"][0]["block_hashes"] == [
                compute_standardized_hash(list(range(16)))]

            # The open stream does not hold up the shutdown.
            server.should_exit = True
            assert list(lines) == []
    thread.join(timeout=10)
    assert not thread.is_alive()
    loop.close()


def test_unread_router_streams_are_aborted(monkeypatch):
    """Streams whose client does not read block on sending. Replacing them
    must not leave their responses running, and they must not hold up the
    shutdown."""
    monkeypatch.setattr(mkm, "STREAM_ABORT_GRACE_SECONDS", 0.1)
    monkeypatch.setattr(runtime_app, "GRACEFUL_SHUTDOWN_TIMEOUT_SECONDS", 1)
    server, loop, thread, port, manager = _start_memory_mode_server(monkeypatch)
    # Large enough an initial block set to fill the socket buffers.
    manager._blocks["qwen"] = {i: 1700000000 for i in range(300_000)}

    sockets = []
    try:
        current = None
        for _ in range(3):
            sockets.append(_register_without_reading(port, "router-a"))
            _wait_until(lambda: manager._streams.get("router-a") not in (None, current))
            current = manager._streams["router-a"]
        # The replaced streams' responses were cancelled.
        _wait_until(lambda: manager._live == {current})

        server.should_exit = True
        thread.join(timeout=10)
        assert not thread.is_alive()
        assert manager._live == set()
    finally:
        for sock in sockets:
            sock.close()
    loop.close()
