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
    routing: dict[str, Any]
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
