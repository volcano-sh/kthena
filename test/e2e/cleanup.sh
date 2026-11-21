#!/bin/bash

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

set -e

CLUSTER_NAME=${CLUSTER_NAME:-kthena-e2e}

echo "Cleaning up Kind cluster for E2E tests..."

# Check if cluster exists
if ! kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
    echo "Kind cluster '${CLUSTER_NAME}' does not exist"
    exit 0
fi

# Delete cluster
echo "Deleting Kind cluster: ${CLUSTER_NAME}"
kind delete cluster --name "${CLUSTER_NAME}"

# Clean up kubeconfig
if [ -f "${KUBECONFIG_E2E:-/tmp/kubeconfig-e2e}" ]; then
    rm -f "${KUBECONFIG_E2E:-/tmp/kubeconfig-e2e}"
    echo "Cleaned up kubeconfig file"
fi

echo "Kind cluster '${CLUSTER_NAME}' deleted successfully"
