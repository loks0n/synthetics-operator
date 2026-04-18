# synthetics-operator
### Architecture & Design Document
*Kubernetes Operator for Unified Synthetic Monitoring — April 2026*


## 1. Overview

synthetics-operator is an open-source Kubernetes operator for unified synthetic monitoring. It provides a consistent, declarative way to define HTTP checks, DNS probes, SSL certificate monitoring, scripted browser tests, and load tests — all exportable to Prometheus and visualised in Grafana.

The project is deliberately opinionated: Playwright for browser tests, k6 for load tests. This constraint simplifies the operator, sets clear expectations for users, and avoids the complexity of supporting arbitrary runtimes.

> **Primary motivation:** replace paid SaaS monitoring products (BetterStack, Datadog Synthetics, etc.) with a fully in-cluster, open-source alternative that unifies uptime probes, DNS checks, scripted browser tests, and load tests under a single declarative API — with no per-seat pricing, no data leaving the cluster, and no proprietary query language.

### 1.1 Core Goals

- Unified monitoring across HTTP, DNS, SSL, browser flows, and load tests
- Declarative CRD-based configuration, native to Kubernetes
- All results exported to Prometheus — no proprietary data store
- Importable Grafana dashboards and Alertmanager rules included in the repo
- Single Helm install, low operational overhead
- Open source, designed for community adoption

### 1.2 What it is not

- Not an alerting system — metrics only, users own their Alertmanager rules
- Not a generic script runner — Playwright and k6 only
- Not a SaaS product — fully in-cluster

### 1.3 Why NATS

CronJob pods are ephemeral and external to the operator process. When a Playwright or k6 test finishes, its result has to go somewhere — that somewhere must be a shared, stateful component reachable from inside an arbitrary pod. This is an unavoidable architectural constraint, not a choice.

The alternatives all have the same problem in different forms:

- **Kubernetes API (ConfigMaps)** — makes the API server the data plane. Every run creates a ConfigMap, triggers a watch event, and requires a reconcile + status patch. The scaling ceiling is the API server reconcile throughput, not the number of probes.
- **HTTP endpoint on the operator** — the operator becomes the shared stateful component. A single replica is a bottleneck and availability risk; multiple replicas require coordinating who owns the metrics state.

Given a shared stateful component is required, NATS is the right choice. It is purpose-built for exactly this — lightweight message routing between processes — and it keeps every other component stateless and independently scalable. The `test-sidecar` publishes and disconnects. Probe workers consume and scale freely. The metrics consumer reads the stream without any knowledge of who wrote to it.

NATS ships as a single ~20MB binary with ~32Mi idle memory footprint. It is not a heavy dependency. With JetStream disabled (the default), it adds no persistence requirements. With JetStream enabled, it gains durability and the ability to buffer results across restarts — controlled entirely by Helm values.

### 1.4 Why OTel, exposed as Prometheus

The operator uses the OpenTelemetry Go SDK for all internal instrumentation and exports via the OTel Prometheus exporter on `/metrics`. This keeps the external interface compatible with the Prometheus ecosystem (Alertmanager, Grafana, recording rules, existing k8s monitoring stacks) while using OTel as the instrumentation layer. No OTel collector is required — the operator's `/metrics` endpoint is scraped directly by whatever Prometheus is already in the cluster.

---

## 2. Custom Resource Definitions

The operator defines four CRDs, split into two execution models: lightweight network probes that run in-process within the operator, and script-based tests that execute as Kubernetes CronJobs.

| CRD | Execution Model | Purpose |
|-----|----------------|---------|
| `HttpProbe` | In-operator | HTTP assertions + SSL certificate checks |
| `DnsProbe` | In-operator | DNS resolution checks |
| `PlaywrightTest` | CronJob (Playwright) | Scripted browser flows and multi-step journeys |
| `K6Test` | CronJob (k6) | Load and performance testing |

### 2.1 Shared fields

All CRDs support the following fields:

| Field | Description |
|-------|-------------|
| `spec.suspend` | Boolean, default false. Pauses the probe without deleting it. In-operator probes are unscheduled; CronJobs set `suspend: true`. Use for maintenance windows. |
| `spec.depends` | List of probe dependencies. See section 2.6. |
| `spec.metricLabels` | Custom Prometheus metric labels. Distinct from Kubernetes `metadata.labels`. See section 3.8. |

### 2.2 HttpProbe

HTTP checks with named assertions on status code, response latency, and SSL certificate expiry. Each assertion is an independent named expression; all are evaluated on every run.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: HTTPProbe
metadata:
  name: api-health
spec:
  interval: 30s
  timeout: 10s
  suspend: false          # pause without deleting
  request:
    url: https://my-service/health
    method: GET           # any HTTP method: GET, POST, PUT, PATCH, DELETE, HEAD, …
    headers:
      Authorization: "Bearer token"
      Content-Type: application/json
    body: '{"ping": true}'          # optional request body
  tls:                              # optional TLS overrides
    insecureSkipVerify: false
    caCert: |                       # PEM-encoded CA certificate
      -----BEGIN CERTIFICATE-----
      …
      -----END CERTIFICATE-----
  assertions:
    - name: status_ok
      expr: "status_code = 200"
    - name: fast_response
      expr: "duration_ms < 500"
    - name: ssl_valid
      expr: "ssl_expiry_days >= 30"
```

**Assertion expression language**

Each assertion has a `name` (used as the `reason` label on `synthetics_probe_up` and the `assertion` label on `synthetics_probe_assertion_result`) and an `expr` of the form `variable op value`.

Supported operators: `=`, `!=`, `<`, `<=`, `>`, `>=`

Available variables for HTTPProbe:

| Variable | Description |
|----------|-------------|
| `status_code` | HTTP response status code |
| `duration_ms` | Total request duration in milliseconds |
| `ssl_expiry_days` | Days until the TLS certificate expires (`-1` if no TLS) |

All assertions are evaluated on every run. `synthetics_probe_up` is 0 if any assertion fails; the `reason` label names the first failure. `synthetics_probe_assertion_result` exposes a per-assertion 0/1 gauge for every assertion regardless of whether others passed or failed.

### 2.3 DnsProbe

DNS resolution checks targeting a hostname. Supports all common record types. An optional `resolver` field allows checking against a specific DNS server rather than the cluster default — useful for debugging split-horizon DNS.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: DNSProbe
metadata:
  name: api-dns
spec:
  interval: 30s
  timeout: 10s
  suspend: false
  query:
    name: my-service.com
    type: A           # A, AAAA, CNAME, MX, TXT, NS, PTR
    resolver: "8.8.8.8:53"   # optional; defaults to 8.8.8.8:53
  assertions:
    - name: has_answers
      expr: "answer_count > 0"
    - name: fast_response
      expr: "duration_ms < 500"
```

Available variables for DNSProbe:

| Variable | Description |
|----------|-------------|
| `answer_count` | Number of records in the DNS Answer section |
| `duration_ms` | DNS query round-trip time in milliseconds |

### 2.4 PlaywrightTest

Playwright scripts stored in ConfigMaps and executed on a schedule inside the cluster. Designed for multi-step browser flows, authenticated journeys, and checks that require JavaScript execution. Playwright version is pinned per probe for reproducibility. A `runner` block configures all pod-level concerns.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: PlaywrightTest
metadata:
  name: checkout-flow
spec:
  interval: 5m
  timeout: 30s
  suspend: false             #
  script:
    configMap:
      name: checkout-playwright-script
      key: script.js
  playwrightVersion: "1.42.0"
  ttlAfterFinished: 1h       # explicit, defaults to 1h if omitted
  depends:
    - kind: HTTPProbe
      name: auth-service

  runner:
    env:
      - name: LOGIN_PASSWORD
        valueFrom:
          secretKeyRef:
            name: test-credentials
            key: password
    envFrom:
      - secretRef:
          name: bulk-secrets
      - configMapRef:
          name: test-config
    resources:
      requests:
        memory: 512Mi
        cpu: 250m
      limits:
        memory: 1Gi
        cpu: 500m
