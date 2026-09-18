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

import logging
import time
from dataclasses import dataclass
from typing import Dict, List, Optional
from urllib.parse import urlparse

logger = logging.getLogger(__name__)

DEFAULT_ROUTER_TTL_SECONDS = 90
MAX_ROUTER_TTL_SECONDS = 3600


def validate_router_endpoint(endpoint: str) -> bool:
    """Only plain http/https URLs with a host are accepted as push targets."""
    if not endpoint:
        return False
    try:
        parsed = urlparse(endpoint)
    except ValueError:
        return False
    return parsed.scheme in ("http", "https") and bool(parsed.hostname)


@dataclass
class RouterRegistration:
    router_id: str
    endpoint: str
    expires_at: float
    # Identifies one router process; a restarted router container re-registers
    # with a new generation even when its pod name (router_id) is unchanged.
    generation: str = ""


class RouterRegistry:
    """Tracks router instances registered for in-memory KV event push.

    Routers re-register periodically (heartbeat); registrations that are not
    renewed within their TTL are pruned and no longer receive events.
    """

    def __init__(self):
        self._routers: Dict[str, RouterRegistration] = {}

    def register(self, router_id: str, endpoint: str,
                 ttl_seconds: int = DEFAULT_ROUTER_TTL_SECONDS,
                 generation: str = "") -> bool:
        """Register or renew a router.

        Returns True when the router needs a full snapshot: it is new, its
        previous registration expired, it re-registered with a new endpoint,
        or it re-registered with a new process generation (e.g. after a
        router container restart that lost the in-memory index).
        """
        if not router_id:
            raise ValueError("router_id must not be empty")
        if not validate_router_endpoint(endpoint):
            raise ValueError(f"invalid router endpoint: {endpoint!r}")

        ttl = min(max(int(ttl_seconds), 1), MAX_ROUTER_TTL_SECONDS)
        now = time.monotonic()
        existing = self._routers.get(router_id)
        needs_snapshot = (
            existing is None
            or existing.endpoint != endpoint
            or existing.generation != generation
            or existing.expires_at <= now
        )
        self._routers[router_id] = RouterRegistration(
            router_id=router_id,
            endpoint=endpoint.rstrip("/"),
            expires_at=now + ttl,
            generation=generation,
        )
        if needs_snapshot:
            logger.info(
                f"Router registered: id={router_id}, endpoint={endpoint}, ttl={ttl}s")
        return needs_snapshot

    def active_endpoints(self) -> List[str]:
        self._prune()
        return [r.endpoint for r in self._routers.values()]

    def _prune(self) -> None:
        now = time.monotonic()
        expired = [rid for rid, reg in self._routers.items() if reg.expires_at <= now]
        for rid in expired:
            logger.info(f"Router registration expired: id={rid}")
            del self._routers[rid]


_router_registry: Optional[RouterRegistry] = None


def get_router_registry() -> RouterRegistry:
    global _router_registry
    if _router_registry is None:
        _router_registry = RouterRegistry()
    return _router_registry
