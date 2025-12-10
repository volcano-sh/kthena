# Image URL to use all building/pushing image targets
HUB ?= ghcr.io/volcano-sh
TAG ?= latest
PROJECT_DIR := $(shell dirname $(abspath $(lastword $(MAKEFILE_LIST))))


# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: gen-crd
gen-crd: controller-gen 
	$(CONTROLLER_GEN) crd paths="./pkg/apis/networking/..." output:crd:artifacts:config=charts/kthena/charts/networking/crds
	$(CONTROLLER_GEN) crd paths="./pkg/apis/workload/..." output:crd:artifacts:config=charts/kthena/charts/workload/crds

.PHONY: gen-docs
gen-docs: crd-ref-docs ## Generate CRD and CLI reference documentation
    # Generate CRD ref docs
	mkdir -p docs/kthena/docs/api
	$(CRD_REF_DOCS) \
		--source-path=./pkg/apis \
		--config=docs/kthena/crd-ref-docs-config.yaml \
		--output-path=docs/kthena/docs/reference/crd \
		--renderer=markdown \
		--output-mode=group
	# Generate Kthena CLI docs using a standalone doc-gen program
	go run ./cli/kthena/internal/tools/docgen/main.go

.PHONY: generate
generate: gen-crd ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	go mod tidy
	./hack/update-codegen.sh
	$(MAKE) gen-docs
	$(MAKE) gen-copyright

.PHONY: gen-check
gen-check: generate
	git diff --exit-code

.PHONY: test
test: generate ## Run tests. Exclude e2e, client-go.
	go test $$(go list ./... | grep -v /e2e | grep -v /client-go) -coverprofile cover.out

.PHONY: test-docs
test-docs: ## Run documentation tests (type check and build)
	cd docs/kthena && npm run typecheck
	cd docs/kthena && npm run build

.PHONY: test-e2e
test-e2e: ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v kind >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@echo "Setting up Kind cluster for E2E tests..."
	@./test/e2e/setup.sh
	@echo "Running E2E tests..."
	@KUBECONFIG=/tmp/kubeconfig-e2e go test $$(go list ./... | grep /test/e2e) -v -timeout=10m
	@echo "E2E tests completed"

.PHONY: test-e2e-cleanup
test-e2e-cleanup: ## Clean up the Kind cluster used for E2E tests.
	@./test/e2e/cleanup.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

##@ Build

.PHONY: build
build: generate fmt vet
	go build -o bin/kthena-router cmd/kthena-router/main.go
	go build -o bin/kthena-controller-manager cmd/kthena-controller-manager/main.go
	go build -o bin/kthena cli/kthena/main.go

IMG_CONTROLLER ?= ${HUB}/kthena-controller-manager:${TAG}
IMG_ROUTER ?= ${HUB}/kthena-router:${TAG}
IMG_DOWNLOADER ?= ${HUB}/downloader:${TAG}
IMG_RUNTIME ?= ${HUB}/runtime:${TAG}

.PHONY: docker-build-router
docker-build-router: generate
	$(CONTAINER_TOOL) build -t ${IMG_ROUTER} -f docker/Dockerfile.kthena-router .

.PHONY: docker-build-controller
docker-build-controller: generate
	$(CONTAINER_TOOL) build -t ${IMG_CONTROLLER} -f docker/Dockerfile.kthena-controller-manager .

.PHONY: docker-build-downloader
docker-build-downloader: generate
	$(CONTAINER_TOOL) build -t ${IMG_DOWNLOADER} --target downloader -f python/Dockerfile python

.PHONY: docker-build-runtime
docker-build-runtime: generate
	$(CONTAINER_TOOL) build -t ${IMG_RUNTIME} --target runtime -f python/Dockerfile python

.PHONY: docker-push
docker-push: docker-build-router docker-build-controller ## Push all images to the registry.
	$(CONTAINER_TOOL) push ${IMG_ROUTER}
	$(CONTAINER_TOOL) push ${IMG_CONTROLLER}

# PLATFORMS defines the target platforms for the images be built to provide support to multiple
# architectures.
PLATFORMS ?= linux/arm64,linux/amd64

# Make sure Buildx is set up:
#   docker buildx create --name mybuilder --driver docker-container --use
#   docker buildx inspect --bootstrap

.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for cross-platform support
	$(CONTAINER_TOOL) buildx build \
		--platform ${PLATFORMS} \
		-t ${IMG_ROUTER} \
		-f docker/Dockerfile.kthena-router \
		--push .
	$(CONTAINER_TOOL) buildx build \
		--platform ${PLATFORMS} \
		-t ${IMG_CONTROLLER} \
		-f docker/Dockerfile.kthena-controller-manager \
		--push .

.PHONY: docker-buildx-all
docker-buildx-all: ## Build all images.
	$(CONTAINER_TOOL) buildx build --platform ${PLATFORMS} --load -t ${IMG_ROUTER} -f docker/Dockerfile.kthena-router .
	$(CONTAINER_TOOL) buildx build --platform ${PLATFORMS} --load -t ${IMG_CONTROLLER} -f docker/Dockerfile.kthena-controller-manager .
	$(CONTAINER_TOOL) buildx build --platform ${PLATFORMS} --load -t ${IMG_DOWNLOADER} --target downloader -f python/Dockerfile python
	$(CONTAINER_TOOL) buildx build --platform ${PLATFORMS} --load -t ${IMG_RUNTIME} --target runtime -f python/Dockerfile python
	@echo "All images built."

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.17.2
GOLANGCI_LINT_VERSION ?= v1.64.8
CRD_REF_DOCS_VERSION ?= v0.2.0



.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

.PHONY: gen-copyright
gen-copyright:
	@echo "Adding copyright headers..."
	@hack/update-copyright.sh

mod-download-go:
	@-GOFLAGS="-mod=readonly" find -name go.mod -execdir go mod download \;
# go mod tidy is needed with Golang 1.16+ as go mod download affects go.sum
# https://github.com/golang/go/issues/43994
# exclude docs folder
	@find . -path ./docs -prune -o -name go.mod -execdir go mod tidy \;

.PHONY: mirror-licenses
mirror-licenses: mod-download-go; \
	go install istio.io/tools/cmd/license-lint@v0.0.0-20240221165422-57f6bfb4cd73;  \
	rm -fr licenses; \
	license-lint --mirror

.PHONY: lint-licenses
lint-licenses:
	@if test -d licenses; then license-lint --config config/licenses-lint.yaml; fi

.PHONY: licenses-check
licenses-check: mirror-licenses; \
    hack/licenses-check.sh

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
