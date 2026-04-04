#!/usr/bin/env bash

set -euo pipefail

KO_VERSION="${KO_VERSION:-v0.18.1}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-v2.11.3}"
SETUP_ENVTEST_VERSION="${SETUP_ENVTEST_VERSION:-latest}"
TOOLS_BIN="${TOOLS_BIN:-$(pwd)/bin}"

mkdir -p "${TOOLS_BIN}"

echo "Installing golangci-lint ${GOLANGCI_LINT_VERSION}"
curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b "${TOOLS_BIN}" "${GOLANGCI_LINT_VERSION}"

echo "Installing ko ${KO_VERSION}"
GOCACHE="${GOCACHE:-/tmp/go-build}" \
GOMODCACHE="${GOMODCACHE:-/tmp/go-mod-cache}" \
GOBIN="${TOOLS_BIN}" \
go install "github.com/google/ko@${KO_VERSION}"

echo "Installing setup-envtest ${SETUP_ENVTEST_VERSION}"
GOCACHE="${GOCACHE:-/tmp/go-build}" \
GOMODCACHE="${GOMODCACHE:-/tmp/go-mod-cache}" \
GOBIN="${TOOLS_BIN}" \
go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@${SETUP_ENVTEST_VERSION}"

echo "Installed tools into ${TOOLS_BIN}"
