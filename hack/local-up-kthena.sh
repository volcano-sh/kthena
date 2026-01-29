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

KT_ROOT=$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )/..
CLUSTER_NAME=${CLUSTER_NAME:-kthena}
CLUSTER_CONTEXT=("--name" "${CLUSTER_NAME}")
KIND_OPT=${KIND_OPT:-}
INSTALL_MODE=${INSTALL_MODE:-"kind"}
KT_NAMESPACE=${KT_NAMESPACE:-"kthena-system"}
IMAGE_PREFIX=${IMAGE_PREFIX:-ghcr.io/volcano-sh}
TAG=${TAG:-$(git rev-parse --verify HEAD 2>/dev/null || echo "latest")}
HELM_RELEASE_NAME=${HELM_RELEASE_NAME:-kthena}
OS=${OS:-$(go env GOOS 2>/dev/null || echo "linux")}

# Color codes for output
RED='\033[0;31m'
YELLOW='\033[1;33m'
GREEN='\033[0;32m'
NC='\033[0m' # No Color

# Counters for preflight checks
FATAL_ERRORS=0
WARNINGS=0

# ============================================================================
# PREFLIGHT VALIDATION FUNCTIONS
# ============================================================================

# Print an error message
function print_error() {
  echo -e "${RED}ERROR${NC}: $1" >&2
}

# Print a warning message
function print_warning() {
  echo -e "${YELLOW}WARNING${NC}: $1"
}

# Print a success message
function print_success() {
  echo -e "${GREEN}✓${NC} $1"
}

# Check if a binary is installed
function check_binary() {
  local binary=$1
  local install_hint=$2
  
  if command -v "${binary}" >/dev/null 2>&1; then
    print_success "${binary} is installed"
    return 0
  else
    print_error "${binary} is not installed"
    if [[ -n "${install_hint}" ]]; then
      echo "  Install hint: ${install_hint}"
    fi
    ((FATAL_ERRORS++))
    return 1
  fi
}

# Check if at least one binary from a list exists
function check_binary_one_of() {
  local binaries=("$@")
  local found=0
  
  for binary in "${binaries[@]}"; do
    if command -v "${binary}" >/dev/null 2>&1; then
      local version=$("${binary}" version 2>/dev/null || echo "(version unknown)")
      print_success "${binary} is installed: ${version}"
      found=1
      break
    fi
  done
  
  if [[ ${found} -eq 0 ]]; then
    # If no cluster tool is present, warn rather than fail immediately.
    # When running in kind mode the script can attempt to install kind later
    # (see check-prerequisites/check-kind in hack/lib/install.sh). Keep this
    # a warning so local setups that will be created by the script are not
    # blocked unnecessarily.
    print_warning "None of the cluster tools were found: ${binaries[*]}"
    echo "  Install one of:"
    echo "    - kind: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
    echo "    - k3d: https://k3d.io/usage/install/"
    echo "    - minikube: https://minikube.sigs.k8s.io/docs/start/"
    ((WARNINGS++))
    return 0
  fi
  return 0
}

# Check if kubectl cluster is reachable
function check_cluster_reachable() {
  echo ""
  echo "Verifying cluster connectivity..."
  
  kubectl cluster-info >/dev/null 2>&1
  if [[ $? -ne 0 ]]; then
    print_error "Kubernetes cluster is not reachable"
    echo "  Make sure a Kubernetes cluster is running and KUBECONFIG is set correctly"
    ((FATAL_ERRORS++))
    return 1
  fi
  
  print_success "Kubernetes cluster is reachable"
  return 0
}

# Check if container runtime (Docker or Podman) is available
function check_container_runtime() {
  local runtime_found=0
  
  if command -v docker >/dev/null 2>&1; then
    local docker_version=$(docker version --format '{{.Server.Version}}' 2>/dev/null || echo "(version unknown)")
    print_success "Docker is installed: ${docker_version}"
    runtime_found=1
    return 0
  fi
  
  if command -v podman >/dev/null 2>&1; then
    local podman_version=$(podman version --format '{{.Version}}' 2>/dev/null || echo "(version unknown)")
    print_success "Podman is installed: ${podman_version}"
    runtime_found=1
    return 0
  fi
  
  if [[ ${runtime_found} -eq 0 ]]; then
    print_error "Neither Docker nor Podman is installed"
    echo "  Install one of:"
    echo "    - Docker: https://docs.docker.com/install/"
    echo "    - Podman: https://podman.io/getting-started/installation/"
    ((FATAL_ERRORS++))
    return 1
  fi
}

