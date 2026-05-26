SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

MODULE      := github.com/kanya-approve/karpenter-provider-rackspace-spot
BIN_DIR     := $(CURDIR)/bin
TOOLS_DIR   := $(BIN_DIR)/tools
GO          ?= go
GOFLAGS     ?=
LDFLAGS     ?= -s -w

CONTROLLER_GEN_VERSION ?= v0.21.0

CONTROLLER_GEN := $(TOOLS_DIR)/controller-gen

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the controller binary
	$(GO) build $(GOFLAGS) -ldflags='$(LDFLAGS)' -o $(BIN_DIR)/controller ./cmd/controller

.PHONY: test
test: ## Run unit tests
	$(GO) test $(GOFLAGS) -race -coverprofile=coverage.txt ./...

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	$(GO) mod tidy

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate CRDs, deepcopy; sync CRD into chart
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths="./pkg/apis/..."
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:artifacts:config=config/crd
	mkdir -p charts/karpenter/crds
	cp config/crd/*.yaml charts/karpenter/crds/

IMAGE ?= ghcr.io/kanya-approve/karpenter-provider-rackspace-spot
TAG   ?= dev

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(TAG) -t $(IMAGE):$(TAG) .

.PHONY: chart-lint
chart-lint: ## Lint the Helm chart
	helm lint charts/karpenter

.PHONY: chart-template
chart-template: ## Render the Helm chart with a placeholder token
	helm template karpenter charts/karpenter --set spot.refreshToken=placeholder

$(CONTROLLER_GEN): | $(TOOLS_DIR)
	GOBIN=$(TOOLS_DIR) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(TOOLS_DIR):
	mkdir -p $@

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR) coverage.txt cover.html
