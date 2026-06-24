#!/usr/bin/env bash
# Copyright 2024 The Volcano Authors.
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

# This script manages GitHub Actions workflow states for the fork repository.
# It allows you to temporarily disable non-benchmark workflows to speed up
# benchmark CI runs, and restore them when creating PRs to upstream.
#
# Usage:
#   ./hack/manage-workflows.sh disable   # Comment out non-benchmark workflows
#   ./hack/manage-workflows.sh enable    # Restore all workflows
#   ./hack/manage-workflows.sh status    # Show current workflow status

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
WORKFLOWS_DIR="${REPO_ROOT}/.github/workflows"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Benchmark workflow (never disable this)
BENCHMARK_WORKFLOW="benchmark.yml"

# List of upstream workflows to manage
UPSTREAM_WORKFLOWS=(
    "approve-workflow.yml"
    "build-push-release.yml"
    "codespell.yaml"
    "docs-tests.yml"
    "e2e-tests.yml"
    "go-check.yml"
    "go-tests.yml"
    "licenses-lint.yaml"
    "python-licenses-lint.yml"
    "python-lint.yml"
    "python-tests.yml"
    "retest.yml"
)

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

is_commented() {
    local file="$1"
    if [[ ! -f "$file" ]]; then
        return 1
    fi
    # Check if the first non-empty line starts with #
    head -n 5 "$file" | grep -q "^#" && return 0 || return 1
}

disable_workflow() {
    local file="$1"
    local basename=$(basename "$file")
    
    if [[ ! -f "$file" ]]; then
        log_warn "Workflow not found: $basename"
        return 0
    fi
    
    if [[ "$basename" == "$BENCHMARK_WORKFLOW" ]]; then
        log_info "Skipping benchmark workflow: $basename"
        return 0
    fi
    
    if [[ "$file" == *.disabled ]]; then
        log_info "Already disabled: $basename"
        return 0
    fi
    
    mv "$file" "${file}.disabled"
    log_info "Disabled: $basename"
}

enable_workflow() {
    local file="$1"
    local basename=$(basename "$file")
    
    if [[ "$file" != *.disabled ]]; then
        return 0
    fi
    
    local original="${file%.disabled}"
    mv "$file" "$original"
    log_info "Enabled: $(basename "$original")"
}

show_status() {
    echo ""
    echo "Workflow Status:"
    echo "================"
    
    for workflow in "${UPSTREAM_WORKFLOWS[@]}"; do
        local file="${WORKFLOWS_DIR}/${workflow}"
        if [[ -f "$file" ]]; then
            echo -e "  ${GREEN}[ENABLED]${NC}  $workflow"
        elif [[ -f "${file}.disabled" ]]; then
            echo -e "  ${RED}[DISABLED]${NC} $workflow"
        else
            echo -e "  ${YELLOW}[MISSING]${NC}  $workflow"
        fi
    done
    
    echo ""
    echo "Benchmark Workflow:"
    echo "==================="
    if [[ -f "${WORKFLOWS_DIR}/${BENCHMARK_WORKFLOW}" ]]; then
        echo -e "  ${GREEN}[ENABLED]${NC}  $BENCHMARK_WORKFLOW"
    else
        echo -e "  ${RED}[MISSING]${NC}  $BENCHMARK_WORKFLOW"
    fi
    echo ""
}

disable_all() {
    log_info "Disabling non-benchmark workflows..."
    
    for workflow in "${UPSTREAM_WORKFLOWS[@]}"; do
        local file="${WORKFLOWS_DIR}/${workflow}"
        disable_workflow "$file"
    done
    
    log_info "All non-benchmark workflows disabled."
    log_info "Only benchmark workflow will run in CI."
}

enable_all() {
    log_info "Enabling all workflows..."
    
    for file in "${WORKFLOWS_DIR}"/*.disabled; do
        if [[ -f "$file" ]]; then
            enable_workflow "$file"
        fi
    done
    
    log_info "All workflows enabled."
}

main() {
    local command="${1:-status}"
    
    case "$command" in
        disable)
            disable_all
            ;;
        enable)
            enable_all
            ;;
        status)
            show_status
            ;;
        *)
            echo "Usage: $0 {disable|enable|status}"
            echo ""
            echo "Commands:"
            echo "  disable  - Comment out non-benchmark workflows"
            echo "  enable   - Restore all workflows"
            echo "  status   - Show current workflow status"
            exit 1
            ;;
    esac
}

main "$@"
