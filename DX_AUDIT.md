# Developer Experience Audit

Findings from a smoke play session on a fresh kind cluster: applying each CRD kind, inducing failures, checking metrics, and watching the kubectl surface. Organised into three buckets.

- **Bucket A** — obvious fixes + consistency. Bugs and small papercuts with an unambiguous right answer.
- **Bucket B** — new features. Additions, each worth its own phase-sized discussion.
- **Bucket C** — production decisions. Product-level calls, especially around how assertions and pass/fail are modelled across kinds.

---

## Bucket A — obvious fixes + consistency

All resolved. Retained here as a historical pointer to the kinds of papercuts to look for in future kinds / phases.

- ~~`kubectl get httpprobes` shows only NAME/AGE~~ — `+kubebuilder:printcolumn` markers added to all four CRDs.
- ~~`kubectl explain playwrighttest.spec` returns empty~~ — Go doc comments added to `PlaywrightTestSpec`, `K6TestSpec`, `RunnerSpec`.
- ~~`otel_scope_*` labels on every metric~~ — OTel Prometheus exporter configured with `WithoutScopeInfo()` and `WithoutTargetInfo()`.
- ~~HTTPProbe/DNSProbe accept non-cron intervals while K6Test/PlaywrightTest reject them~~ — `validateProbeInterval` accepts sub-minute freely, applies the cron divisibility rule for intervals >= 1m, matching the CronJob-backed kinds.
- ~~Default `timeout: 10s` + short interval → confusing error~~ — `defaultIntervalTimeout` now caps timeout at `min(10s, interval)`.
- ~~`examples/phase1-httpprobe.yaml` dev vocabulary~~ — renamed to `httpprobe-basic.yaml`; probe name `phase1-echo` → `echo-healthcheck`.

---

## Bucket B — new features

Each needs its own phase-sized scope. Listed lightest-weight first.

- ~~**Kubernetes events on probe/test transitions.**~~ Shipped. `internal/events.Notifier` subscribes to `Store.OnProbeTransition` / `OnTestTransition` and emits `Normal ProbeActive` / `Warning ProbeFailed` (probes) and `Normal TestActive` / `Warning TestFailed` (tests) on each outcome flip. `kubectl describe` now shows a timeline.
- **Grafana dashboard for PlaywrightTest.** `dashboards/playwright-tests.json` — suite pass-rate, per-test duration, currently-failing-tests. Should have landed as part of Phase 10 but slid because the metric schema is still under debate (Bucket C3).
- **`kubectl synthetics` plugin.** Subcommands: `status`, `logs`, `trigger-now`, `results`. Every user interaction today needs port-forward + curl + grep. A plugin that queries `/metrics` via the in-cluster Service closes the gap.
- **Richer HTTPProbe assertions.** Phase 2 described body-contains + JSONPath (`BodyAssertion`), but today the assertion expression language is numeric-only. Reintroducing body assertions needs either a richer spec or a new sibling field — depends on Bucket C1.

---

## Bucket C — production decisions

These aren't fixes. They're product-level calls that shape the schema before it accumulates users.

### C1 — Assertion expression language scope

Current HTTPProbe grammar: `variable op number`. No strings, regex, JSON, or booleans. Phase 2 in the README describes body-contains and JSONPath, but current types don't include them.

- *Drop the ambition from README* — numeric only. Body assertions need a different shape anyway.
- *Extend grammar* — add string literals + regex operator (`~=`) + JSONPath as a first-class variable. Bigger language, more to get right.
- *Second assertion type* — keep `expr` for numeric, add `bodyContains` / `jsonPath` as sibling fields. Two shapes can be clearer than one overloaded grammar.

**Recommendation**: two shapes. Overloaded DSLs create fights.

### C2 — Metrics surface: what do we promise?

- ~~**Delete promises the product doesn't keep.**~~ `synthetics_consecutive_failures` cut from README. `synthetics_probe_suppressed` shipped with Phase 11.
- **Cardinality budget.** ~15–20 lines per probe × 1000 probes = 20k+ lines per scrape, before `metricLabels` (Phase 12) lands. Profile now and write a "recommended max probes per operator" figure into the docs rather than discovering it at a user site.

### C3 — Opinionated image lock-in (note, not question)

The playwright-runner ships with pinned Playwright 1.48.2 + Chromium. Users can't choose the version, can't add Firefox/WebKit, can't override Chromium flags. This is the trade from "opinionated + small". Once there's a user on 1.48.2 needing 1.52, the answer is "upgrade the whole chart." Decide now whether that's acceptable — if not, `spec.playwrightVersion` returns and the project needs a multi-image strategy.

---

## Suggested order of action

1. Bucket C1 (assertion grammar) next, if body/JSON assertions re-enter scope.
2. Bucket B as bandwidth permits.
