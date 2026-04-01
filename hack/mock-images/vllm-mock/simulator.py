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
import threading
import time
import random
from typing import Optional

class Request:
    def __init__(self, arrived_at, input_tokens, output_tokens, arrived_next=0):
        self.arrived_at = arrived_at
        self.input_tokens = input_tokens
        self.output_tokens = output_tokens
        self.arrived_next = arrived_next

class Simulator:
    def __init__(self, config=None):
        self._terminate = False

    def start(self):
        # Dummy thread for compatibility
        def dummy_run():
            while not self._terminate:
                time.sleep(1)
        
        t = threading.Thread(target=dummy_run)
        t.start()
        return t

    def stop(self):
        self._terminate = True

    def execute(self, request: Request) -> float:
        # Simple latency mock: base delay + per-token delay
        base_latency = 0.05
        per_token_latency = 0.002
        latency = base_latency + (request.input_tokens + request.output_tokens) * per_token_latency
        latency *= random.uniform(0.9, 1.1)
        return latency
