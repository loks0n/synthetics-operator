SHELL := /bin/bash

GOCACHE ?= /tmp/go-build
GOMODCACHE ?= /tmp/go-mod-cache
GOFLAGS :=

TOOLS_BIN := $(CURDIR)/bin
export PATH := $(TOOLS_BIN):$(PATH)

GOLANGCI_LINT_VERSION ?= v2.11.3
KO_VERSION ?= v0.18.1
SETUP_ENVTEST_VERSION ?= latest
ENVTEST_K8S_VERSION ?= 1.34.x
KUBEBUILDER_ASSETS ?= $(shell [ -x "$(TOOLS_BIN)/setup-envtest" ] && $(TOOLS_BIN)/setup-envtest use -p path $(ENVTEST_K8S_VERSION) 2>/dev/null)

.PHONY: tools fmt lint vet test test-envtest helm-lint helm-template ko-build-local ci

tools:
	TOOLS_BIN=$(TOOLS_BIN) GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) KO_VERSION=$(KO_VERSION) SETUP_ENVTEST_VERSION=$(SETUP_ENVTEST_VERSION) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./hack/install-tools.sh

fmt:
	test -z "$$(gofmt -l .)"

lint: tools
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(TOOLS_BIN)/golangci-lint run --timeout=5m

vet:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go vet ./...

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-envtest: tools
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

helm-lint:
	helm lint charts/synthetics-operator

helm-template:
	helm template synthetics-operator charts/synthetics-operator >/dev/null

ko-build-local: tools
	KO_DOCKER_REPO=ko.local $(TOOLS_BIN)/ko build --local --bare .

ci: fmt vet test-envtest helm-lint helm-template