```

All four CRD types use `interval` (duration string). For PlaywrightTest and K6Test, the operator converts the interval to a CronJob schedule string and applies a per-test minute offset using `probeOffset()` — preventing clustering when multiple tests share the same interval. Minimum interval for CronJob-backed tests is `1m` (cron resolution limit); sub-minute intervals are only supported by HttpProbe and DnsProbe.

### 2.5 K6Test

k6 scripts executed on an interval or triggered externally via CI. Supports distributed execution via parallelism — the operator automatically divides VUs across test pods using k6 execution segments, users just set total VUs in the script. A `runner` block configures all pod-level concerns separately from test configuration.

Thresholds are defined in the k6 script itself — the script is the source of truth. The operator parses threshold pass/fail results from k6 summary output; there is no `spec.thresholds` field on the CRD.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: K6Test
metadata:
  name: api-load
spec:
  interval: 2h              # optional, minimum 1m

  suspend: false            #
  parallelism: 4
  k6Version: "0.50.0"
  script:
    configMap:
      name: api-k6-script
      key: script.js
  ttlAfterFinished: 1h      # explicit default documented

  runner:
    env:
      - name: TARGET_URL
        value: "https://my-service"
      - name: API_KEY
        valueFrom:
          secretKeyRef:
            name: test-credentials
            key: api-key
    envFrom:
      - secretRef:
          name: bulk-test-secrets
      - configMapRef:
          name: test-config
    resources:
      requests:
        memory: 256Mi
        cpu: 500m
      limits:
        memory: 512Mi
        cpu: 1000m
    affinity:
      podAntiAffinity:
        preferred:
          - topologyKey: kubernetes.io/hostname   # spread runners across nodes
```

### 2.6 depends field

All CRDs support a `depends` field listing other probes that must be healthy before a failure is considered actionable. If any dependency is unhealthy, the probe still runs but its failure is suppressed in metrics — a separate metric is emitted instead, preserving visibility without triggering alerts.

Deliberately limited to one level of depth — no chaining, no orchestration. Purely a noise reduction tool.

```yaml
depends:
  - kind: HTTPProbe
    name: auth-service
  - kind: DNSProbe
    name: api-dns
```

### 2.7 CRD version graduation strategy

Initial release as `v1alpha1`. Graduation path:

- `v1alpha1` → `v1beta1` once CRD schemas stabilise and at least 3 external adopters are running in production
- `v1beta1` → `v1` after one full minor release cycle with no breaking schema changes
- Conversion webhooks will be implemented at each transition to support zero-downtime upgrades

No timeline commitment — driven by schema stability, not calendar.

---

## 3. Metrics Schema

All metrics use a consistent label set across CRD types, enabling unified Grafana dashboards. The operator exposes a `/metrics` endpoint on port 8080 for Prometheus scraping.

### 3.1 Shared labels

```
name, namespace, kind   # kind = HttpProbe | DnsProbe | PlaywrightTest | K6Test
```

Plus any custom labels defined in `spec.metricLabels` — see section 3.8.

### 3.2 Shared metrics — all types

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_probe_up` | gauge 0\|1 | Whether the last probe run succeeded |
| `synthetics_consecutive_failures` | gauge | Number of consecutive failures since last success |
| `synthetics_last_run_timestamp` | gauge | Unix timestamp of last probe execution |
| `synthetics_probe_suppressed` | gauge 0\|1 | Probe failed but suppressed due to unhealthy dependency |
| `synthetics_probe_config_error` | gauge 0\|1 | Probe is misconfigured (bad URL, unreachable resolver, invalid script). Distinct from execution failure. |

### 3.3 HttpProbe metrics

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_probe_duration_ms` | gauge | Total request duration in milliseconds |
| `synthetics_probe_http_status_code` | gauge | HTTP response status code |
| `synthetics_probe_http_version` | gauge | HTTP version (1.0, 1.1, 2.0, 3.0) |
| `synthetics_probe_http_phase_duration_ms` | gauge | Per-phase timing with `phase` label: `dns`, `connect`, `tls`, `processing`, `transfer` |
| `synthetics_probe_tls_cert_expiry_timestamp_seconds` | gauge | Unix timestamp of TLS leaf certificate expiry; absent if no TLS |
| `synthetics_probe_assertion_result` | gauge 0\|1 | Per-assertion pass/fail with `assertion` (name) and `expr` (expression) labels. Emitted for every assertion on every run regardless of other assertion outcomes. |
| `synthetics_probe_http_info` | gauge (always 1) | Static probe configuration: `method` label carries the HTTP method |

### 3.4 DnsProbe metrics

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_dns_success` | gauge 0\|1 | Whether DNS resolution succeeded |
| `synthetics_dns_response_ms` | gauge | DNS response time in milliseconds |
| `synthetics_dns_response_first_answer_value` | gauge | Value of the first record in the DNS Answer section (value always 1), with a `value` label carrying the resolved string. Works across all record types: IP for A/AAAA, hostname for CNAME/MX/NS/PTR, text for TXT. One series per probe — no cardinality risk. |
| `synthetics_dns_response_first_answer_type` | gauge | Record type of the first Answer section record (value always 1), with a `type` label (e.g. `A`, `CNAME`, `MX`). Lets you detect type mismatches — e.g. queried A but received a CNAME. |
| `synthetics_dns_response_answer_count` | gauge | Number of records in the Answer section |
| `synthetics_dns_response_authority_count` | gauge | Number of records in the Authority section |
| `synthetics_dns_response_additional_count` | gauge | Number of records in the Additional section |

### 3.5 PlaywrightTest metrics

Playwright's JSON reporter provides per-test results. The operator parses this output from the CRD status and emits individual test metrics alongside suite-level rollups, giving per-test visibility in Grafana rather than a single pass/fail gauge.

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_playwright_test_passed` | gauge 0\|1 | Per-test pass/fail with `suite` and `test` labels |
| `synthetics_playwright_test_duration_ms` | gauge | Per-test duration with `suite` and `test` labels |
| `synthetics_playwright_tests_total` | gauge | Total number of tests in the suite |
| `synthetics_playwright_tests_passed` | gauge | Number of passing tests |
| `synthetics_playwright_tests_failed` | gauge | Number of failing tests |
| `synthetics_playwright_runner_start_time` | gauge | Pod start timestamp — useful for spotting slow cold starts |

### 3.6 K6Test metrics

k6 summary JSON is parsed from the CRD status on completion and re-exported as curated metrics. The full k6 Prometheus output plugin is not used, keeping the schema clean.

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_loadtest_passed` | gauge 0\|1 | Whether all k6 thresholds passed |
| `synthetics_loadtest_http_req_duration_ms` | gauge | Request duration percentiles with quantile label |
| `synthetics_loadtest_http_req_failed_rate` | gauge | Proportion of failed requests |
| `synthetics_loadtest_vus` | gauge | Virtual user count at test completion |
| `synthetics_loadtest_iterations` | gauge | Total iterations completed |
| `synthetics_loadtest_duration_seconds` | gauge | Total test run duration |

### 3.7 Operator health metrics

The operator exposes its own health metrics alongside probe metrics, giving visibility into scheduling saturation, controller errors, and cert lifecycle.

**Worker pool**

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_operator_probes_total` | gauge | Total registered probes per `kind` |
| `synthetics_operator_worker_pool_size` | gauge | Configured worker pool size |
| `synthetics_operator_worker_pool_active` | gauge | Currently executing workers |
| `synthetics_operator_worker_pool_queue_depth` | gauge | Jobs waiting in queue — key saturation signal |

**Controller**

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_operator_reconcile_total` | counter | Reconcile attempts with `kind` and `result` (success\|error\|requeue) labels |
| `synthetics_operator_reconcile_duration_ms` | histogram | Reconcile duration per `kind` |
| `synthetics_operator_cronjob_rejected_total` | counter | Jobs rejected by the API server (resource quota, LimitRange, admission webhook). Surfaces cluster policy conflicts. |

**CronJob lifecycle**

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_operator_cronjob_active` | gauge | Currently running Jobs per `kind` |
| `synthetics_operator_cronjob_failed_total` | counter | Jobs that failed without posting results per `kind` |