# Main preflight checks function
function check_prereqs() {
  echo "================================================"
  echo "Running Preflight Validation Checks"
  echo "================================================"
  echo ""
  
  # ---- BINARY CHECKS ----
  echo "Checking required binaries..."
  check_binary "kubectl" "https://kubernetes.io/docs/tasks/tools/"
  check_binary "helm" "https://helm.sh/docs/intro/install/"
  check_container_runtime
  check_binary_one_of "kind" "k3d" "minikube"
  
  echo ""
  
  # ---- CLUSTER VALIDATION (only for existing clusters) ----
  if [[ "${INSTALL_MODE}" == "existing" ]]; then
    check_cluster_reachable
  fi
  
  echo ""
  echo "================================================"
  
  # Print summary
  if [[ ${FATAL_ERRORS} -gt 0 ]]; then
    echo -e "${RED}Preflight check FAILED${NC}"
    echo "Fatal errors: ${FATAL_ERRORS}"
    echo ""
    return 1
  fi
  
  if [[ ${WARNINGS} -gt 0 ]]; then
    echo -e "${YELLOW}Preflight check completed with ${WARNINGS} warning(s)${NC}"
  else
    echo -e "${GREEN}Preflight check PASSED${NC}"
  fi
  echo ""
  
  return 0
}

# prepare deploy yaml and docker images
function prepare {
  echo "Preparing..."
  install-helm

  echo "Building docker images"
  make docker-build-all HUB=${IMAGE_PREFIX} TAG=${TAG}
}

function install-kthena {
  # Create namespace if it doesn't exist
  kubectl create namespace ${KT_NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -
  
  # Install kthena using helm
  helm upgrade --install ${HELM_RELEASE_NAME} ${KT_ROOT}/charts/kthena \
    --namespace ${KT_NAMESPACE} \
    --set workload.controllerManager.image.repository=${IMAGE_PREFIX}/kthena-controller-manager \
    --set workload.controllerManager.image.tag=${TAG} \
    --set workload.controllerManager.image.pullPolicy=IfNotPresent \
    --set workload.controllerManager.downloaderImage.repository=${IMAGE_PREFIX}/downloader \
    --set workload.controllerManager.downloaderImage.tag=${TAG} \
    --set workload.controllerManager.runtimeImage.repository=${IMAGE_PREFIX}/runtime \
    --set workload.controllerManager.runtimeImage.tag=${TAG} \
    --set networking.kthenaRouter.image.repository=${IMAGE_PREFIX}/kthena-router \
    --set networking.kthenaRouter.image.tag=${TAG} \
    --set networking.kthenaRouter.image.pullPolicy=IfNotPresent
}

function uninstall-kthena {
  helm uninstall ${HELM_RELEASE_NAME} --namespace ${KT_NAMESPACE} || true
  kubectl delete namespace ${KT_NAMESPACE} --ignore-not-found=true
}

# clean up
function cleanup {
  uninstall-kthena

  if [ "${INSTALL_MODE}" == "kind" ]; then
    echo "Running kind: [kind delete cluster ${CLUSTER_CONTEXT[*]}]"
    kind delete cluster "${CLUSTER_CONTEXT[@]}" || true
  fi
}

echo $* | grep -E -q "\-\-help|\-h"
if [[ $? -eq 0 ]]; then
  echo "Customize the kind-cluster name:

    export CLUSTER_NAME=<custom cluster name>  # default: kthena

Customize kind options other than --name:

    export KIND_OPT=<kind options>

Using existing kubernetes cluster rather than starting a kind cluster:

    export INSTALL_MODE=existing

Customize image prefix:

    export IMAGE_PREFIX=<image prefix>  # default: ghcr.io/volcano-sh

Customize image tag:

    export TAG=<image tag>  # default: git commit hash

Customize namespace:

    export KT_NAMESPACE=<namespace>  # default: kthena-system

Customize helm release name:

    export HELM_RELEASE_NAME=<release name>  # default: kthena

Cleanup all installation:

    ./hack/local-up-kthena.sh -q

Run preflight checks only (without deploying):

    ./hack/local-up-kthena.sh --check-only
"
  exit 0
fi

echo $* | grep -E -q "\-\-quit|\-q"
if [[ $? -eq 0 ]]; then
  cleanup
  exit 0
fi

# Check for --check-only flag
echo $* | grep -E -q "\-\-check-only"
if [[ $? -eq 0 ]]; then
  check_prereqs
  exit $?
fi

source "${KT_ROOT}/hack/lib/install.sh"

# Run preflight checks (will exit with error if fatal issues found)
check_prereqs || exit 1

# The call to check-prerequisites (from hack/lib/install.sh) remains to
# perform any install-time actions the helper implements (for example,
# installing kind or helm automatically when running in kind mode). The
# global preflight above only validates environment/cluster readiness and
# provides early, actionable errors; this call continues to allow the
# existing helper to perform safe, idempotent installs if needed.
check-prerequisites

prepare

if [ "${INSTALL_MODE}" == "kind" ]; then
  kind-up-cluster
  export KUBECONFIG=${HOME}/.kube/config
fi

install-kthena

echo ""
echo "Kthena has been installed successfully!"
echo "Namespace: ${KT_NAMESPACE}"
echo "Release name: ${HELM_RELEASE_NAME}"
echo ""
echo "To check the status:"
echo "  kubectl get pods -n ${KT_NAMESPACE}"
echo ""
echo "To uninstall:"
echo "  ./hack/local-up-kthena.sh -q"

