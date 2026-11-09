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

# Run ModelBooster, coordinated/in-place rollouts, and the remaining controller
# tests in separate serial Go processes. Each domain gets its own 20m process
# budget within the 50m CI job, with TestMain's existing installation and cleanup.
# The two disjoint prefixes and their complement select every test once.
suite_exit_code=0
go test -v -count=1 -timeout=20m -run '^TestModelCR' ./test/e2e/controller-manager/... || suite_exit_code=$?
go test -v -count=1 -timeout=20m -run '^TestModelServing(Coordinated|InPlace)' ./test/e2e/controller-manager/... || suite_exit_code=$?
go test -v -count=1 -timeout=20m -skip '^TestModelCR|^TestModelServing(Coordinated|InPlace)' ./test/e2e/controller-manager/... || suite_exit_code=$?
exit "$suite_exit_code"
