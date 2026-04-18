# Ubiquitous Language

## Synthetic checks

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Probe** | A lightweight, in-process HTTP or DNS check that runs on a repeating interval, managed by the operator's in-memory scheduler | Monitor, check, test |
| **Test** | A heavyweight, script-driven load test (k6, Playwright) that runs as a Kubernetes CronJob | Probe, job, run |
| **HTTPProbe** | A **Probe** that makes an HTTP request and asserts on the response | HTTP check, HTTP monitor |
| **DNSProbe** | A **Probe** that resolves a DNS name and asserts on the answer | DNS check, DNS monitor |
| **K6Test** | A **Test** that executes a k6 JavaScript script as a CronJob | k6 probe, k6 job |
| **PlaywrightTest** | A **Test** that executes a Playwright browser test spec as a CronJob | Playwright probe, browser test |
| **Script** | The JavaScript file, stored in a ConfigMap, that a **K6Test** or **PlaywrightTest** executes | Test file, source |
| **Assertion** | A single named condition evaluated against a **Probe** response (e.g. status == 200) | Check, rule, expectation |
| **Interval** | The target period between consecutive executions of a **Probe** or **Test** | Frequency, period, schedule |
| **Suspend** | A flag on a **Probe** or **Test** that pauses execution without deleting the resource | Pause, disable |

## Results and observability

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **TestResult** | The JSON payload produced after each **Test** run and shipped over NATS — contains kind, name, namespace, success, timestamp, and duration | ProbeResult, result message, event |
| **NATS** | The message bus used to carry **TestResults** from **Test** pods to the operator | Message queue, event bus |
| **Metrics Store** | The in-process Prometheus registry that records **Probe** outcomes and ingested **TestResults** | Metrics server, store |

## Scheduling and execution

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Scheduler** | The in-memory component that maintains the set of active **Probes** and dispatches them to **Workers** on their **Interval** | Cron, timer, dispatcher |
| **Worker** | A goroutine in the **Worker Pool** that executes a single **Probe** | Thread, runner |
| **Worker Pool** | A bounded set of **Workers** that limits concurrency of in-process **Probe** execution | Thread pool |
| **Runner** | The per-kind binary that wraps a test process (k6-runner wraps k6; playwright-runner wraps the Playwright test runner), captures its outcome, and writes a **TestResult** to a shared volume | Wrapper, executor |
| **CronJob** | The Kubernetes batch resource created and owned by the operator for each **Test** (K6Test or PlaywrightTest) | Job, periodic job |

## Infrastructure

| Term | Definition | Aliases to avoid |
| --- | --- | --- |
| **Reconciler** | The controller-runtime component that watches a CRD and drives its actual state toward desired state | Controller, handler, sync loop |
| **Webhook** | The Kubernetes admission webhook that defaults and validates CRD specs before they are persisted | Validator, admission controller |
| **test-sidecar** | The native sidecar container that runs alongside the **Runner**, reads the **TestResult** JSON, and publishes it over NATS. Kind-agnostic — the JSON payload carries whatever detail the **Runner** emits | results-writer, sidecar |
| **k6-runner** | The init-container image that stages the k6 **Runner** binary into a shared volume, then runs k6 in the stock `grafana/k6` main container | runner-installer (only the install mode), results-writer |
| **playwright-runner** | A single main-container image (`node:22-slim` + pinned `@playwright/test` + Chromium) whose ENTRYPOINT runs the Playwright test runner, parses the JSON reporter, and writes the **TestResult** | playwright sidecar, browser runner |
| **RunnerSpec** | The optional pod-level configuration on a **K6Test** or **PlaywrightTest** (env, resources, affinity) for the **Runner** container | Runner config, pod spec |

## Relationships

- A **Probe** (HTTPProbe, DNSProbe) is managed by the in-memory **Scheduler**; no CronJob is created.
- A **Test** (K6Test, PlaywrightTest) owns exactly one **CronJob**; the **Scheduler** is not involved.
- Each **Test** CronJob pod runs one **Runner** container and one **test-sidecar** container. K6Test additionally uses an init container to stage the k6-runner binary into the stock `grafana/k6` image; PlaywrightTest uses the playwright-runner image as the main container directly.
- The **Runner** writes a **TestResult** JSON to a shared volume; the **test-sidecar** reads it and publishes it over **NATS**. PlaywrightTest **TestResults** carry a per-test breakdown; K6Test **TestResults** carry only the aggregate pass/fail.
- The **Metrics Store** consumes **TestResults** from NATS and **Probe** outcomes directly from **Workers**.
- A **Test** may have a **RunnerSpec** that controls pod-level resources; an **HTTPProbe** or **DNSProbe** does not.

## Example dialogue

> **Dev:** "Do **Probes** and **Tests** use the same execution path?"

> **Domain expert:** "No — a **Probe** runs in-process inside the operator pod on its **Interval**; the **Scheduler** dispatches it to a **Worker**. A **Test** creates a **CronJob** that spins up a pod with the **Runner** and the **test-sidecar**."

> **Dev:** "So if I set `suspend: true` on a **K6Test**, what happens to the **CronJob**?"

> **Domain expert:** "The **Reconciler** propagates `suspend` to the **CronJob** spec, so Kubernetes stops triggering new runs. The **CronJob** itself still exists."

> **Dev:** "And the **TestResult** — that's only for **Tests**, not **Probes**?"

> **Domain expert:** "Correct. **Probes** write their outcome directly to the **Metrics Store**. **Tests** produce a **TestResult** JSON in the pod and ship it over **NATS**, where the operator's consumer ingests it into the same **Metrics Store**."

> **Dev:** "So the **Assertion** concept only applies to **Probes**?"

> **Domain expert:** "Today, yes. An **Assertion** is evaluated against an HTTP response or a DNS answer set — neither exists in a **Test** run, where pass/fail is determined purely by the k6 script's exit code."

## Flagged ambiguities

- **"runner"** appears in two contexts: the `RunnerSpec` field on `K6Test` (pod-level configuration) and the **Runner** binary that wraps k6. They are distinct — `RunnerSpec` is configuration, **Runner** is the executable. Code uses `RunnerSpec` and `k6-runner` to keep them apart; spoken language should do the same.
- **"results-writer"** was an early name for the **test-sidecar** container. It is retired — prefer **test-sidecar** in all contexts.
- **"ProbeResult"** was an early name for **TestResult** on the NATS wire format. It is retired — **Probes** never use NATS; only **Tests** produce **TestResults**.
- **"probe"** (lowercase generic) was sometimes used to mean any synthetic check. The codebase distinguishes **Probe** (HTTPProbe, DNSProbe) from **Test** (K6Test) — use the specific term.
