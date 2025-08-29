# Image URL to use all building/pushing image targets
IMG ?= controller:latest
PROVIDER_LIBVIRT_IMG ?= ghcr.io/projectbeskar/virtrigaud/provider-libvirt:latest
PROVIDER_VSPHERE_IMG ?= ghcr.io/projectbeskar/virtrigaud/provider-vsphere:latest
PROVIDER_PROXMOX_IMG ?= ghcr.io/projectbeskar/virtrigaud/provider-proxmox:latest
TAG ?= latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Version information
VERSION ?= $(shell git describe --tags --always --dirty)
GIT_SHA ?= $(shell git rev-parse HEAD)
LDFLAGS := -X github.com/projectbeskar/virtrigaud/internal/version.Version=$(VERSION) -X github.com/projectbeskar/virtrigaud/internal/version.GitSHA=$(GIT_SHA)

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

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./api/v1alpha1" paths="./api/infra.virtrigaud.io/v1beta1" paths="./internal/controller/..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	@echo "Formatting code (excluding libvirt packages)..."
	@go list ./... | grep -v '/internal/providers/libvirt' | grep -v '/cmd/provider-libvirt' | grep -v '/test/integration' | xargs go fmt

.PHONY: vet
vet: ## Run go vet against code.
	@echo "Running go vet (excluding libvirt packages)..."
	@go list ./... | grep -v '/internal/providers/libvirt' | grep -v '/cmd/provider-libvirt' | grep -v '/test/integration' | xargs go vet

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	@echo "Running tests (excluding libvirt packages)..."
	@TEST_DIRS=$$(find . -name "*_test.go" -not -path "./internal/providers/libvirt/*" -not -path "./cmd/provider-libvirt/*" -not -path "./test/e2e/*" -not -path "./test/integration/*" -exec dirname {} \; | sort -u | tr '\n' ' '); \
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$TEST_DIRS -coverprofile cover.out

.PHONY: envtest-setup
envtest-setup: setup-envtest ## Install setup-envtest and export KUBEBUILDER_ASSETS for local runs
	@echo "To run tests locally with envtest, export:"
	@echo "export KUBEBUILDER_ASSETS=\"$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)\""

.PHONY: providers-version
providers-version: ## Print versions for manager and all providers
	@echo "Manager: $(VERSION)"
	@echo "Provider libvirt: $(VERSION)"
	@echo "Provider vSphere: $(VERSION)"
	@echo "Git SHA: $(GIT_SHA)"

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@$(KIND) get clusters | grep -q 'kind' || { \
		echo "No Kind cluster is running. Please start a Kind cluster before running the e2e tests."; \
		exit 1; \
	}
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: ci
ci: test lint proto-lint generate manifests vet ## Run all CI checks locally
	@echo "✅ All CI checks passed"

# Protocol buffer definitions
PROTO_DIR = proto

.PHONY: proto
proto: buf ## Generate gRPC stubs from protocol buffer definitions using buf
	cd $(PROTO_DIR) && $(BUF) generate

.PHONY: proto-lint
proto-lint: buf ## Lint protocol buffer definitions
	cd $(PROTO_DIR) && $(BUF) lint

.PHONY: proto-breaking
proto-breaking: buf ## Check for breaking changes in protocol buffer definitions
	cd $(PROTO_DIR) && $(BUF) breaking --against '.git#branch=main'

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: build-provider-libvirt
build-provider-libvirt: proto ## Build libvirt provider binary (requires CGO)
	CGO_ENABLED=1 go build -ldflags "$(LDFLAGS)" -o bin/provider-libvirt ./cmd/provider-libvirt

.PHONY: build-provider-vsphere
build-provider-vsphere: proto ## Build vsphere provider binary
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/provider-vsphere ./cmd/provider-vsphere

.PHONY: build-provider-proxmox
build-provider-proxmox: proto ## Build proxmox provider binary
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/provider-proxmox ./cmd/provider-proxmox

.PHONY: build-provider-mock
build-provider-mock: ## Build mock provider binary
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/provider-mock ./cmd/provider-mock

.PHONY: build-providers
build-providers: build-provider-libvirt build-provider-vsphere build-provider-proxmox build-provider-mock ## Build all provider binaries

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

##@ Module Release

.PHONY: release-proto
release-proto: proto-lint ## Release proto module with tags and buf push
	@echo "Releasing proto module..."
	@cd proto && \
	if [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION is required. Usage: make release-proto VERSION=v0.1.0"; \
		exit 1; \
	fi && \
	git tag proto/$(VERSION) && \
	echo "Tagged proto module with proto/$(VERSION)" && \
	if command -v buf >/dev/null 2>&1; then \
		echo "Pushing to buf registry..."; \
		buf push || echo "buf push failed or not configured"; \
	else \
		echo "buf not found, skipping buf push"; \
	fi

.PHONY: release-sdk
release-sdk: ## Release SDK module with tags and generate docs
	@echo "Releasing SDK module..."
	@cd sdk && \
	if [ -z "$(VERSION)" ]; then \
		echo "Error: VERSION is required. Usage: make release-sdk VERSION=v0.1.0"; \
		exit 1; \
	fi && \
	go mod tidy && \
	git tag sdk/$(VERSION) && \
	echo "Tagged SDK module with sdk/$(VERSION)" && \
	mkdir -p ../docs/sdk && \
	echo "# Provider SDK $(VERSION)" > ../docs/sdk/README.md && \
	echo "" >> ../docs/sdk/README.md && \
	echo "Install the SDK:" >> ../docs/sdk/README.md && \
	echo "" >> ../docs/sdk/README.md && \
	echo '```bash' >> ../docs/sdk/README.md && \
	echo "go get github.com/projectbeskar/virtrigaud/sdk/provider@$(VERSION)" >> ../docs/sdk/README.md && \
	echo '```' >> ../docs/sdk/README.md && \
	echo "" >> ../docs/sdk/README.md && \
	echo "## Packages" >> ../docs/sdk/README.md && \
	echo "" >> ../docs/sdk/README.md && \
	for pkg in $$(find provider -name "*.go" -exec dirname {} \; | sort -u); do \
		echo "- [$$pkg](./$$pkg/)" >> ../docs/sdk/README.md; \
	done

##@ Build

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} --build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-provider-libvirt
docker-provider-libvirt: ## Build docker image for libvirt provider
	$(CONTAINER_TOOL) build -f cmd/provider-libvirt/Dockerfile -t $(PROVIDER_LIBVIRT_IMG) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) .

.PHONY: docker-provider-vsphere
docker-provider-vsphere: ## Build docker image for vsphere provider
	$(CONTAINER_TOOL) build -f cmd/provider-vsphere/Dockerfile -t $(PROVIDER_VSPHERE_IMG) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) .

.PHONY: docker-provider-proxmox
docker-provider-proxmox: ## Build docker image for proxmox provider
	$(CONTAINER_TOOL) build -f cmd/provider-proxmox/Dockerfile -t $(PROVIDER_PROXMOX_IMG) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) .

.PHONY: docker-providers
docker-providers: docker-provider-libvirt docker-provider-vsphere docker-provider-proxmox ## Build all provider docker images

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name virtrigaud-builder
	$(CONTAINER_TOOL) buildx use virtrigaud-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm virtrigaud-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
BUF ?= $(LOCALBIN)/buf

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.64.8
BUF_VERSION ?= v1.46.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: buf
buf: $(BUF) ## Download buf locally if necessary.
$(BUF): $(LOCALBIN)
	$(call go-install-tool,$(BUF),github.com/bufbuild/buf/cmd/buf,$(BUF_VERSION))

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