**Result ingestion**

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_operator_results_received_total` | counter | Results received from NATS result stream per `kind` |
| `synthetics_operator_results_parse_failed_total` | counter | Results received but failed to parse per `kind` |
| `synthetics_operator_nats_publish_failed_total` | counter | NATS publish failures per component (sidecar, probe-worker) |

**Certificate**

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_operator_cert_expiry_days` | gauge | Days until webhook serving cert expires |
| `synthetics_operator_cert_rotations_total` | counter | Number of cert rotations performed |

> `synthetics_operator_worker_pool_queue_depth` is the most operationally useful — a growing queue means the pool is saturated and needs more workers or fewer probes.

### 3.8 Custom metric labels

All CRDs support a `spec.metricLabels` field that propagates to every Prometheus metric emitted for that probe. This enables per-team alerting, per-environment dashboard filtering, and criticality tiering without requiring separate namespaces.

```yaml
kind: HTTPProbe
metadata:
  name: checkout-api
spec:
  url: https://my-service/health
  interval: 30s
  metricLabels:
    team: payments
    env: production
    tier: critical
```

All metrics for this probe include those labels:

```
synthetics_probe_up{name="checkout-api", namespace="default", kind="HTTPProbe", team="payments", env="production", tier="critical"} 1
```

This unlocks per-team Alertmanager rules:

```yaml
alert: TeamProbeFailure
expr: synthetics_consecutive_failures{team="payments"} >= 3
```

And per-team Grafana dashboard variables — filter by `team` without needing separate namespaces.

These labels are distinct from Kubernetes metadata labels on the CRD — `spec.metricLabels` is explicitly for Prometheus metric labels, giving users precise control over metric cardinality. This is a deliberate separation: Kubernetes `metadata.labels` remain for selectors, policy, and GitOps tooling; `spec.metricLabels` is for observability. High-cardinality values (e.g. `user_id`) should be avoided as they create unbounded metric series.

---

## 4. Controller Architecture

> This section describes both the Phase 1 shipping shape and the later target architecture. Phase 1 is intentionally smaller: one operator deployment, in-process HttpProbe execution, in-memory OTel instruments, and no NATS, CronJob-backed tests, or deployment split yet.

### 4.1 Phase 1 deployment architecture

Phase 1 ships a single `synthetics-operator` Deployment created by the Helm chart.

| Component | Replicas | Responsibilities in Phase 1 |
|---|---|---|
| `synthetics-operator` | 1 | Reconcile `HttpProbe`, run the in-process worker pool, expose `/metrics`, serve webhooks, and manage webhook certs |

Phase 1 execution path is deliberately direct:

```
HttpProbe CRD → reconcile into in-memory schedule → worker pool executes HTTP GET → OTel instruments updated in process → /metrics
```

The worker pool never writes back to the Kubernetes API. Probe results update the in-memory metrics store only. CR status is written exclusively by the reconciler, and only on spec changes (`GenerationChangedPredicate`). This keeps API server write load proportional to the rate of configuration changes, not the rate of probe executions.

Deferred from Phase 1:

- NATS work queue and result stream
- Separate `controller`, `probes`, and `metrics` deployments
- Separate `synthetics-operator-webhook` deployment
- `DnsProbe`, `PlaywrightTest`, and `K6Test`

This keeps the MVP aligned with the Phase 1 deliverable: declare an `HttpProbe`, scrape `/metrics`, alert on pass/fail.

### 4.2 Target deployment architecture (Phase 8+)

Four deployments with independent scaling characteristics:

| Deployment | Default replicas | Scales to | Responsibilities |
|---|---|---|---|
| `synthetics-operator-controller` | 1 | 1 | Reconcile CRDs, manage CronJobs, status conditions, cert rotation |
| `synthetics-operator-probes` | 1 | N | HttpProbe + DnsProbe execution, publish results to NATS |
| `synthetics-operator-metrics` | 1 | N | Consume NATS result stream, serve `/metrics` |
| `synthetics-operator-webhook` | 2 | N | Validate + default CRDs |

NATS JetStream is the message bus connecting all components. CronJob sidecars publish directly to NATS — they never talk to the operator.

```
controller          → NATS work queue  ← probe-workers (compete for jobs)
CronJob sidecars    → NATS result stream
probe-workers       → NATS result stream
NATS result stream  → metrics-consumer → /metrics
```

### 4.3 Probe scheduling

In Phase 1, `HttpProbe` scheduling is in-process. The reconciler registers each probe with the local scheduler, which computes a stable offset using `probeOffset(namespace, name, interval)` and enqueues runs onto an in-memory worker pool. No external queue exists yet; a pod restart causes a brief scheduling gap until the operator rebuilds the schedule from watched CRDs.

From Phase 15 onward, the same offset algorithm is reused with NATS as the transport. The controller publishes jobs to the NATS work queue at the scheduled time and probe workers compete to consume them.

The controller publishes a job to the NATS work queue at the scheduled time for each HttpProbe and DnsProbe. Probe workers compete to consume jobs — NATS delivers each job to exactly one worker. On ACK, the message is removed. On worker crash, NATS redelivers after a timeout.

Rather than jitter (which is random and can still cluster), each probe's publish time is derived deterministically from a hash of its `namespace/name`:

```go
func probeOffset(namespace, name string, interval time.Duration) time.Duration {
    h := fnv.New64a()
    h.Write([]byte(namespace + "/" + name))
    return time.Duration(h.Sum64() % uint64(interval))
}
```

This gives each probe a stable offset within its interval that is independent of all other probes. Adding or removing a probe does not affect any other probe's schedule, and the operator restarting produces identical offsets — no stored state required.

Probes with different intervals are bucketed independently. A 10s probe and a 30s probe cannot be distributed relative to each other and are not.

Distribution quality: for N probes uniformly hashed across an interval, the expected maximum gap is approximately `interval × ln(N) / N`. At 100 probes on a 30s interval that is ~10ms — negligible. At 10 probes it is ~70ms — still fine for any realistic probe interval.

> **Why not midpoint insertion?** Inserting each probe at the midpoint of the largest existing gap requires maintaining sorted global state and breaks on operator restart unless the assignment is persisted. It also clusters badly when probes are removed, leaving gaps that only partially fill when new probes arrive. Hash-based offsets are simpler, stateless, and stable.

Workers are stateless — a plain Deployment. Scale by adding replicas; NATS distributes work automatically. No sharding, no StatefulSet, no rebalancing on scale changes.

If the controller restarts, no jobs are published during the gap (~5–10s). In-flight probe workers finish their current jobs normally; the work queue drains and sits idle until the controller recovers.

### 4.4 CronJob management (PlaywrightTest + K6Test)

The operator reconciles PlaywrightTest and K6Test CRDs into Kubernetes CronJobs. `spec.interval` is converted to a CronJob schedule string with a per-probe offset derived from `probeOffset()` — preventing clustering when many tests share the same interval. Since cron has 1-minute resolution, the offset is `probeOffset() % interval_in_minutes`, giving a stable minute-level slot within the interval window. Key invariants:

- Never run two Jobs concurrently for the same probe — controller checks for existing running Jobs before creating a new one
- `ttlAfterFinished` defaults to 1h if omitted — enforced by the defaulting webhook to prevent pod accumulation
- Pod eviction detected via Job failure status — operator records a failure metric directly if a pod disappears without reporting results

**Job rejection handling** — in locked-down clusters, Jobs created by the operator can be rejected by ResourceQuota, LimitRange, or admission webhooks. Without surfacing this, the CronJob silently never runs and `synthetics_probe_up` just stops updating.

Two layers ship in Phase 5 (when K6Test lands):

_Layer 1 — detect and surface._ If `Job.Create` returns a 403 or 422, the operator records `synthetics_operator_cronjob_rejected_total`, sets the `Ready` condition to `False` with `reason: JobCreationFailed` and the API server's rejection message, and emits a Kubernetes Event on the CRD object. Events appear in `kubectl describe`, which is where most people look first.

```yaml
status:
  conditions:
    - type: Ready
      status: "False"
      reason: JobCreationFailed
      message: "exceeded quota: cpu requests 500m > remaining 200m"
      lastTransitionTime: "2026-04-04T12:00:00Z"
```

