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
class ScenarioConfig:
    name: str
    description: str
    load: dict[str, Any]
    backends: dict[str, Any]
    aiperf: dict[str, Any] = field(default_factory=dict)
    metrics: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_yaml(cls, path: str | Path) -> "ScenarioConfig":
        with Path(path).open(encoding="utf-8") as file:
            data = yaml.safe_load(file)
        return cls(**data)


@dataclass
class BenchmarkResult:
    config_name: str
    scenario: str
    timestamp: str
    metrics: dict[str, Any]
    raw_output: str
    artifacts: dict[str, Any] = field(default_factory=dict)
