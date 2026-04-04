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

### 1.3 Why OTel, exposed as Prometheus

The operator uses the OpenTelemetry Go SDK for all internal instrumentation and exports via the OTel Prometheus exporter on `/metrics`. This keeps the external interface compatible with the Prometheus ecosystem (Alertmanager, Grafana, recording rules, existing k8s monitoring stacks) while using OTel as the instrumentation layer. No OTel collector is required — the operator's `/metrics` endpoint is scraped directly by whatever Prometheus is already in the cluster.

---

## 2. Custom Resource Definitions

The operator defines four CRDs, split into two execution models: lightweight network probes that run in-process within the operator, and script-based runners that execute as Kubernetes CronJobs.

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

HTTP checks with assertions on status code, response latency, and body content. SSL certificate monitoring is included as a field on HttpProbe rather than a separate CRD, since both target the same URL.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: HttpProbe
metadata:
  name: api-health
spec:
  interval: 30s
  timeout: 10s
  suspend: false          # pause without deleting
  request:
    url: https://my-service/health
    method: GET
    headers:
      Authorization: "Bearer ${SECRET_TOKEN}"
    body: '{"ping": true}'          # optional request body
    tls:                             # optional mTLS client certs
      clientCertSecret:
        name: mtls-client-cert
        certKey: tls.crt
        keyKey: tls.key
  assertions:
    status: 200
    latency:
      maxMs: 300
    body:
      contains: "ok"
      json:
        - path: "$.status"
          equals: "healthy"
  ssl:
    enabled: true
    expiryWarningDays: 30
    expiryMinimumDays: 7
    verifyChain: true
  depends:
    - kind: DnsProbe
      name: api-dns
  metricLabels:
    team: payments
    env: production
    tier: critical
```

### 2.3 DnsProbe

DNS resolution checks targeting a hostname. Supports all common record types. An optional `resolver` field allows checking against a specific DNS server rather than the cluster default — useful for debugging split-horizon DNS.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: DnsProbe
metadata:
  name: api-dns
spec:
  interval: 1m
  timeout: 5s
  suspend: false          #
  hostname: my-service.com
  recordType: A   # A, AAAA, CNAME, MX, TXT, NS
  assertions:
    resolvedAddresses:
      contains: "1.2.3.4"
    responseTimeMs: 100
    maxResolvedAddresses: 20   # cardinality cap, see 3.4
  resolver: "8.8.8.8:53"   # optional
```

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
    - kind: HttpProbe
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

All four CRD types use `interval` (duration string). For PlaywrightTest and K6Test, the operator converts the interval to a CronJob schedule string and applies a per-probe offset using the same gap-filling even distribution algorithm used for in-operator probes. This prevents clustering when multiple tests share the same interval. Minimum interval for CronJob-backed probes is `1m` (cron resolution limit); sub-minute intervals are only supported by HttpProbe and DnsProbe.

### 2.5 K6Test

k6 scripts executed on an interval or triggered externally via CI. Supports distributed execution via parallelism — the operator automatically divides VUs across runner pods using k6 execution segments, users just set total VUs in the script. A `runner` block configures all pod-level concerns separately from test configuration.

Thresholds are defined in the k6 script itself — the script is the source of truth. The operator parses threshold pass/fail results from k6 summary output; there is no `spec.thresholds` field on the CRD.

```yaml
apiVersion: synthetics.dev/v1alpha1
kind: K6Test
metadata:
  name: api-load
spec:
  interval: 2h              # optional, minimum 1m
  runOnDeploy: false        # trigger on new deployment
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
  - kind: HttpProbe
    name: auth-service
  - kind: DnsProbe
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
| `synthetics_probe_success` | gauge 0\|1 | Whether the last probe run succeeded |
| `synthetics_consecutive_failures` | gauge | Number of consecutive failures since last success |
| `synthetics_last_run_timestamp` | gauge | Unix timestamp of last probe execution |
| `synthetics_probe_suppressed` | gauge 0\|1 | Probe failed but suppressed due to unhealthy dependency |
| `synthetics_probe_config_error` | gauge 0\|1 | Probe is misconfigured (bad URL, unreachable resolver, invalid script). Distinct from execution failure. |

### 3.3 HttpProbe metrics

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_probe_duration_ms` | gauge | Response time of the current request in milliseconds. |
| `synthetics_probe_status_code` | gauge | HTTP status code returned |
| `synthetics_probe_assertion_passed` | gauge 0\|1 | Per-assertion result with assertion label |
| `synthetics_ssl_expiry_days` | gauge | Days until SSL certificate expiry |
| `synthetics_ssl_valid` | gauge 0\|1 | Whether the certificate chain is valid |