`JobCreationFailed` is a reason on the `Ready` condition, not a separate condition type. The condition set stays stable and predictable; the reason communicates what went wrong.

_Layer 2 — pre-flight check (add if users hit this in practice)._ On reconcile, before creating a Job, check whether the namespace has a ResourceQuota and whether `runner.resources.requests` would exceed remaining capacity. If it would, skip creation, set the status condition, and requeue with backoff — avoiding the create-fail-create-fail noise loop.

A troubleshooting section in the docs covers the common case: "if your K6Test never runs, check `kubectl describe k6test <name>` and look for `Ready=False` with `reason: JobCreationFailed`. Verify `runner.resources` fits within namespace quotas."

### 4.5 Result ingestion

In Phase 1 there is no result transport layer. The in-process HttpProbe worker updates OTel instruments directly after each run and the same process exposes `/metrics`. Results never flow back to the Kubernetes API — the CR is a config object, not a live status board. Runtime state (success, duration, consecutive failures, cert expiry) lives in the metrics store and is visible via Prometheus.

NATS-backed result ingestion is introduced later for CronJob-backed runners and independent scaling:

All results flow through NATS — probe workers publish in-process results, CronJob sidecars publish after the test completes. The metrics consumer subscribes to the result stream and updates OTel instruments. No HTTP ingest endpoint, no shared in-memory state, no single-writer constraint.

**Probe worker flow:**

```
1. Controller publishes job to NATS work queue
2. Probe worker consumes job, runs HttpProbe/DnsProbe
3. Worker publishes result to NATS result stream, ACKs job
4. Metrics consumer receives result, updates OTel instruments
```

**CronJob sidecar flow:**

```
1. Main container runs test, writes raw output to /results/output.json (shared volume)
2. Sidecar normalizes output, publishes result to NATS result stream
3. Job completes (native sidecar terminates automatically — requires Kubernetes 1.33+)
4. Metrics consumer receives result, updates OTel instruments
```

The main container stays completely stock — users can pin to official `mcr.microsoft.com/playwright` or `grafana/k6` images without modification. The `test-sidecar` handles NATS publishing across all test types.

**Result message:**

```json
{
  "kind": "PlaywrightTest",
  "probeName": "checkout-flow",
  "success": true,
  "timestamp": "2026-04-04T12:00:00Z",
  "durationMs": 4200
}
```

**Metrics consumer:**

Subscribes to the NATS result stream and records results directly into OTel instruments (gauges, counters). The OTel Prometheus exporter serves current instrument state on `/metrics` at scrape time. Multiple metrics-consumer replicas can each subscribe to the full stream — all replicas hold identical state and serve identical `/metrics`. Prometheus can scrape any of them.

On restart, instrument state is lost until results arrive from each probe's next run — typically within one interval. Prometheus shows a gap, not stale data.

**Sidecar NATS auth:**

The sidecar authenticates to NATS using its pod ServiceAccount token. NATS is configured with a callout to the operator controller for token validation via the Kubernetes TokenReview API. Runner pods require no Kubernetes API write permissions.

**Condition set** — two conditions, stable across all CRD types:

| Condition | Meaning |
|-----------|---------|
| `Ready` | Whether the probe is functioning normally. `False` with a `reason` when something is wrong. |
| `Suspended` | Whether `spec.suspend` is true. Separate from `Ready` so a suspended probe is clearly distinct from a failing one. |

`Ready` reasons:

| Reason | Set when | Set by |
|--------|----------|--------|
| `Initializing` | Probe registered, first run not yet complete | Reconciler (on spec change) |
| `JobCreationFailed` | CronJob pod rejected by quota/LimitRange/admission webhook | Reconciler (on Job watch) |
| `ConfigError` | Probe spec is invalid (bad URL, unreachable resolver) | Reconciler (on Job watch) — future |
| `ProbeSucceeded` | Last run passed all assertions | Future (status writes deferred) |
| `ProbeFailed` | Last run failed assertions or timed out | Future (status writes deferred) |

For HttpProbe and DnsProbe, `Ready` transitions to `ProbeSucceeded`/`ProbeFailed` are deferred. Runtime pass/fail state is available via `synthetics_probe_up` and `synthetics_consecutive_failures` metrics, which update on every run without touching the Kubernetes API. This keeps API server write load proportional to configuration changes, not probe execution frequency.

**Crash handling:**

- If the main container crashes before producing results, the sidecar has nothing to publish; the controller derives failure from Job/Pod status via the Job watch and sets `Ready=False`.
- If the sidecar fails to connect to NATS, the result is lost. The next scheduled run restores state. Prometheus shows a gap in the affected metric. With JetStream enabled, NATS buffers messages across restarts — the sidecar retries until NATS is available.

### 4.6 CRD webhooks

Phase 1 ships validating and defaulting webhooks in the same operator process as reconciliation, scheduling, and `/metrics`. This satisfies the MVP requirement for immediate spec validation without adding deployment split complexity on day one.

In Phase 7, webhooks move to a **separate `synthetics-operator-webhook` deployment** — stateless, no leader election, 2+ replicas. This keeps `kubectl apply` available even while the main operator is restarting. The webhook deployment still shares the same binary, started with a `--webhook-only` flag.

The webhook provides immediate feedback on invalid resources at apply time rather than surfacing errors later via status conditions.

**Validating webhook** — rejects invalid resources before they are persisted:

```yaml
# Rejected immediately at kubectl apply
kind: HTTPProbe
spec:
  interval: 0s          # nonsensical
  request:
    url: "not-a-url"    # invalid
```

**Defaulting webhook** — fills in sensible defaults before the resource is stored:

```go
func (h *HttpProbe) Default() {
    if h.Spec.Timeout == 0 {
        h.Spec.Timeout = 10 * time.Second
    }
    if h.Spec.Interval == 0 {
        h.Spec.Interval = 30 * time.Second
    }
}

func (k *K6Test) Default() {
    if k.Spec.TTLAfterFinished == 0 {
        k.Spec.TTLAfterFinished = 1 * time.Hour  // prevent pod accumulation
    }
}
```

### 4.7 Certificate management

Webhooks require TLS — the operator manages its own certificates with no external dependencies. cert-manager is explicitly not required.

In Phase 1, the same operator process both serves the webhook and manages cert rotation. From Phase 7 onward, cert management is split across two deployments: the controller generates and rotates certs, writing them to a shared Secret, and webhook replicas watch that Secret and hot-reload certs via an atomic pointer swap.

**Startup sequence (operator):**

```
1. Check if synthetics-webhook-certs Secret exists
     → if not, generate self-signed CA + serving cert, store in Secret
     → if yes, load from Secret and check expiry
2. Compare Secret CA against caBundle in ValidatingWebhookConfiguration
     → if they diverge, re-inject caBundle (self-heals after a crashed rotation)
3. Start background rotation goroutine
```

**Startup sequence (webhook replicas):**

```
1. Load cert from synthetics-webhook-certs Secret
2. Start webhook server with GetCertificate reading from atomic pointer
```

**Secret structure** — follows standard naming conventions:

```
synthetics-webhook-certs
  ca.crt
  ca.key
  tls.crt
  tls.key
```

**Rotation** — a background goroutine wakes at a configurable threshold (default: when 20% of TTL remains) and regenerates certs without restarting the operator. Default cert TTL is 90 days.

**Cert reload mechanism** — the webhook server's `tls.Config` never holds a cert directly. It uses `GetCertificate`, which reads from an `atomic.Pointer[tls.Certificate]`. A Secret informer reloads the in-memory serving cert on update. No lock contention, no race, no restart.

```go
type CertManager struct {
    secretName string
    dnsNames   []string
    certTTL    time.Duration  // default 90 days
    rotateAt   float64        // rotate when X% of TTL remaining, default 0.2
    currentCert atomic.Pointer[tls.Certificate]  // hot-swappable
}

tlsConfig := &tls.Config{
    GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
        return cm.currentCert.Load(), nil
    },
}
```

**Rotation path (operator):**

```
1. Goroutine wakes when 20% of TTL remains
2. Generate new CA + serving cert
3. Write to synthetics-webhook-certs Secret (atomic update)
4. Update caBundle in ValidatingWebhookConfiguration
```

