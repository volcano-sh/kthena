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
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml


@dataclass
class BackendProfile:
    """A group of mock backend pods sharing the same behaviour.

    YAML key → mocker CLI flag:
      engineType     → --engine-type
      model          → --model-path
      speedupRatio   → --speedup-ratio
      kvCacheBlocks  → --num-gpu-blocks-override
      maxNumSeqs     → --max-num-seqs
    """
    name: str
    count: int
    engine_type: str
    model: str
    speedup_ratio: float
    kv_cache_blocks: int | None = None
    max_num_seqs: int | None = None


@dataclass
class BackendsConfig:
    """Typed representation of the ``backends:`` block in a scenario YAML.

    Supports an optional ``common`` key for per-field defaults shared across
    all profiles.  Each profile can override any common field.
    """
    profiles: list[BackendProfile]

    # per-field defaults, matching benchmark/router-benchmark/k8s/mocker-deployment.yaml
    default_engine_type: str = "sglang"
    default_model: str = "Qwen/Qwen3-0.6B"
    default_speedup_ratio: float = 1.0
    default_kv_cache_blocks: int = 16384
    default_max_num_seqs: int = 256

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "BackendsConfig":
        common = data.get("common", {})
        defaults = {
            "default_engine_type": common.get("engineType", cls.default_engine_type),
            "default_model": common.get("model", cls.default_model),
            "default_speedup_ratio": float(common.get("speedupRatio", cls.default_speedup_ratio)),
            "default_kv_cache_blocks": common.get("kvCacheBlocks", cls.default_kv_cache_blocks),
            "default_max_num_seqs": common.get("maxNumSeqs", cls.default_max_num_seqs),
        }
        profiles = [
            BackendProfile(
                name=p["name"],
                count=p["count"],
                engine_type=p.get("engineType", defaults["default_engine_type"]),
                model=p.get("model", defaults["default_model"]),
                speedup_ratio=float(p.get("speedupRatio", defaults["default_speedup_ratio"])),
                kv_cache_blocks=p.get("kvCacheBlocks"),
                max_num_seqs=p.get("maxNumSeqs"),
            )
            for p in data.get("profiles", [])
        ]
        return cls(profiles=profiles, **defaults)


@dataclass
class ScenarioConfig:
    name: str
    description: str
    load: dict[str, Any]
    backends: BackendsConfig
    aiperf: dict[str, Any] = field(default_factory=dict)
    metrics: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self):
        """Coerce a raw dict backends to BackendsConfig for ergonomic construction."""
        if isinstance(self.backends, dict):
            self.backends = BackendsConfig.from_dict(self.backends)

    @classmethod
    def from_yaml(cls, path: str | Path) -> "ScenarioConfig":
        with Path(path).open(encoding="utf-8") as file:
            data = yaml.safe_load(file)
        # Convert raw dict to typed BackendsConfig
        data["backends"] = BackendsConfig.from_dict(data["backends"])
        return cls(**data)


@dataclass
class BenchmarkResult:
    config_name: str
    scenario: str
    timestamp: str
    metrics: dict[str, Any]
    raw_output: str
    artifacts: dict[str, Any] = field(default_factory=dict)
