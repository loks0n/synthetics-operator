# Contributing

Thanks for your interest in synthetics-operator. This file covers the practical bits — how to build, test, and ship changes. For what the project is and why, read the top-level `README.md`; for the developer-experience backlog, read `DX_AUDIT.md`.

## Development environment

Requirements:

- Go matching `go.mod`
- Docker (or OrbStack) — `make dev` expects a running daemon
- [`kind`](https://kind.sigs.k8s.io/), [`kubectl`](https://kubernetes.io/docs/tasks/tools/), [`helm`](https://helm.sh/)
- [`tilt`](https://docs.tilt.dev/) for the inner dev loop

One-time setup:

```sh
make tools   # installs ko, golangci-lint, setup-envtest, controller-gen into ./bin
```

Inner loop:

```sh
make dev     # creates kind cluster "synthetics-dev", runs `tilt up` with live-reload
```

`make dev` watches the source, rebuilds the operator image on save, and re-deploys. Probes you `kubectl apply` to the dev cluster will be picked up by the running operator.

## Tests

| Target | What it runs | Speed |
|---|---|---|
| `make test` | Unit tests for everything except envtest. | Seconds. |
| `make test-envtest` | Unit tests + envtest (spins up an ephemeral API server via `setup-envtest`). | ~30s. |
| `make lint` | `golangci-lint`. | Seconds. |
| `make helm-lint` / `make helm-template` | Chart validation. | Seconds. |

CI runs the same targets on every PR (`.github/workflows/ci.yaml`). A `kind-smoke` job runs on pushes to `main` and does a full end-to-end check: operator image built, loaded into kind, chart installed with NATS enabled, an `HTTPProbe` and a `PlaywrightTest` applied, and their metrics verified on `/metrics`.

Before opening a PR, at minimum:

```sh
make lint
make test-envtest
make helm-lint
```

## Regenerating CRDs

The API types in `api/v1alpha1/` drive both the `zz_generated.deepcopy.go` and the CRD YAML in `config/crd/bases/` (copied into `charts/synthetics-operator/crds/`). After changing any spec struct, marker, or kubebuilder annotation:

```sh
make generate
```

## Adding a new CRD kind

Using `PlaywrightTest` as the reference pattern, a new synthetic kind needs:

1. **Types** in `api/v1alpha1/<kind>_types.go` — spec + status structs, SchemeBuilder registration, `+kubebuilder:printcolumn` markers.
2. **Webhook** in `api/v1alpha1/<kind>_webhook.go` — defaulter + validator. Validator struct holds `client.Reader` (see `HTTPProbeValidator`) for dep existence and cycle checks. Call `ValidateDepends` and `ValidateMetricLabels` from `validate()`.
3. **Reconciler** in `controllers/<kind>_controller.go`. Model on `playwrighttest_controller.go` for CronJob-backed kinds or `http_probe_controller.go` for in-process kinds. Must call `Metrics.Delete`, `Metrics.ClearDepends`, and `Metrics.ClearMetricLabels` on deletion and `Metrics.SetDepends` + `Metrics.SetMetricLabels` on reconcile.
4. **Runner image** (tests only) in `images/<kind>-runner/`. Writes a `TestResult` JSON to `/results/output.json`; the `test-sidecar` picks it up and ships over NATS.
5. **Main wiring** in `main.go` — construct the reconciler, register its webhook, pass image flags through.
6. **Helm** — add the kind to `charts/synthetics-operator/templates/clusterrole.yaml` and `clusterrole-webhook.yaml`, add mutating + validating webhook config entries in `webhooks.yaml`, add any `kind-specific-image` values.
7. **Store** — if the kind has family-specific metrics (like `synthetics_test_playwright_case_*`), add observable gauges in `internal/metrics/store.go` and plumb them into the observe callback.
8. **DependencyKind enum** — add the new kind to `api/v1alpha1/depends.go` (`DependencyKind<Kind>` constant and the switch in `ValidateDepends` / `fetchDepends` / `checkDepExists`).
9. **Docs** — README §2 CRD table, §3 metric schema, `UBIQUITOUS_LANGUAGE.md` if the kind introduces new vocabulary.
10. **Tests** — webhook test mirroring the existing patterns, at least one envtest covering admission + reconciliation, example YAML in `examples/`.
11. **kind-smoke** — extend `.github/workflows/ci.yaml` to apply the new kind and assert on at least one emitted metric.

Run `make generate && make test-envtest && make helm-lint` to shake out missing pieces.

## Release process

Releases are triggered by pushing a tag `vX.Y.Z`:

```sh
git tag v1.2.3
git push origin v1.2.3
```

The `release` workflow builds and publishes:

- Operator image, `test-sidecar`, and `k6-runner` via `ko` to `ghcr.io/loks0n/synthetics-*`.
- `playwright-runner` via Docker buildx to the same registry.
- Helm chart packaged as OCI artifact at `oci://ghcr.io/loks0n/charts/synthetics-operator`.
- GitHub Release notes generated from commits since the last tag.

See `.github/workflows/release.yaml` and `.goreleaser.yaml` for the exact steps.

## PR expectations

- One logical change per PR. If you're renaming a thing and adding a feature, those are two PRs.
- Regenerate CRDs + run `make lint` before pushing.
- For new CRD kinds or schema changes, update the README metric schema and `UBIQUITOUS_LANGUAGE.md`.
- Include test coverage for new behaviour.
- Commit messages: brief subject (~70 chars), body explaining *why* if non-obvious. The commit log is the project's history; make it readable.

## Debugging a failing probe or test

In order of increasing work:

1. `kubectl describe httpprobe my-probe` — transition events (`ProbeActive`, `ProbeFailed`) show a timeline.
2. `curl $(kubectl -n synthetics-system get svc synthetics-operator-metrics -o jsonpath='{.spec.clusterIP}'):8080/metrics | grep synthetics_probe` — current pass/fail + `result` label.
3. `kubectl -n synthetics-system logs deployment/synthetics-operator` — reconcile and scheduler output.
4. For `K6Test` / `PlaywrightTest`: `kubectl get cronjob my-test` and `kubectl logs -l job-name=my-test-<id>` on the most recent job pod.

## Where the pieces live

| Path | What it is |
|---|---|
| `api/v1alpha1/` | CRD types, webhooks, generated deepcopy |
| `controllers/` | Reconcilers for each kind, plus `scheduler.go` (in-process probes) |
| `internal/probes/` | HTTP + DNS executor code, worker pool |
| `internal/metrics/` | Prometheus/OTel store, `synthetics_probe` / `synthetics_test` gauges |
| `internal/events/` | Transition notifier emitting `ProbeActive` / `ProbeFailed` events |
| `internal/natsconsumer/` | NATS subscriber feeding test results into the metrics store |
| `internal/results/` | Shared NATS message format (`TestResult`) |
| `internal/webhookcerts/` | Self-managed webhook serving certs, hot-reload |
| `images/` | Runner image sources (`k6-runner`, `playwright-runner`, `test-sidecar`) |
| `charts/synthetics-operator/` | Helm chart |
| `dashboards/` | Grafana dashboards (source JSON; `hack/dashboard-configmaps.yaml` is generated) |
| `alerts/` | `PrometheusRule` spec |
| `examples/` | Reference CRD YAMLs |
| `hack/` | kind config, tool installers, generated artifacts |