**Reload path (all replicas, including leader):**

```
1. Informer watches synthetics-webhook-certs Secret
2. On update event, parse new tls.crt + tls.key from Secret data
3. Swap atomic pointer: cm.currentCert.Store(&newCert)
4. Next TLS handshake picks up new cert automatically
```

No fsnotify against Secret mounts, no pod restart. The informer watches the Secret through the Kubernetes API and the `OnUpdate` handler parses and swaps the in-memory certificate immediately. A periodic reconcile still rotates certs and re-injects the CA bundle before expiry.

**caBundle divergence edge case** — if the operator rotates the cert but crashes before updating the WebhookConfiguration's `caBundle`, the new cert is signed by a CA the API server doesn't trust. Webhook calls fail. On startup, the operator checks whether the CA in the Secret matches the `caBundle` in the WebhookConfiguration and re-injects if they diverge. This self-heals on the next restart.

Webhook replicas watch the Secret via informer and reload certs on change via atomic pointer swap.

### 4.8 Availability

Phase 1 availability is intentionally simple:

- One operator replica
- Brief probe scheduling gap during restart
- Webhooks unavailable during that restart window

This is acceptable for the MVP and is the explicit reason Phase 7 exists.

**`synthetics-operator-webhook` (2+ replicas)**
- Stateless — any replica handles any webhook call
- PodDisruptionBudget ensures at least one replica is always up

**`synthetics-operator-controller` (1 replica)**
- Single replica; no leader election needed
- On restart (~5–10s): no jobs published to work queue; probe workers drain existing jobs and idle until controller recovers; CronJobs continue running unaffected
- Reconcile is idempotent — always diffs current vs desired state
- Graceful shutdown on SIGTERM — in-flight reconciles complete before exit

**`synthetics-operator-probes` (1+ replicas)**
- Stateless — any worker consumes any job from the NATS work queue
- Scale up to increase probe throughput; scale down without resharding

**`synthetics-operator-metrics` (1+ replicas)**
- Each replica consumes the full NATS result stream — all replicas hold identical OTel instrument state
- Scale up for `/metrics` scrape availability; all replicas serve identical data

**NATS**
- Default: 1 replica, no JetStream — results are best-effort, gaps acceptable
- With JetStream + PVC: results buffered across restarts — no gaps during controller or metrics-consumer downtime
- With 3 replicas: NATS cluster, survives single-node failure

### 4.9 Resource footprint

Phase 1 footprint is a single operator pod with enough headroom for reconciliation, webhook serving, metrics export, and the in-process worker pool. The per-deployment footprint below is the later split architecture.

```yaml
# synthetics-operator-controller
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi

# synthetics-operator-probes (per replica)
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi

# synthetics-operator-metrics (per replica)
resources:
  requests:
    cpu: 10m
    memory: 32Mi
  limits:
    cpu: 100m
    memory: 128Mi

# synthetics-operator-webhook (per replica)
resources:
  requests:
    cpu: 10m
    memory: 32Mi
  limits:
    cpu: 50m
    memory: 64Mi

# nats (per replica)
resources:
  requests:
    cpu: 10m
    memory: 32Mi
  limits:
    cpu: 100m
    memory: 64Mi
```

### 4.10 RBAC summary

Phase 1 RBAC is a reduced subset of the target model: the single operator Deployment needs CRD, Secret, webhook configuration, Event, and leader-election style access, but not NATS-specific worker separation or CronJob test permissions yet.

**`synthetics-operator-controller` ClusterRole:**

| Resource | Verbs | Why |
|----------|-------|-----|
| `httpprobes`, `dnsprobes`, `playwrighttests`, `k6tests` | get, list, watch, update, patch | Reconcile CRDs, update status conditions |
| `jobs`, `cronjobs` | get, list, watch, create, update, delete | Manage CronJobs for script-based tests |
| `configmaps` | get, list, watch, create, update, patch | Read scripts, manage webhook ConfigMaps |
| `secrets` | get, list, watch, create, update | Webhook certs Secret |
| `validatingwebhookconfigurations`, `mutatingwebhookconfigurations` | get, update, patch | Inject caBundle |
| `leases` | get, create, update | controller-runtime informer health |
| `events` | create, patch | Emit Kubernetes events |
| `tokenreviews` | create | Validate sidecar ServiceAccount tokens for NATS auth callout |

**`synthetics-operator-probes` ClusterRole:**

| Resource | Verbs | Why |
|----------|-------|-----|
| `httpprobes`, `dnsprobes` | get, list, watch | Read probe specs to execute |

**`synthetics-operator-metrics` ClusterRole:**

| Resource | Verbs | Why |
|----------|-------|-----|
| — | — | No Kubernetes API access needed |

**`synthetics-operator-webhook` ClusterRole:**

| Resource | Verbs | Why |
|----------|-------|-----|
| `httpprobes`, `dnsprobes`, `playwrighttests`, `k6tests` | get, list, watch | Read CRD schemas for validation |
| `secrets` | get, watch | Load webhook serving cert |

Runner pod ServiceAccounts require no Kubernetes API permissions. Sidecars authenticate to NATS via their pod ServiceAccount token; the controller validates tokens via TokenReview on the NATS auth callout. See section 4.4.

### 4.10 NetworkPolicy

Runner pods publish results directly to NATS using their pod ServiceAccount token. The controller validates tokens via a NATS auth callout — the token grants no Kubernetes API write permissions. A rogue pod with a stolen token can publish a fabricated result but cannot modify CRDs, ConfigMaps, or any other cluster resource.

A default NetworkPolicy is shipped in the Helm chart. Opt-in because not every cluster has a CNI that enforces NetworkPolicy.

```yaml
# values.yaml
networkPolicy:
  enabled: false   # opt-in, requires CNI support (Calico, Cilium, etc.)
  prometheusNamespace: monitoring
```

When enabled, separate policies are generated per deployment:

- `synthetics-operator-metrics`: `:8080` from monitoring namespace, `:8081` from kubelet
- `synthetics-operator-webhook`: `:9443` from all namespaces (API server), `:8081` from kubelet
- `synthetics-operator-controller`: `:8081` from kubelet only
- `synthetics-operator-probes`: `:8081` from kubelet only
- `nats`: `:4222` (client) from test namespaces + operator namespace, `:6222` (cluster) between NATS pods only

Egress is not restricted — probe workers need to reach arbitrary external endpoints.

---

## 5. Helm Configuration

### 5.1 NATS scaling tiers

```yaml
# Small — default, single team, <100 probes
nats:
  replicas: 1
  jetstream:
    enabled: false

# Medium — multiple teams, 100–1000 probes
# JetStream buffers results across restarts — no gaps during controller or metrics-consumer downtime
nats:
  replicas: 1
  jetstream:
    enabled: true
    storage:
      size: 5Gi
      storageClass: ""   # uses cluster default

# Large — org-wide, 1000+ probes
# 3-replica NATS cluster survives single-node failure; multiple operator replicas for throughput
nats:
  replicas: 3
  jetstream:
    enabled: true
    storage:
      size: 20Gi
      storageClass: ""

operator:
  probes:
    replicas: 3   # probe workers scale independently
  metrics:
    replicas: 2   # all replicas serve identical /metrics
```

### 5.2 Scheduling configuration

Each deployment exposes the standard Kubernetes scheduling primitives. Defaults are empty (no constraints) except where noted.

```yaml
webhook:
  priorityClassName: system-cluster-critical  # default: elevated — webhook down blocks all CRD applies
  nodeSelector: {}
  tolerations: []
  affinity: {}
  topologySpreadConstraints: []

controller:
  priorityClassName: ""
  nodeSelector: {}
  tolerations: []
  affinity: {}
  topologySpreadConstraints: []

probes:
  priorityClassName: ""
  nodeSelector: {}    # e.g. restrict to nodes with external network access
  tolerations: []
  affinity: {}
  topologySpreadConstraints: []

metrics:
  priorityClassName: ""
  nodeSelector: {}
  tolerations: []
  affinity: {}
  topologySpreadConstraints: []

nats:
  priorityClassName: ""
  nodeSelector: {}
  tolerations: []
  affinity: {}
  topologySpreadConstraints: []
```