### 3.4 DnsProbe metrics

| Metric | Type | Description |
|--------|------|-------------|
| `synthetics_dns_success` | gauge 0\|1 | Whether DNS resolution succeeded |
| `synthetics_dns_response_ms` | gauge | DNS response time in milliseconds |
| `synthetics_dns_resolved_address` | gauge | One series per resolved address (value always 1). **Capped at `maxResolvedAddresses` (default 20)** to prevent cardinality explosion with CDN hostnames that rotate IPs. If the response exceeds the cap, addresses are truncated and `synthetics_dns_resolved_truncated` is set to 1. |
| `synthetics_dns_resolved_truncated` | gauge 0\|1 | Set if resolved addresses exceeded the cap |

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
kind: HttpProbe
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
synthetics_probe_success{name="checkout-api", namespace="default", kind="HttpProbe", team="payments", env="production", tier="critical"} 1
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

### 4.1 Deployment architecture

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

### 4.2 Probe scheduling via NATS work queue

The controller publishes a job to the NATS work queue at the scheduled time for each HttpProbe and DnsProbe. Probe workers compete to consume jobs — NATS delivers each job to exactly one worker. On ACK, the message is removed. On worker crash, NATS redelivers after a timeout.

Rather than jitter (which is random and can still cluster), jobs are published using the same gap-filling even distribution algorithm — the controller staggers publish times deterministically across each interval.

> **Example:** 6 probes on a 30s interval have jobs published at 0s, 5s, 10s, 15s, 20s, 25s — guaranteed even spread with no clustering.

Workers are stateless — a plain Deployment. Scale by adding replicas; NATS distributes work automatically. No sharding, no StatefulSet, no rebalancing on scale changes.

If the controller restarts, no jobs are published during the gap (~5–10s). In-flight probe workers finish their current jobs normally; the work queue drains and sits idle until the controller recovers.

### 4.3 CronJob management (PlaywrightTest + K6Test)

The operator reconciles PlaywrightTest and K6Test CRDs into Kubernetes CronJobs. `spec.interval` is converted to a CronJob schedule string with a per-probe offset derived from the gap-filling even distribution algorithm — preventing clustering when many tests share the same interval. Key invariants:

- Never run two Jobs concurrently for the same probe — controller checks for existing running Jobs before creating a new one
- `ttlAfterFinished` defaults to 1h if omitted — enforced by the defaulting webhook to prevent pod accumulation
- Pod eviction detected via Job failure status — operator records a failure metric directly if a pod disappears without reporting results

**Job rejection handling** — in locked-down clusters, Jobs created by the operator can be rejected by ResourceQuota, LimitRange, or admission webhooks. Without surfacing this, the CronJob silently never runs and `synthetics_probe_success` just stops updating.

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

### 4.4 Result ingestion via NATS

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

The main container stays completely stock — users can pin to official `mcr.microsoft.com/playwright` or `grafana/k6` images without modification. The `results-writer` sidecar handles normalization and NATS publishing across all runner types.

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

| Reason | Set when |
|--------|----------|
| `ProbeSucceeded` | Last run passed all assertions |
| `ProbeFailed` | Last run failed assertions or timed out |
| `JobCreationFailed` | CronJob pod rejected by quota/LimitRange/admission webhook |
| `ConfigError` | Probe spec is invalid (bad URL, unreachable resolver) |
| `Initializing` | Probe registered, first run not yet complete |

Status conditions are updated by the controller's Job watch — not the result stream — so they remain accurate even if a result message is lost.

**Crash handling:**

- If the main container crashes before producing results, the sidecar has nothing to publish; the controller derives failure from Job/Pod status via the Job watch and sets `Ready=False`.
- If the sidecar fails to connect to NATS, the result is lost. The next scheduled run restores state. Prometheus shows a gap in the affected metric. With JetStream enabled, NATS buffers messages across restarts — the sidecar retries until NATS is available.

