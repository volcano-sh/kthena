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
"""
SGLang metrics mock server for kthena runtime verification.
Exposes Prometheus metrics at /metrics on port 30000.
"""
from flask import Response
from random import randint
import os

try:
    from flask import Flask
except ImportError:
    raise ImportError("flask is required. Run: pip install flask")

app = Flask(__name__)

MODEL_NAME = os.getenv("MODEL_NAME", "meta-llama/Llama-3.1-8B-Instruct")
SGLANG_METRICS_PORT = 30000


def generate_sglang_metrics():
    """Generate SGLang-compatible Prometheus metrics."""
    model_name = MODEL_NAME
    token_usage = min(1.0, randint(10, 90) / 100.0)
    num_queue_reqs = randint(0, 5)
    ttft_sum = randint(100, 500) / 1000.0
    ttft_count = randint(10, 100)
    tpot_sum = randint(50, 200) / 1000.0
    tpot_count = ttft_count

    return f"""# HELP sglang:token_usage KV cache utilization ratio (0.0-1.0)
# TYPE sglang:token_usage gauge
sglang:token_usage{{model_name="{model_name}"}} {token_usage}
# HELP sglang:num_queue_reqs Number of requests waiting in queue
# TYPE sglang:num_queue_reqs gauge
sglang:num_queue_reqs{{model_name="{model_name}"}} {num_queue_reqs}
# HELP sglang:time_to_first_token_seconds Histogram of time to first token in seconds
# TYPE sglang:time_to_first_token_seconds histogram
sglang:time_to_first_token_seconds_bucket{{le="0.001",model_name="{model_name}"}} 0
sglang:time_to_first_token_seconds_bucket{{le="0.005",model_name="{model_name}"}} 0
sglang:time_to_first_token_seconds_bucket{{le="0.08",model_name="{model_name}"}} {int(ttft_count * 0.3)}
sglang:time_to_first_token_seconds_bucket{{le="+Inf",model_name="{model_name}"}} {ttft_count}
sglang:time_to_first_token_seconds_sum{{model_name="{model_name}"}} {ttft_sum}
sglang:time_to_first_token_seconds_count{{model_name="{model_name}"}} {ttft_count}
# HELP sglang:time_per_output_token_seconds Histogram of time per output token in seconds
# TYPE sglang:time_per_output_token_seconds histogram
sglang:time_per_output_token_seconds_bucket{{le="0.001",model_name="{model_name}"}} 0
sglang:time_per_output_token_seconds_bucket{{le="0.005",model_name="{model_name}"}} {int(tpot_count * 0.5)}
sglang:time_per_output_token_seconds_bucket{{le="0.08",model_name="{model_name}"}} {tpot_count}
sglang:time_per_output_token_seconds_bucket{{le="+Inf",model_name="{model_name}"}} {tpot_count}
sglang:time_per_output_token_seconds_sum{{model_name="{model_name}"}} {tpot_sum}
sglang:time_per_output_token_seconds_count{{model_name="{model_name}"}} {tpot_count}
"""


@app.route("/metrics")
def metrics():
    return Response(generate_sglang_metrics(), mimetype="text/plain; charset=utf-8")


@app.route("/health")
def health():
    return "ok", 200


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=SGLANG_METRICS_PORT)