`priorityClassName` on the webhook defaults to `system-cluster-critical` — the same class used by CoreDNS and kube-proxy. If the cluster is under resource pressure and the webhook pod is evicted, `kubectl apply` fails cluster-wide for all CRD types. The other deployments default to empty (inheriting the namespace default) since probe gaps during pressure are acceptable.

The `runner` block on `K6Test` and `PlaywrightTest` exposes the same fields for CronJob pods — see section 2.5.

### 5.3 Repository Structure

Phase 1 only needs a subset of this layout: `HttpProbe` API types, a controller, in-process scheduler/worker code, shared metrics wiring, and the Helm chart. The tree below is the target repository structure after later phases land.

```
/api/v1alpha1                   # CRD type definitions
/controllers                    # Reconcile logic per CRD (runs in controller deployment)
  http_probe_controller.go
  dns_probe_controller.go
  playwright_test_controller.go
  k6_test_controller.go
  /internal
    scheduler.go                # Work queue job publishing + even distribution
    runner.go                   # Shared CronJob lifecycle management
    certmanager.go              # Cert generation, rotation, reload
/probes                         # Probe worker (runs in probes deployment)
  worker.go                     # NATS work queue consumer, probe execution
  /internal
    metrics.go                  # Shared OTel meter, instrument definitions
/metrics                        # Metrics consumer (runs in metrics deployment)
  consumer.go                   # NATS result stream consumer, OTel instrument updates
/images
  /test-sidecar                 # Sidecar image for publishing TestResult JSON to NATS
/charts
  /synthetics-operator          # Helm chart for the operator
/dashboards
  /grafana
    overview.json               # All probe types, health at a glance
    http-probes.json            # HttpProbe drilldown
    playwright-tests.json       # PlaywrightTest drilldown
    k6-tests.json               # K6Test results and trends
/alerts
  /prometheus
    rules.yaml                  # Recording rules + default alert rules
/examples                       # Sample CRDs for each type
```

---

## 6. Grafana Dashboards & Alerts

### 6.1 Dashboard distribution

Dashboards are distributed in two ways:

- As ConfigMaps via the Helm chart — Grafana sidecar auto-imports on install (opt-in, enabled by default if Grafana is detected)
- Published to grafana.com with stable IDs for manual import

```yaml
# values.yaml
grafana:
  dashboards:
    enabled: true   # Creates ConfigMaps with grafana dashboard label
```

### 6.2 Default alert rules

Conservative defaults that users can override. The operator emits metrics — Alertmanager owns the alerting.

```yaml
groups:
  - name: synthetics
    rules:
      - alert: ProbeFailure
        expr: synthetics_consecutive_failures >= 3
        labels:
          severity: warning

      - alert: ProbeNotRunning
        expr: time() - synthetics_last_run_timestamp > 300
        labels:
          severity: warning

      - alert: LoadTestFailing
        expr: synthetics_loadtest_passed == 0
        labels:
          severity: warning

      - alert: HighLatency
        expr: synthetics_probe_duration_ms > 1000
        labels:
          severity: warning

      - alert: ProbeConfigError           #
        expr: synthetics_probe_config_error == 1
        for: 5m
        labels:
          severity: warning
```

---

## 7. Testing Strategy

Testing an operator has unique challenges — the reconcile loop, CRD lifecycle, and webhook behaviour all need different approaches. The strategy uses three layers with increasing scope and decreasing frequency.

### 7.1 Unit tests

Pure Go, no cluster needed. Fast, run on every PR.

- Cert generation and rotation logic
- Hash-based probe offset algorithm (`FNV64(namespace/name) % interval`)
- Probe result parsing (k6 summary JSON, Playwright output)
- Metric recording logic
- Webhook validation and defaulting functions — these are just Go functions, trivially testable
- Intake payload normalization and in-memory metrics map update logic

### 7.2 Controller tests with envtest

envtest ships with controller-runtime and spins up a real API server and etcd locally without a full cluster. This is the sweet spot for operator testing — full reconcile loop against real CRDs, no kind required.

What to cover:

- Reconcile creates the correct CronJob from a `K6Test` or `PlaywrightTest` spec
- CronJob pods include test-sidecar with correct ServiceAccount
- Updating a probe interval updates the CronJob schedule
- Deleting a probe cleans up owned resources
- `depends` suppression — create two probes, mark one unhealthy, verify the other is suppressed in metrics
- Probe worker publishes result to NATS, metrics consumer receives it and values appear correctly on `/metrics`
- Job watch updates status conditions correctly on Job success/failure
- Webhook validation rejects invalid specs
- Webhook defaulting fills missing fields
- `suspend: true` pauses in-operator probes and sets CronJob `suspend`
- `ttlAfterFinished` defaults applied by webhook

Covers ~80% of operator behaviour, runs in CI in under a minute.

### 7.3 Integration tests with kind

Full cluster, real Jobs running, real Playwright and k6 images. Slower but tests the complete path end to end.

- `PlaywrightTest` CronJob runs, script executes, sidecar publishes to NATS, metrics appear on `/metrics`
- `K6Test` runs, thresholds evaluated, summary parsed correctly
- Sidecar retries on transient operator unavailability (simulate by scaling operator to zero briefly)
- Cert rotation — force expiry, verify rotation happens, webhook continues working
- Operator restart — kill the operator pod, verify it recovers and probes resume; verify webhook deployment remains available throughout

- Resource quota rejection — apply a tight ResourceQuota, verify operator records rejection metric and sets status condition

Run on merge to main, not every PR. The current repo uses a kind smoke path for Phase 1: build image, install the Helm chart, apply an `HttpProbe`, and verify both status and `/metrics`.

### 7.4 e2e smoke tests

Installs the operator via Helm against a real cluster (kind in CI or staging), applies one of each CRD type, verifies metrics appear in Prometheus. Catches packaging and install issues that controller tests miss.

### 7.5 Scale tests

Run nightly, not on every PR. Spin up 500+ HttpProbes and verify:

- Hash-based offsets are stable under rapid add/remove churn — existing probe schedules do not shift
- Distribution quality stays acceptable (max gap ≤ 3× expected gap) at 500+ probes
- Worker pool is not exhausted
- Operator memory stays within the defined footprint

### 7.6 CI pipeline

```
PR opened
  → unit tests                          (seconds)
  → envtest controller tests            (< 1 min)
  → lint + vet

Merge to main
  → all of the above
  → kind integration tests              (5-10 min)
  → Helm e2e smoke test

Nightly
  → scale tests (including churn)
  → full kind suite across k8s versions (1.33, 1.34, 1.35, 1.36)
```

### 7.7 Multi-version testing

