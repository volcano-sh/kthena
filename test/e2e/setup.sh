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

HUB=${HUB:-ghcr.io/volcano-sh}
# Tag for e2e test
TAG=${TAG:-1.0.0}
CLUSTER_NAME=${CLUSTER_NAME:-kthena-e2e}

# Create Kind cluster
echo "Creating Kind cluster: ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}"

# Set kubeconfig
kind get kubeconfig --name "${CLUSTER_NAME}" > /tmp/kubeconfig-e2e
export KUBECONFIG=/tmp/kubeconfig-e2e

echo "Kind cluster '${CLUSTER_NAME}' created successfully"

# Wait for cluster to be ready
echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=300s

# Docker build
echo "Start to build Docker images"
if [ "$(uname -m)" = "x86_64" ]; then
  export DOCKER_BUILDX_PLATFORM=linux/amd64
else
  export DOCKER_BUILDX_PLATFORM=linux/arm64
fi
make docker-buildx-all HUB=${HUB} TAG=${TAG} PLATFORMS=${DOCKER_BUILDX_PLATFORM}

# Load images into Kind cluster
echo "Loading Docker images into Kind cluster"
kind load docker-image ${HUB}/kthena-router:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/kthena-controller-manager:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/downloader:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/runtime:${TAG} --name "${CLUSTER_NAME}"

# Install cert-manager
echo "Start to install cert-manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.yaml
echo "Waiting for cert-manager to be ready..."
go install github.com/cert-manager/cmctl/v2@latest && $(go env GOPATH)/bin/cmctl check api --wait=5m
# Install Volcano
echo "Start to install Volcano"
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/volcano/master/installer/volcano-development.yaml
# Install by helm
helm install kthena ./charts/kthena --namespace dev --create-namespace \
  --set networking.kthenaRouter.image.tag=${TAG} \
  --set networking.webhook.image.tag=${TAG} \
  --set workload.controllerManager.image.tag=${TAG} \
  --set workload.controllerManager.downloaderImage.tag=${TAG} \
  --set workload.controllerManager.runtimeImage.tag=${TAG}

# Wait for pods to be ready
echo "Waiting for pods to be ready..."
kubectl wait --for=condition=Ready pod --all -n=dev --timeout=300s

echo "E2E setup completed successfully"
echo "Cluster: ${CLUSTER_NAME}"
echo "KUBECONFIG: /tmp/kubeconfig-e2e"