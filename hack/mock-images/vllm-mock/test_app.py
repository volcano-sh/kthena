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
import unittest
from app import app


class FlaskTestCase(unittest.TestCase):

    def setUp(self):
        self.client = app.test_client()

    def test_metrics(self):
        expected_total = 100
        replica = 3
        response = self.client.get('/metrics')
        self.assertEqual(response.status_code, 200)
        data = response.data.decode()
        print(f"response data: \n<<<<<<<<<<<<<<<<\n{data}\n<<<<<<<<<<<<<<<<")
        # metrics exists
        self.assertIn('vllm:request_success_total', data)
        self.assertIn('vllm:avg_prompt_throughput_toks_per_s', data)
        self.assertIn('vllm:avg_generation_throughput_toks_per_s', data)

        # assert metric value
        self.assertIn(
            f'vllm:request_success_total{{finished_reason="stop",model_name="llama2-70b"}} {expected_total / replica}',
            data)
        self.assertIn(f'vllm:avg_prompt_throughput_toks_per_s{{model_name="llama2-70b"}} {expected_total / replica}',
                      data)
        self.assertIn(
            f'vllm:avg_generation_throughput_toks_per_s{{model_name="llama2-70b"}} {expected_total / replica}', data)


if __name__ == '__main__':
    unittest.main()