### 4.5 CRD webhooks

Validating and defaulting webhooks run in a **separate `synthetics-operator-webhook` deployment** — stateless, no leader election, 2+ replicas. This means `kubectl apply` remains available even while other operator components are restarting. The webhook deployment shares the same binary but starts with a `--webhook-only` flag.

The webhook provides immediate feedback on invalid resources at apply time rather than surfacing errors later via status conditions.

**Validating webhook** — rejects invalid resources before they are persisted:

```yaml
# Rejected immediately at kubectl apply
kind: HttpProbe
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

### 4.6 Certificate management

Webhooks require TLS — the operator manages its own certificates with no external dependencies. cert-manager is explicitly not required.

Cert management is split across the two deployments: the operator generates and rotates certs, writing them to a shared Secret. Webhook replicas watch that Secret and hot-reload certs on change via an atomic pointer swap.

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

**Cert reload mechanism** — the webhook server's `tls.Config` never holds a cert directly. It uses `GetCertificate`, which reads from an `atomic.Pointer[tls.Certificate]`. No lock contention, no race, no restart.

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

No fsnotify (fragile with Secret volume mounts), no polling, no restart. The informer is already running — controller-runtime watches Secrets for the webhook cert lifecycle. The only new code is the `OnUpdate` handler that parses and swaps.

**caBundle divergence edge case** — if the operator rotates the cert but crashes before updating the WebhookConfiguration's `caBundle`, the new cert is signed by a CA the API server doesn't trust. Webhook calls fail. On startup, the operator checks whether the CA in the Secret matches the `caBundle` in the WebhookConfiguration and re-injects if they diverge. This self-heals on the next restart.

Webhook replicas watch the Secret via informer and reload certs on change via atomic pointer swap.

### 4.7 Availability

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

### 4.8 Resource footprint

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

### 4.9 RBAC summary

**`synthetics-operator-controller` ClusterRole:**

| Resource | Verbs | Why |
|----------|-------|-----|
| `httpprobes`, `dnsprobes`, `playwrighttests`, `k6tests` | get, list, watch, update, patch | Reconcile CRDs, update status conditions |
| `jobs`, `cronjobs` | get, list, watch, create, update, delete | Manage CronJobs for script runners |
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
- `nats`: `:4222` (client) from runner namespaces + operator namespace, `:6222` (cluster) between NATS pods only

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

### 5.2 Repository Structure

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
  /results-writer               # Sidecar image for normalizing runner output and publishing to NATS
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
- Gap-filling even distribution algorithm
- Probe result parsing (k6 summary JSON, Playwright output)
- Metric recording logic
- Webhook validation and defaulting functions — these are just Go functions, trivially testable
- Intake payload normalization and in-memory metrics map update logic

### 7.2 Controller tests with envtest

envtest ships with controller-runtime and spins up a real API server and etcd locally without a full cluster. This is the sweet spot for operator testing — full reconcile loop against real CRDs, no kind required.

What to cover:

- Reconcile creates the correct CronJob from a `K6Test` or `PlaywrightTest` spec
- CronJob pods include results-writer sidecar with correct ServiceAccount
- Updating a probe interval updates the CronJob schedule
- Deleting a probe cleans up owned resources
- `depends` suppression — create two probes, mark one unhealthy, verify the other is suppressed in metrics
- Posting to `/ingest` updates OTel instruments and values appear correctly on `/metrics`
- Job watch updates status conditions correctly on Job success/failure
- Webhook validation rejects invalid specs
- Webhook defaulting fills missing fields
- `suspend: true` pauses in-operator probes and sets CronJob `suspend`
- `ttlAfterFinished` defaults applied by webhook

Covers ~80% of operator behaviour, runs in CI in under a minute.

### 7.3 Integration tests with kind

Full cluster, real Jobs running, real Playwright and k6 images. Slower but tests the complete path end to end.

- `PlaywrightTest` CronJob runs, script executes, sidecar posts to `/ingest`, metrics appear on `/metrics`
- `K6Test` runs, thresholds evaluated, summary parsed correctly
- Sidecar retries on transient operator unavailability (simulate by scaling operator to zero briefly)
- Cert rotation — force expiry, verify rotation happens, webhook continues working
- Operator restart — kill the operator pod, verify it recovers and probes resume; verify webhook deployment remains available throughout
- `runOnDeploy` — trigger a deployment, verify `K6Test` fires
- Resource quota rejection — apply a tight ResourceQuota, verify operator records rejection metric and sets status condition

Run on merge to main, not every PR.

### 7.4 e2e smoke tests

Installs the operator via Helm against a real cluster (kind in CI or staging), applies one of each CRD type, verifies metrics appear in Prometheus. Catches packaging and install issues that controller tests miss.

### 7.5 Scale tests

Run nightly, not on every PR. Spin up 500+ HttpProbes and verify:

- Even distribution is working correctly across the worker pool
- Even distribution handles rapid add/remove churn without degrading
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
| Assertions are stateless | CRD spec assertions (`status`, `latency.maxMs`, `body`, `resolvedAddresses`) evaluate the current run only — no sliding windows, no history, no aggregation. Anything that requires multiple results over time (p95 latency, consecutive failure rate) belongs in Alertmanager rules against the emitted metrics, not in the CRD schema. |
| In-operator scheduling | HttpProbe and DnsProbe run as goroutines inside the operator. Sub-minute intervals required; pod-per-run would be wasteful. |
| CronJobs for scripts | PlaywrightTest and K6Test need isolated runtimes and run at minute-or-longer intervals. CronJobs are the natural fit. |
| Even distribution over jitter | Probes distributed deterministically using a gap-filling algorithm. Jitter is random and can still cluster; even distribution is guaranteed. |
| NATS work queue for probe scheduling | Controller publishes jobs to a NATS work queue; probe workers compete to consume. Workers are stateless — scale by adding replicas, no resharding. Controller restart causes a brief scheduling gap; workers drain existing jobs normally. |
| NATS result stream | All results (probe workers + CronJob sidecars) flow through a single NATS result stream. Decouples writers from the metrics consumer. With JetStream enabled, results are buffered across restarts. |
| Result writer sidecar pattern | Job pods include a native sidecar (`results-writer`) that normalizes runner output and publishes to NATS. Main container stays completely stock (official Playwright/k6 images). Runner pods need no Kubernetes API write permissions. Requires Kubernetes 1.33+. |
| Re-export k6 metrics | Curated k6 summary metrics re-exported by operator rather than forwarding full k6 Prometheus output. Keeps schema clean. |
| depends field | One level deep only. Suppresses failure metrics when a dependency is unhealthy. No orchestration, no chaining — purely noise reduction. |
| CRD webhooks | Validating and defaulting webhooks in a separate `synthetics-operator-webhook` deployment (2–3 replicas, stateless). Keeps `kubectl apply` available during operator restarts. Same binary, `--webhook-only` flag. |
| Self-managed certs | Operator generates and rotates self-signed CA + serving certs, stored in a Secret. Webhook replicas watch the Secret and hot-reload via atomic pointer swap. No cert-manager dependency. |
| Grafana in repo | Dashboards and alert rules shipped in the repo, distributed via Helm ConfigMaps and grafana.com. Minimises time-to-value. |
| Operator health metrics | NATS queue depth, reconcile errors, CronJob failures, Job rejections, result stream lag, and cert expiry all instrumented via OTel SDK. Exported as Prometheus on `/metrics` via OTel Prometheus exporter. NATS queue depth is the key saturation signal. |
| Custom metric labels | `spec.metricLabels` on all CRDs propagates to OTel metric attributes (Prometheus labels). Enables per-team alerting and dashboard filtering without separate namespaces. Deliberately separate from k8s metadata labels so observability labels can be managed independently and cardinality stays explicit. |
| suspend field | All CRDs support `spec.suspend` to pause probes without deletion. In-operator probes are unscheduled; CronJobs set `suspend: true`. |
| Consistent interval syntax | All four CRDs use `interval` (duration string). For CronJob-backed probes the operator converts the interval to a CronJob schedule with a per-probe offset for even distribution. Sub-minute intervals (e.g. `10s`) only supported by HttpProbe and DnsProbe; minimum for PlaywrightTest and K6Test is `1m`. |
| OTel SDK, Prometheus export | OTel Go SDK for all instrumentation, exported via OTel Prometheus exporter on `/metrics`. External interface stays Prometheus-compatible; no OTel collector required. |
| Testing layers | Unit tests for pure logic, envtest for controller/webhook behaviour, kind for full end-to-end. envtest covers ~80% of operator behaviour without needing a full cluster. Scale tests include churn scenarios. |
| CI cadence | Unit + envtest on every PR (fast). kind integration + Helm e2e on merge to main. Scale tests and multi-version matrix nightly. |
| Multi-version testing | Nightly suite runs against the four most recent k8s minor versions. Support policy is derived from actual CI coverage. |
| CRD graduation | `v1alpha1` initially. Graduation to `v1beta1` and `v1` driven by schema stability and adoption, with conversion webhooks at each transition. |
| Idiomatic status schema | All CRDs expose `observedGeneration`, two stable condition types (`Ready`, `Suspended`), first-class `lastRunTime`/`lastSuccessTime`/`lastFailureTime`/`consecutiveFailures`, and a normalized `summary` block. Job rejection surfaces as `Ready=False` with `reason: JobCreationFailed` — a reason, not a separate condition type. Status is a clean operator-owned API; the run artifact (ConfigMap) is the transport layer. |
| NetworkPolicy (opt-in) | Helm chart ships an opt-in NetworkPolicy restricting operator pod inbound to `:8080` (metrics) from monitoring namespace, `:8081` (health) from kubelet, `:9443` (ingest + webhooks) from runner namespaces. Egress unrestricted. Disabled by default — not every cluster has an enforcing CNI. |
| Cert reload via atomic pointer | Webhook `tls.Config` uses `GetCertificate` reading from `atomic.Pointer[tls.Certificate]`. Leader rotates and writes Secret; all replicas reload via Secret informer `OnUpdate`. No fsnotify, no polling, no restart. Startup re-injects `caBundle` if it diverges from Secret CA (self-heals after crashed rotation). |

---

## 9. Implementation Phases

Each phase ships a usable product. No phase is purely foundational.

### Phase 1 — HttpProbe MVP

**Deliverable:** Users can define HTTP GET checks as CRDs and see pass/fail in Prometheus.

- Project scaffold via Kubebuilder
- `HttpProbe` CRD: URL, method, headers, status code assertion, `spec.suspend`
- In-operator worker pool with even distribution scheduling
- OTel instrumentation (`synthetics_probe_success`, `synthetics_probe_duration_ms`, `synthetics_consecutive_failures`, `synthetics_last_run_timestamp`, `synthetics_probe_config_error`) exported via Prometheus exporter on `/metrics`
- `/metrics` endpoint on `:8080`
- Validating and defaulting webhooks for HttpProbe (with self-managed certs)
- Helm chart (operator deployment, RBAC, CRD install, webhook config)
- Unit tests for worker pool, distribution algorithm, webhook functions
- envtest for reconcile loop and webhook behaviour

**Usable because:** teams can monitor HTTP endpoint availability with a declarative CRD. Webhooks catch invalid specs immediately.

---

### Phase 2 — HttpProbe body and latency assertions

**Deliverable:** Assert on response body content and single-request latency, not just status code.

- Body assertions: `contains` string match and JSON path equality checks
- Latency assertion: `maxMs` — fail the probe if this single response exceeds the threshold
- `synthetics_probe_assertion_passed` metric per assertion
- Unit tests for assertion evaluation logic

**Usable because:** teams can validate that endpoints return correct content and respond within an acceptable time, not just that they return 200. Percentile-based SLOs belong in Alertmanager rules against `synthetics_probe_duration_ms`.

---

### Phase 3 — HttpProbe advanced requests

**Deliverable:** Monitor POST endpoints and services that require mTLS.

- `spec.request.body` — arbitrary request body for POST/PUT/PATCH
- `spec.request.tls` — mTLS client cert via Secret reference
- Unit tests for request construction, mTLS setup

**Usable because:** real API monitoring requires more than GET requests. POST health endpoints and mutual-TLS authenticated services are common in production.

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

- `DnsProbe` CRD: hostname, record type, resolver, resolved address assertions, `maxResolvedAddresses` cap
- Full DnsProbe metrics schema (`synthetics_dns_success`, `synthetics_dns_response_ms`, `synthetics_dns_resolved_address`, `synthetics_dns_resolved_truncated`)
- Validating and defaulting webhooks for DnsProbe
- envtest for DnsProbe reconcile loop and webhook behaviour

**Usable because:** DNS failures are a distinct failure mode from HTTP failures and warrant their own probe type with dedicated assertions.

---

### Phase 6 — Grafana dashboards and alert rules

**Deliverable:** Importable dashboards and sensible default alert rules for all probes shipped so far.

- Grafana dashboards for HttpProbe and DnsProbe (overview + per-probe drilldown)
- Default Prometheus alert rules (probe failure, high latency, SSL expiry, probe not running)
- Distributed via Helm ConfigMaps (Grafana sidecar auto-import) and grafana.com

**Usable because:** metrics without dashboards require users to build their own visualisation from scratch. Default rules cover the most common alert cases out of the box.

---

### Phase 7 — Webhook deployment split

**Deliverable:** Separate `synthetics-operator-webhook` deployment for independent availability of CRD validation.

- `--webhook-only` flag: disables reconcile loop, worker pool, and ingest handler
- PodDisruptionBudget ensures at least one webhook replica is always up
- Graceful shutdown — in-flight probe runs complete before the process exits

**Usable because:** without this, operator restarts cause `kubectl apply` to fail for all CRD types.

---

### Phase 8 — CronJob infrastructure

**Deliverable:** Shared infrastructure required by all CronJob-backed probe types.

- `results-writer` sidecar image: normalizes runner output, posts to operator `/ingest` with retry
- Operator `/ingest` HTTP handler: TokenReview auth, buffered channel, worker pool, in-memory metrics map
- Scale testing suite (500+ in-operator probes, add/remove churn)

**Usable because:** K6Test and PlaywrightTest both depend on this infrastructure. Building and validating it independently reduces risk when those CRDs ship.

---

### Phase 9 — K6Test

**Deliverable:** k6 load tests defined as CRDs and run on a schedule or triggered from CI.

- `K6Test` CRD: script ConfigMap reference, `interval`, `runOnDeploy`, `parallelism`, `k6Version`, `ttlAfterFinished`, `runner` block
- CronJob reconciliation with even distribution offset
- Automatic VU distribution across parallel pods using k6 execution segments
- k6 summary JSON parsing in the results-writer sidecar
- Job rejection detection: `Ready=False` with `reason: JobCreationFailed`, metric, and Kubernetes Event
- k6 Grafana dashboard
- kind integration tests for full Job lifecycle
- envtest for CronJob reconciliation

**Usable because:** teams can schedule load tests declaratively inside the cluster and alert on threshold failures via Prometheus, without managing k6 execution infrastructure separately.

---

### Phase 10 — PlaywrightTest

**Deliverable:** Playwright browser tests defined as CRDs and run on a schedule.

- `PlaywrightTest` CRD: script ConfigMap reference, `interval`, `playwrightVersion`, `ttlAfterFinished`, `runner` block
- CronJob reconciliation with even distribution offset (reuses Phase 8 infrastructure)
- Playwright JSON reporter output parsing in the results-writer sidecar
- Per-test metrics with `suite` and `test` labels; suite-level rollups
- Playwright Grafana dashboard
- kind integration tests with real Playwright image

**Usable because:** multi-step browser flows and authenticated journeys can be monitored continuously inside the cluster without a separate Playwright infrastructure.

---

### Phase 11 — `depends` field

**Deliverable:** Suppress failure alerts for probes whose dependencies are already unhealthy.

- `spec.depends` on all CRDs — list of probe references that must be healthy for a failure to be actionable
- Suppression logic: probe still runs, failure metric replaced by `synthetics_probe_suppressed=1`
- One level deep only — no chaining

**Usable because:** a downstream service failing because its upstream dependency is down creates redundant alerts. Suppression reduces noise without hiding the original failure.

---

### Phase 12 — Distribution

**Deliverable:** Easy to discover, install, and contribute to.

- Multi-version nightly CI matrix (four most recent Kubernetes minor versions)
- goreleaser pipeline for versioned image and chart releases
- Grafana dashboard publication to grafana.com
- OperatorHub / Artifact Hub listing
- Full example library in `/examples`
- Contributing guide and development setup docs

**Usable because:** the project moves from an internal tool to a publishable open source operator with stable release artifacts.

