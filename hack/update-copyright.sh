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

set -eo pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"

GO_TPL="$ROOT_DIR/hack/boilerplate.go.txt"
PY_TPL="$ROOT_DIR/hack/boilerplate.py.txt"

add_header() {
  local file=$1
  local template=$2
  local temporary

  temporary=$(mktemp "${file}.tmp.XXXXXX")
  if ! cp -p "$file" "$temporary"; then
    rm -f "$temporary"
    return 1
  fi

  if ! { cat "$template" && echo && cat "$file"; } > "$temporary"; then
    rm -f "$temporary"
    return 1
  fi

  if ! mv "$temporary" "$file"; then
    rm -f "$temporary"
    return 1
  fi
}

add_headers() {
  local extension=$1
  local template=$2
  local file

  while IFS= read -r -d '' file; do
    if ! grep -q "Copyright The Volcano Authors" "$file"; then
      add_header "$file" "$template"
    fi
  done < <(find "$ROOT_DIR" \
    -type d \( -name vendor -o -name venv \) -prune -o \
    -type f -name "*.${extension}" -print0)
}

add_headers go "$GO_TPL"
add_headers py "$PY_TPL"

echo "Update Copyright Done"
