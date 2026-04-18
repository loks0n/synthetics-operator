# synthetics-operator

## Key commands

```sh
make tools           # install golangci-lint, ko, setup-envtest into ./bin
make generate        # regenerate CRDs from API types (run after changing api/v1alpha1)
make lint            # golangci-lint (uses ./bin/golangci-lint)
make test            # go test ./... (unit tests only, no envtest)
make test-envtest    # go test ./... with real API server via setup-envtest
make helm-lint       # helm lint the chart
make dev             # create kind cluster + tilt up (full local dev loop)
make ko-build-local              # build operator image into local Docker daemon via ko
make ko-build-test-sidecar-local # build test-sidecar image into local Docker daemon via ko
make ko-build-k6-runner-local    # build k6-runner image into local Docker daemon via ko
```

## Naming

**Helper functions are a code smell.** "Helper" is a catchall that means you haven't named the problem. When you reach for a name like `helper`, `util`, `common`, or `misc` — stop. Find the real concept, name it, and put it in a purpose-built package or type. Every `internal/helpers/` directory is a junk drawer waiting to happen.
