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


set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(git rev-parse --show-toplevel)"
RELEASE_DIR=${RELEASE_DIR:-"_output/release"}
CHART_OUT_PATH="${ROOT_DIR}/${RELEASE_DIR}/chart"

TAG=${TAG:?"TAG must be set, e.g. TAG=v1.2.3"}
# The chart version follows plain semver like the other charts in the repository.
# Image tags keep the "v" prefix used by the released images.
CHART_VERSION=${TAG#v}
IMAGE_TAG="v${CHART_VERSION}"

for tool in helm yq; do
    if ! command -v "${tool}" >/dev/null 2>&1; then
        echo "Error: ${tool} is required but not installed"
        exit 1
    fi
done

echo "generate chart ${CHART_VERSION} with image tag ${IMAGE_TAG}"
rm -rf "${CHART_OUT_PATH}"
mkdir -p "${CHART_OUT_PATH}"
cp -r "${ROOT_DIR}/charts/kthena" "${CHART_OUT_PATH}/kthena"

yq e -i '
  .workload.controllerManager.image.tag = "'"${IMAGE_TAG}"'" |
  .workload.controllerManager.downloaderImage.tag = "'"${IMAGE_TAG}"'" |
  .workload.controllerManager.runtimeImage.tag = "'"${IMAGE_TAG}"'" |
  .networking.kthenaRouter.image.tag = "'"${IMAGE_TAG}"'"
' "${CHART_OUT_PATH}/kthena/values.yaml"

helm package "${CHART_OUT_PATH}/kthena" --version "${CHART_VERSION}" --app-version "${CHART_VERSION}" -d "${CHART_OUT_PATH}"
rm -rf "${CHART_OUT_PATH}/kthena"
echo "helm package end, chart is in ${CHART_OUT_PATH}"
