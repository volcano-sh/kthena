# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE.2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
"""Deployment and lifecycle management for external dependencies.

Manages Redis and other services required by router plugins (e.g.,
kvcache-aware) during benchmark matrix runs.
"""
from __future__ import annotations

import subprocess
from pathlib import Path

_REDIS_YAML = str(Path(__file__).resolve().parents[2] / "plugins" / "redis-standalone.yaml")


def deploy_redis():
    """Deploy Redis from the bundled redis-standalone.yaml."""
    subprocess.run(
        ["kubectl", "apply", "-f", _REDIS_YAML],
        check=True,
    )


def wait_for_redis_ready(timeout_sec: int = 60):
    """Wait for the redis-server pod to be Running."""
    subprocess.run(
        [
            "kubectl", "wait", "--for=condition=ready",
            "pod", "-l", "app.kubernetes.io/name=redis",
            f"--timeout={timeout_sec}s",
        ],
        check=True,
    )


def cleanup_redis():
    """Tear down the Redis deployment."""
    subprocess.run(
        [
            "kubectl", "delete", "-f", _REDIS_YAML,
            "--ignore-not-found",
        ],
        check=False,
    )