Test against at least the four most recent k8s minor versions in the nightly suite. CronJob behaviour and Job lifecycle have changed meaningfully across recent releases. kind makes this straightforward:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    image: kindest/node:v1.33.0
```

The native sidecar pattern (initContainer with `restartPolicy: Always`) requires Kubernetes 1.33+, where this feature is GA. This is the minimum supported version for the operator.

---

## 8. Key Design Decisions

| Decision | Outcome |
|----------|---------|
| Opinionated stack | Playwright for browser tests, k6 for load tests only. No arbitrary script runtimes. Reduces complexity, sets clear expectations, enables purpose-built runner images. |
| 4 CRDs, 2 prefixed | `HttpProbe` and `DnsProbe` are protocol-based and stay unprefixed. `PlaywrightTest` and `K6Test` are tool-coupled and prefixed explicitly — leaves room for alternatives like `GatlingTest` later. |
| runner block on K6Test + PlaywrightTest | Pod-level config (env, envFrom, resources, affinity) isolated in a `runner` block, mirroring the k6 operator pattern. Separates test configuration from infrastructure configuration. `envFrom` supports bulk-loading from Secrets and ConfigMaps. |
| Automatic VU distribution | K6Test operator divides total VUs by parallelism using k6 execution segments automatically. Users set total VUs in the script, not per-pod VUs. |
| Thresholds in script only | k6 thresholds defined in the script, not duplicated in the CRD. Avoids drift between two sources of truth. Operator parses threshold results from k6 summary output. |
| No alerting in operator | Operator emits metrics only. Users own Alertmanager rules. Avoids duplicating alerting infrastructure and keeps operator scope focused. |
| Assertions are stateless | CRD spec assertions evaluate the current run only — no sliding windows, no history, no aggregation. Each assertion is a named expression (`variable op value`) evaluated against the probe result. All assertions run on every probe execution; `synthetics_probe_up` reflects the overall pass/fail, while `synthetics_probe_assertion_result` carries a per-assertion 0/1 gauge with `assertion` and `expr` labels. Anything requiring multiple results over time (p95 latency, consecutive failure rate) belongs in Prometheus/Alertmanager rules against emitted metrics, not in the CRD schema. |
| In-operator scheduling | HttpProbe and DnsProbe run as goroutines inside the operator. Sub-minute intervals required; pod-per-run would be wasteful. |
| CronJobs for scripts | PlaywrightTest and K6Test need isolated runtimes and run at minute-or-longer intervals. CronJobs are the natural fit. |
| Hash-based probe offset | Each probe's schedule offset is `FNV64(namespace/name) % interval`. Deterministic across restarts, independent per probe — adding or removing a probe does not affect any other. No global state. Jitter was rejected: random offsets can still cluster and change on restart. Midpoint insertion was rejected: requires sorted global state, breaks on restart without persistence, and degrades badly when probes are removed. |
| NATS as the shared stateful component | CronJob results must go somewhere external to the operator process — a shared stateful component is unavoidable. NATS is the minimal right answer: purpose-built for message routing, ~32Mi idle, no persistence required by default, and keeps every other component stateless. ConfigMaps and an HTTP ingest endpoint were considered; both make either the API server or the operator a scaling bottleneck. See section 1.3. |
| NATS work queue for probe scheduling | Deferred to Phase 15. Phase 1 uses the same hash-based offset algorithm with an in-memory worker pool inside the operator; later the controller publishes jobs to a NATS work queue and stateless probe workers compete to consume them. |
| NATS result stream | All results (probe workers + CronJob sidecars) flow through a single NATS result stream. Decouples writers from the metrics consumer. With JetStream enabled, results are buffered across restarts. |
| test-sidecar pattern | CronJob test pods include a native sidecar (`test-sidecar`) that reads the TestResult JSON and publishes it to NATS. Main container stays completely stock (official Playwright/k6 images). Test pods need no Kubernetes API write permissions. Requires Kubernetes 1.33+. |
| Re-export k6 metrics | Curated k6 summary metrics re-exported by operator rather than forwarding full k6 Prometheus output. Keeps schema clean. |
| depends field | One level deep only. Suppresses failure metrics when a dependency is unhealthy. No orchestration, no chaining — purely noise reduction. |
| CRD webhooks | Phase 1 ships validating and defaulting webhooks in the main operator process. Phase 7 moves them into a separate `synthetics-operator-webhook` deployment (2–3 replicas, stateless) so `kubectl apply` stays available during operator restarts. Same binary, `--webhook-only` flag. |
| Self-managed certs | Operator generates and rotates self-signed CA + serving certs, stored in a Secret. Webhook replicas watch the Secret and hot-reload via atomic pointer swap. No cert-manager dependency. |
| Grafana in repo | Dashboards and alert rules shipped in the repo, distributed via Helm ConfigMaps and grafana.com. Minimises time-to-value. |
| Operator health metrics | NATS queue depth, reconcile errors, CronJob failures, Job rejections, result stream lag, and cert expiry all instrumented via OTel SDK. Exported as Prometheus on `/metrics` via OTel Prometheus exporter. NATS queue depth is the key saturation signal. |
| Custom metric labels | `spec.metricLabels` on all CRDs propagates to OTel metric attributes (Prometheus labels). Enables per-team alerting and dashboard filtering without separate namespaces. Deliberately separate from k8s metadata labels so observability labels can be managed independently and cardinality stays explicit. |
| suspend field | All CRDs support `spec.suspend` to pause probes without deletion. In-operator probes are unscheduled; CronJobs set `suspend: true`. |
| Consistent interval syntax | All four CRDs use `interval` (duration string). For CronJob-backed tests the operator converts the interval to a CronJob schedule with a per-test offset for even distribution. Sub-minute intervals (e.g. `10s`) only supported by HttpProbe and DnsProbe; minimum for PlaywrightTest and K6Test is `1m`. |
| OTel SDK, Prometheus export | OTel Go SDK for all instrumentation, exported via OTel Prometheus exporter on `/metrics`. External interface stays Prometheus-compatible; no OTel collector required. |
| Testing layers | Unit tests for pure logic, envtest for controller/webhook behaviour, kind for full end-to-end. envtest covers ~80% of operator behaviour without needing a full cluster. Scale tests include churn scenarios. |
| CI cadence | `gofmt`, `go vet`, unit tests, envtest, and Helm lint/render run on every PR. A kind smoke install runs on pushes to `main` and on demand. A nightly kind matrix covers currently-supported bootstrap versions and can expand as the project adds more phases. |
| Multi-version testing | The nightly kind matrix currently covers Kubernetes 1.33 and 1.34 for the Phase 1 HttpProbe path. Expand the matrix as later phases add CronJob-backed tests and more version-sensitive behaviour. |
| CRD graduation | `v1alpha1` initially. Graduation to `v1beta1` and `v1` driven by schema stability and adoption, with conversion webhooks at each transition. |
| Status writes scoped to config changes | For HttpProbe/DnsProbe, the reconciler writes status only on spec changes (`GenerationChangedPredicate`): `observedGeneration`, `Suspended` condition, and the initial `Ready=Initializing` condition. The worker pool never patches the CR — runtime state (pass/fail, duration, consecutive failures) lives exclusively in the metrics store. This keeps API server write load proportional to configuration changes, not probe execution frequency. `lastRunTime`/`consecutiveFailures` in the CR schema are reserved for a future decision on when/whether to surface per-run state in kubectl output. |
| NetworkPolicy (opt-in) | Helm chart ships per-deployment opt-in NetworkPolicies. NATS client port `:4222` restricted to operator + test namespaces. Metrics `:8080` restricted to monitoring namespace. Webhook `:9443` open to API server. Disabled by default — not every cluster has an enforcing CNI. |
| Cert reload via atomic pointer | Webhook `tls.Config` uses `GetCertificate` reading from `atomic.Pointer[tls.Certificate]`. Leader rotates and writes Secret; all replicas reload via Secret informer `OnUpdate`. No fsnotify, no polling, no restart. Startup re-injects `caBundle` if it diverges from Secret CA (self-heals after crashed rotation). |

---

## 9. Implementation Phases

Each phase ships a usable product. No phase is purely foundational.

### Phase 1 — HttpProbe MVP

**Deliverable:** Users can define HTTP GET checks as CRDs and see pass/fail in Prometheus.

- Project scaffold via controller-runtime with Kubebuilder-style APIs, markers, and CRD/webhook layout
- `HttpProbe` CRD: URL, method, headers, status code assertion, `spec.suspend`
- In-operator worker pool with even distribution scheduling
- OTel instrumentation (`synthetics_probe_up`, `synthetics_probe_duration_ms`, `synthetics_consecutive_failures`, `synthetics_last_run_timestamp`, `synthetics_probe_config_error`) exported via Prometheus exporter on `/metrics`
- Validating and defaulting webhooks for HttpProbe (with self-managed certs)
- Helm chart (operator deployment, RBAC, CRD install, webhook config)
- Unit tests for worker pool, distribution algorithm, webhook functions
- envtest for reconcile loop and webhook behaviour

**Phase 1 implementation profile:**

- Single `synthetics-operator` Deployment
- `HttpProbe` only
- HTTP `GET` only
- In-process scheduler and worker pool
- OTel instruments updated directly by probe execution
- Webhooks served from the same binary and pod as the controller
- No NATS, no CronJobs, no deployment split

**Phase 1 exit criteria:**

- Applying a valid `HttpProbe` creates no secondary resources beyond operator-owned webhook/cert objects
- Applying an invalid `HttpProbe` is rejected at admission time
- A healthy endpoint sets `synthetics_probe_up=1` and updates duration and timestamp metrics
- A failing endpoint sets `synthetics_probe_up=0` and increments `synthetics_consecutive_failures`
- `spec.suspend: true` stops future executions without deleting the CRD
- Operator restart rebuilds schedules from the API server and resumes probing without manual intervention

**Usable because:** teams can monitor HTTP endpoint availability with a declarative CRD. Webhooks catch invalid specs immediately.

---

### Phase 2 — Named assertions + HTTP phase timing

**Deliverable:** Named expression-based assertions for both HTTPProbe and DNSProbe; per-phase HTTP timing breakdown; TLS certificate expiry tracking.

- Named assertion expression language: `variable op value` (operators `=`, `!=`, `<`, `<=`, `>`, `>=`)
- HTTPProbe variables: `status_code`, `duration_ms`, `ssl_expiry_days`
- DNSProbe variables: `answer_count`, `duration_ms`
- All assertions evaluated on every run (no short-circuit on first failure for metrics)
- `synthetics_probe_assertion_result` gauge with `assertion` (name) and `expr` (expression) labels
- `synthetics_probe_up` `reason` label carries the name of the first failing assertion
- `synthetics_probe_http_phase_duration_ms` with `phase` label: `dns`, `connect`, `tls`, `processing`, `transfer`
- `synthetics_probe_tls_cert_expiry_timestamp_seconds`
- Admission webhook validates assertion expressions and variable names at create/update time
- Any HTTP method supported (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, …)
- Custom request headers and arbitrary request body
- TLS config: `insecureSkipVerify`, custom CA certificate (PEM)
- Per-connection transport (no keep-alive reuse) ensures all phases are measured on every run
- Unit tests for expression parser, evaluator, per-probe job logic, and webhook validation

**Usable because:** teams can define precise pass/fail criteria directly in the CRD, see per-assertion status in Grafana, and receive alerts that name the failing check.

---

### Phase 4 — SSL certificate monitoring

**Deliverable:** Track certificate expiry for any HTTPS endpoint already defined as an HttpProbe.

- `spec.ssl.enabled`, `spec.ssl.expiryWarningDays`, `spec.ssl.expiryMinimumDays`, `spec.ssl.verifyChain`
- `synthetics_ssl_expiry_days` and `synthetics_ssl_valid` metrics
- Unit tests for cert expiry calculation

**Usable because:** SSL expiry is a common cause of outages and is naturally co-located with the HTTP probe targeting the same URL.

---

### Phase 5 — DnsProbe

**Deliverable:** DNS resolution checks as a first-class CRD.

- `DNSProbe` CRD: hostname, record type, resolver, answer value assertions
- Full DNSProbe metrics schema (`synthetics_dns_success`, `synthetics_dns_response_ms`, `synthetics_dns_response_first_answer_value`, `synthetics_dns_response_first_answer_type`, `synthetics_dns_response_answer_count`, `synthetics_dns_response_authority_count`, `synthetics_dns_response_additional_count`)
- Validating and defaulting webhooks for DnsProbe
- envtest for DnsProbe reconcile loop and webhook behaviour

**Usable because:** DNS failures are a distinct failure mode from HTTP failures and warrant their own probe type with dedicated assertions.

---

### Phase 6 — Grafana dashboards and alert rules

**Deliverable:** Importable dashboards and default alert rules for HttpProbe and DnsProbe.

- Grafana dashboards for HttpProbe and DnsProbe (overview + per-probe drilldown)
- Default Prometheus alert rules (probe failure, high latency, SSL expiry, probe not running)
- Distributed via Helm ConfigMaps (Grafana sidecar auto-import) and grafana.com

**Usable because:** metrics without dashboards require users to build their own visualisation from scratch. Default rules cover the most common alert cases out of the box.

---

### Phase 7 — Webhook deployment split

**Deliverable:** Separate `synthetics-operator-webhook` deployment so `kubectl apply` stays available during operator restarts.

- `--webhook-only` flag: disables reconcile loop, probe workers, and NATS connections
- PodDisruptionBudget ensures at least one webhook replica is always up

**Usable because:** without this, operator restarts cause `kubectl apply` to fail for all CRD types.

---

### Phase 8 — NATS + CronJob result transport

**Deliverable:** Shared result transport infrastructure required by all CronJob-backed tests.

- NATS deployment (single replica, no JetStream by default) added to Helm chart
- `test-sidecar` image: reads TestResult JSON, publishes to NATS result stream with retry
- Operator subscribes to NATS result stream and updates OTel instruments — single `/metrics` endpoint, no deployment split yet
- Helm values for NATS scaling tiers (replicas, JetStream, storage)

In-process probe workers continue to update OTel instruments directly. NATS work queue and deployment split deferred to Phase 15.

**Usable because:** K6Test and PlaywrightTest both depend on this infrastructure. Validating it independently reduces risk when those CRDs ship.

---

### Phase 9 — K6Test

**Deliverable:** k6 load tests defined as CRDs and run on a schedule.

- `K6Test` CRD: script ConfigMap reference, `interval`, `parallelism`, `k6Version`, `ttlAfterFinished`, `runner` block
- CronJob reconciliation with even distribution offset
- Automatic VU distribution across parallel pods using k6 execution segments
- k6 summary JSON parsing in the test-sidecar
- Job rejection detection: `Ready=False` with `reason: JobCreationFailed`, metric, and Kubernetes Event
- k6 Grafana dashboard
- kind integration tests for full Job lifecycle
- envtest for CronJob reconciliation

**Usable because:** teams can schedule load tests declaratively and alert on threshold failures via Prometheus.

---

### Phase 10 — PlaywrightTest

**Deliverable:** Playwright browser tests defined as CRDs and run on a schedule.

- `PlaywrightTest` CRD: script ConfigMap reference, `interval`, `playwrightVersion`, `ttlAfterFinished`, `runner` block
- CronJob reconciliation with even distribution offset
- Playwright JSON reporter output parsing in the test-sidecar
- Per-test metrics with `suite` and `test` labels; suite-level rollups
- Playwright Grafana dashboard
- kind integration tests with real Playwright image

**Usable because:** multi-step browser flows and authenticated journeys can be monitored continuously without a separate Playwright infrastructure.

---

### Phase 11 — `depends` field

**Deliverable:** Suppress failure alerts for probes whose dependencies are already unhealthy.

- `spec.depends` on all CRDs — list of probe references that must be healthy for a failure to be actionable
- Suppression logic: probe still runs, failure metric replaced by `synthetics_probe_suppressed=1`
- One level deep only — no chaining

**Usable because:** a downstream service failing because its upstream is down creates redundant alerts. Suppression reduces noise without hiding the root cause.

---

### Phase 12 — `metricLabels` field

**Deliverable:** Propagate custom labels from CRD spec to all emitted Prometheus metrics.

- `spec.metricLabels` on all CRDs — arbitrary key/value pairs added as OTel attributes
- Webhook validation: rejects high-cardinality label values (e.g. UUIDs)

**Usable because:** teams need to filter and group probe metrics by team, environment, or tier without creating separate namespaces.

---

### Phase 13 — Distribution

**Deliverable:** Project ready for public release and community adoption.

- Multi-version nightly CI matrix (four most recent Kubernetes minor versions)
- goreleaser pipeline for versioned image and chart releases
- Grafana dashboard publication to grafana.com
- OperatorHub / Artifact Hub listing
- Full example library in `/examples`
- Contributing guide and development setup docs

**Usable because:** the project moves from an internal tool to a publishable open source operator with stable release artifacts.

---

### Phase 14 — Horizontal scaling

**Deliverable:** Independent scaling of probe workers and metrics consumer for high probe counts.

- `synthetics-operator-probes` deployment: stateless workers consume jobs from NATS work queue, publish results to NATS result stream
- `synthetics-operator-metrics` deployment: subscribes to full result stream, serves `/metrics`; multiple replicas serve identical data
- Controller publishes scheduled probe jobs to NATS work queue
- In-process probe execution removed from operator binary
- Helm values for replica counts per deployment

**Usable because:** at 1000+ probes the single operator process becomes a bottleneck. This unlocks horizontal scaling without any CRD API or result format changes.
