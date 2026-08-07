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
TAG=${TAG:-latest}
CLUSTER_NAME=${CLUSTER_NAME:-kthena-e2e}
TEST_CATEGORY=${TEST_CATEGORY:-all}

# Create Kind cluster
echo "Creating Kind cluster: ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}"

kind get kubeconfig --name "${CLUSTER_NAME}" > /tmp/kubeconfig-e2e
export KUBECONFIG=/tmp/kubeconfig-e2e

echo "Kind cluster '${CLUSTER_NAME}' created successfully"

echo "Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=300s

echo "Start to build Docker images"
make docker-build-all HUB=${HUB} TAG=${TAG}

echo "Loading Docker images into Kind cluster"
kind load docker-image ${HUB}/kthena-router:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/kthena-controller-manager:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/downloader:${TAG} --name "${CLUSTER_NAME}"
kind load docker-image ${HUB}/runtime:${TAG} --name "${CLUSTER_NAME}"

echo "Start to install cert-manager"
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
echo "Waiting for cert-manager to be ready..."
go install github.com/cert-manager/cmctl/v2@latest && $(go env GOPATH)/bin/cmctl check api --wait=5m

echo "Start to install Volcano"
kubectl apply -f https://raw.githubusercontent.com/volcano-sh/volcano/master/installer/volcano-development.yaml

# Install CRDs based on test category
case "${TEST_CATEGORY}" in
  gateway-api)
    echo "Start to install Gateway API CRDs"
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
    ;;
  gateway-inference-extension)
    echo "Start to install Gateway API CRDs"
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
    echo "Start to install Gateway API Inference Extension CRDs"
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.2.0/manifests.yaml
    ;;
  controller-manager)
    echo "Start to install LeaderWorkerSet CRDs"
    kubectl create -f https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/tags/v0.8.0/charts/lws/crds/leaderworkerset.x-k8s.io_leaderworkersets.yaml
    ;;
  router)
    echo "Router tests: no additional CRDs needed"
    ;;
  all|*)
    echo "Start to install Gateway API CRDs"
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/standard-install.yaml
    echo "Start to install Gateway API Inference Extension CRDs"
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.2.0/manifests.yaml
    echo "Start to install LeaderWorkerSet CRDs"
    kubectl create -f https://raw.githubusercontent.com/kubernetes-sigs/lws/refs/tags/v0.8.0/charts/lws/crds/leaderworkerset.x-k8s.io_leaderworkersets.yaml
    ;;
esac

echo "E2E setup completed for category: ${TEST_CATEGORY}"
echo "Cluster: ${CLUSTER_NAME}"
echo "KUBECONFIG: /tmp/kubeconfig-e2e"
