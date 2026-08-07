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

# ko defaults to linux/amd64 regardless of host, which produces images a kind
# node on Apple Silicon cannot execute. Build for the host architecture so the
# local dev loop works on arm64 and amd64 alike.
KO_PLATFORM ?= linux/$(shell go env GOARCH)

.PHONY: tools generate lint test test-envtest helm-lint helm-template \
        ko-build-local ko-build-controller-local ko-build-webhook-local \
        ko-build-prober-local ko-build-metrics-local ko-build-heartbeat-local \
        ko-build-test-sidecar-local ko-build-k6-runner-local docker-build-playwright-runner-local \
        kind-create kind-delete dev

tools:
	TOOLS_BIN=$(TOOLS_BIN) GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION) KO_VERSION=$(KO_VERSION) SETUP_ENVTEST_VERSION=$(SETUP_ENVTEST_VERSION) GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./hack/install-tools.sh

generate:
	$(TOOLS_BIN)/controller-gen object paths="./api/..."
	$(TOOLS_BIN)/controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/synthetics.dev_httpprobes.yaml charts/synthetics-operator/crds/synthetics.dev_httpprobes.yaml
	cp config/crd/bases/synthetics.dev_dnsprobes.yaml charts/synthetics-operator/crds/synthetics.dev_dnsprobes.yaml
	cp config/crd/bases/synthetics.dev_tcpprobes.yaml charts/synthetics-operator/crds/synthetics.dev_tcpprobes.yaml
	cp config/crd/bases/synthetics.dev_heartbeats.yaml charts/synthetics-operator/crds/synthetics.dev_heartbeats.yaml
	cp config/crd/bases/synthetics.dev_k6tests.yaml charts/synthetics-operator/crds/synthetics.dev_k6tests.yaml
	cp config/crd/bases/synthetics.dev_playwrighttests.yaml charts/synthetics-operator/crds/synthetics.dev_playwrighttests.yaml

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

ko-build-local: ko-build-controller-local ko-build-webhook-local ko-build-prober-local ko-build-metrics-local ko-build-heartbeat-local

ko-build-controller-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator-controller $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./cmd/controller

ko-build-webhook-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator-webhook $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./cmd/webhook

ko-build-prober-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator-prober $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./cmd/prober

ko-build-metrics-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator-metrics $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./cmd/metrics

ko-build-heartbeat-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-operator-heartbeat $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./cmd/heartbeat

ko-build-test-sidecar-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-test-sidecar $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./images/test-sidecar

ko-build-k6-runner-local:
	@test -x "$(TOOLS_BIN)/ko" || { echo "missing $(TOOLS_BIN)/ko; run 'make tools' first" >&2; exit 1; }
	@KO_DOCKER_REPO=ko.local/synthetics-k6-runner $(TOOLS_BIN)/ko build --bare --platform=$(KO_PLATFORM) ./images/k6-runner

docker-build-playwright-runner-local:
	@docker build -t ko.local/synthetics-playwright-runner ./images/playwright-runner

dashboard-configmaps: ## Regenerate hack/dashboard-configmaps.yaml from dashboards/*.json
	@for file in dashboards/*.json; do \
		base=$$(basename $$file .json); \
		echo "---"; \
		kubectl create configmap $$base-dashboard -n monitoring \
			--from-file=$$(basename $$file)=$$file \
			--dry-run=client -o yaml | \
			kubectl label -f - --local --dry-run=client -o yaml grafana_dashboard=1; \
	done > hack/dashboard-configmaps.yaml
