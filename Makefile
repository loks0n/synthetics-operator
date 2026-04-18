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

KIND_CLUSTER ?= synthetics-dev

.PHONY: tools generate lint test test-envtest helm-lint helm-template ko-build-local \
        ko-build-results-writer-local kind-create kind-delete dev

tools:
	TOOLS_BIN=$(TOOLS_BIN) GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) KO_VERSION=$(KO_VERSION) SETUP_ENVTEST_VERSION=$(SETUP_ENVTEST_VERSION) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./hack/install-tools.sh

generate:
	controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/synthetics.dev_httpprobes.yaml charts/synthetics-operator/crds/synthetics.dev_httpprobes.yaml
	cp config/crd/bases/synthetics.dev_dnsprobes.yaml charts/synthetics-operator/crds/synthetics.dev_dnsprobes.yaml

lint: tools
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(TOOLS_BIN)/golangci-lint run --timeout=5m

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

test-envtest: tools
	KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) go test ./...

helm-lint:
	helm lint charts/synthetics-operator

helm-template:
	helm template synthetics-operator charts/synthetics-operator >/dev/null

kind-create:
	kind get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$" || \
		kind create cluster --name $(KIND_CLUSTER) --config hack/kind-config.yaml

kind-delete:
	kind delete cluster --name $(KIND_CLUSTER)

dev: tools kind-create
	tilt up

ko-build-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator $(TOOLS_BIN)/ko build --bare .

ko-build-results-writer-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-results-writer $(TOOLS_BIN)/ko build --bare ./images/results-writer

dashboard-configmaps: ## Regenerate hack/dashboard-configmaps.yaml from dashboards/*.json
	@for entry in "synthetics-overview-dashboard:synthetics-overview.json" "synthetics-http-probe-dashboard:http-probe.json" "synthetics-dns-probe-dashboard:dns-probe.json"; do \
		name=$$(echo $$entry | cut -d: -f1); \
		file=$$(echo $$entry | cut -d: -f2); \
		echo "---"; \
		kubectl create configmap $$name -n monitoring \
			--from-file=$$file=dashboards/$$file \
			--dry-run=client -o yaml | \
			python3 -c "import sys,yaml; d=yaml.safe_load(sys.stdin); d['metadata']['labels']={'grafana_dashboard':'1'}; print(yaml.dump(d))"; \
	done > hack/dashboard-configmaps.yaml
