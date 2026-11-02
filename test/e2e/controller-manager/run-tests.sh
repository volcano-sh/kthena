#!/usr/bin/env bash

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

set -euo pipefail

# ModelBooster's real vLLM lifecycle includes a long termination grace period.
# Give that domain and the remaining controller tests separate Go test processes
# without changing their timeouts or running against shared resources in parallel.
# The same prefix and its complement run every test exactly once, including new
# tests. Each process uses TestMain's existing installation and namespace cleanup.
suite_exit_code=0
go test -v -count=1 -timeout=20m -run '^TestModelCR' ./test/e2e/controller-manager/... || suite_exit_code=$?
go test -v -count=1 -timeout=20m -skip '^TestModelCR' ./test/e2e/controller-manager/... || suite_exit_code=$?
exit "$suite_exit_code"
