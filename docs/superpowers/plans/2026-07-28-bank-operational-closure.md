# Bank Operational Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete bank with OpenTelemetry, protected compensation operations, runnable Kubernetes overlays, failure-injection E2E gates, dashboards, alerts, CI, documentation, and final template packaging.

**Architecture:** Bank reuses the proven self-contained telemetry API shape from commerce and extends it for gRPC and workflow spans. Dev and production Kustomize overlays make state ownership explicit, while CI exercises crash recovery, idempotency, compensation failure, route isolation, scaling, and trace continuity.

**Tech Stack:** Go 1.22, OpenTelemetry Go 1.32.0, OTel gRPC instrumentation 0.57.0, Prometheus, Grafana, Jaeger, Docker Compose, Kubernetes/Kustomize, GitHub Actions.

## Global Constraints

- Bank and commerce contain independent copies of shared platform patterns.
- Internal gRPC and operator APIs are never routed through the public gateway.
- A financial compensation can be resolved only with an immutable reconciliation reference and current-state validation.
- Dev credentials are labeled unsafe; production delegates identity and secrets to the platform.
- The default topology does not claim PostgreSQL or RabbitMQ HA.
- CI failures capture logs and always remove volumes.
- Every task ends with focused tests and a commit.

---

## File Map

- `templates/bank/internal/platform/telemetry`: OTel provider, propagation, logging, and metrics.
- `templates/bank/internal/platform/grpcx`: gRPC tracing interceptors.
- `templates/bank/internal/platform/messaging`: RabbitMQ tracing.
- `templates/bank/internal/payment/admin`: protected compensation recovery.
- `templates/bank/deploy/otel`, `prometheus`, `grafana`: observability stack.
- `templates/bank/deploy/k8s/overlays`: runnable dev and external-state production shapes.
- `templates/bank/test/smoke.sh`: end-to-end payment and failure gates.
- `.github/workflows/ci.yml`: bank static and E2E jobs.

### Task 1: Add Bank OpenTelemetry Foundation

**Files:**
- Create: `templates/bank/internal/platform/telemetry/provider.go`
- Create: `templates/bank/internal/platform/telemetry/provider_test.go`
- Create: `templates/bank/internal/platform/telemetry/propagation.go`
- Create: `templates/bank/internal/platform/telemetry/propagation_test.go`
- Create: `templates/bank/internal/platform/telemetry/logging.go`
- Modify: `templates/bank/go.mod`
- Modify: `templates/bank/go.sum`

**Interfaces:**
- Produces: the same `telemetry.Config`, `Provider`, AMQP carrier, and trace log fields as commerce.

- [ ] **Step 1: Port contract tests, not implementation**

Add provider-disabled, resource identity, trace-field, and AMQP propagation
tests using package import root `bank`.

- [ ] **Step 2: Verify tests fail**

Run:

```bash
cd templates/bank
go test ./internal/platform/telemetry
```

Expected: compile failure.

- [ ] **Step 3: Add pinned dependencies and implementation**

Run:

```bash
cd templates/bank
go get go.opentelemetry.io/otel@v1.32.0
go get go.opentelemetry.io/otel/sdk@v1.32.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.32.0
go get go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc@v0.57.0
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.57.0
go mod tidy
```

Implement the same public contract as commerce so platform behavior remains
consistent without cross-template imports.

- [ ] **Step 4: Run tests and commit**

```bash
cd templates/bank
go test ./internal/platform/telemetry
git add templates/bank/internal/platform/telemetry templates/bank/go.mod templates/bank/go.sum
git commit -m "feat(bank): add OpenTelemetry foundation"
```

### Task 2: Trace REST, gRPC, RabbitMQ, and Workflow

**Files:**
- Modify: `templates/bank/internal/platform/httpx/server.go`
- Modify: `templates/bank/internal/platform/httpx/server_test.go`
- Modify: `templates/bank/internal/platform/grpcx/client.go`
- Modify: `templates/bank/internal/platform/grpcx/server.go`
- Modify: `templates/bank/internal/platform/grpcx/client_test.go`
- Modify: `templates/bank/internal/platform/messaging/rabbitmq.go`
- Modify: `templates/bank/internal/platform/messaging/rabbitmq_test.go`
- Modify: `templates/bank/internal/platform/workflow/engine.go`
- Create: `templates/bank/internal/platform/workflow/tracing_test.go`

**Interfaces:**
- Produces: continuous W3C trace context through REST, Preparation gRPC, RabbitMQ, Action handlers, and Compensation.

- [ ] **Step 1: Write in-memory span tests**

Assert exact span names:

```text
bank.http
bank.grpc.customer.GetCustomer
bank.grpc.core.GetAccount
bank.messaging.publish
bank.messaging.consume
workflow.prepare
workflow.action.execute
workflow.action.wait
workflow.action.compensate
```

- [ ] **Step 2: Run focused tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/httpx ./internal/platform/grpcx ./internal/platform/messaging ./internal/platform/workflow
```

Expected: new span assertions fail.

- [ ] **Step 3: Add instrumentation**

Use `otelhttp.NewHandler`, `otelgrpc.NewClientHandler`,
`otelgrpc.NewServerHandler`, AMQP header propagation, and workflow tracer
attributes `workflow.id`, `workflow.type`, `workflow.action`, `command.id`, and
`workflow.direction`.

- [ ] **Step 4: Run focused and race tests**

```bash
cd templates/bank
go test ./internal/platform/...
go test -race ./internal/platform/...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform
git commit -m "feat(bank): trace workflow communication paths"
```

### Task 3: Add Protected Compensation Operations

**Files:**
- Create: `templates/bank/proto/bank/payment/v1/workflow_admin.proto`
- Create: generated files under `templates/bank/gen/bank/payment/v1`
- Create: `templates/bank/internal/payment/admin/service.go`
- Create: `templates/bank/internal/payment/admin/service_test.go`
- Create: `templates/bank/internal/payment/admin/auth.go`
- Create: `templates/bank/internal/payment/admin/auth_test.go`
- Modify: `templates/bank/cmd/payment/main.go`
- Modify: `templates/bank/deploy/k8s/base/config.yaml`

**Interfaces:**
- Produces: internal gRPC `RetryCompensation` and `RecordReconciliation`.
- Requires: `x-bank-operator-token` metadata and NetworkPolicy-restricted access.

- [ ] **Step 1: Write auth and reconciliation tests**

Reject missing or incorrect token with `codes.Unauthenticated`. Reject
reconciliation without a non-empty external reference. For funds hold, validate
the hold is released; for ledger transfer, validate a reversal voucher exists
and balances reconcile before marking the Action compensated.

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/payment/admin
```

Expected: compile failure.

- [ ] **Step 3: Define and generate admin Protobuf**

Methods:

```protobuf
rpc RetryCompensation(RetryCompensationRequest) returns (WorkflowStatus);
rpc RecordReconciliation(RecordReconciliationRequest) returns (WorkflowStatus);
```

`RecordReconciliationRequest` contains workflow ID, Action name, external
reference, and reason.

- [ ] **Step 4: Implement protected service**

Compare the supplied token in constant time. Persist operator identity,
reference, reason, previous state, new state, and timestamp in an immutable
`workflow_operator_audit` table.

- [ ] **Step 5: Run tests and commit**

```bash
cd templates/bank
make proto
go test ./internal/payment/admin ./cmd/payment
git add templates/bank/proto templates/bank/gen templates/bank/internal/payment/admin templates/bank/cmd/payment/main.go templates/bank/deploy/k8s/base/config.yaml
git commit -m "feat(bank): protect compensation recovery operations"
```

### Task 4: Add Observability Stack, Dashboards, and Alerts

**Files:**
- Create: `templates/bank/compose.observability.yaml`
- Create: `templates/bank/deploy/otel/collector.yaml`
- Create: `templates/bank/deploy/prometheus/prometheus.yaml`
- Create: `templates/bank/deploy/prometheus/alerts.yaml`
- Create: `templates/bank/deploy/grafana/provisioning/datasources/datasources.yaml`
- Create: `templates/bank/deploy/grafana/provisioning/dashboards/bank.yaml`
- Create: `templates/bank/deploy/grafana/dashboards/payment-workflows.json`
- Create: `templates/bank/deploy/grafana/dashboards/message-reliability.json`
- Create: `templates/bank/deploy/grafana/dashboards/core-ledger.json`
- Modify: `templates/bank/Makefile`

**Interfaces:**
- Produces: `make observability`, `make observability-down`, and `make trace-smoke`.

- [ ] **Step 1: Add JSON and Prometheus rule validation**

Add a Make target:

```make
observability-check:
	jq empty deploy/grafana/dashboards/*.json
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" prom/prometheus:v2.54.1 \
	  promtool check config /etc/prometheus/prometheus.yaml
```

- [ ] **Step 2: Configure real trace export**

Collector receives OTLP and exports traces to `jaeger:4317`. Prometheus scrapes
all application replicas by DNS and loads `alerts.yaml`.

- [ ] **Step 3: Add exact alerts**

Rules:

```promql
max(workflow_waiting_age_seconds) > 60
sum(workflow_compensation_failures_total) > 0
max(outbox_oldest_age_seconds) > 30
sum(rabbitmq_consumer_lag) > 100
sum(ledger_invariant_failures_total) > 0
```

Use `for: 1m` except ledger invariant failure, which uses `for: 0m`.

- [ ] **Step 4: Add dashboards**

Payment dashboard: status counts, P95 Action duration, compensation failures,
oldest waiting workflow. Messaging dashboard: Outbox age, Inbox duplicates,
consumer lag. Ledger dashboard: postings, reversals, invariant failures.

- [ ] **Step 5: Validate and commit**

```bash
cd templates/bank
make observability-check
docker compose -f compose.yaml -f compose.observability.yaml config --quiet
git add templates/bank/compose.observability.yaml templates/bank/deploy templates/bank/Makefile
git commit -m "feat(bank): add workflow observability and alerts"
```

### Task 5: Add Runnable Dev and External-State Production Overlays

**Files:**
- Create: `templates/bank/deploy/k8s/overlays/dev/kustomization.yaml`
- Create: `templates/bank/deploy/k8s/overlays/dev/state.yaml`
- Create: `templates/bank/deploy/k8s/overlays/dev/secret.yaml`
- Create: `templates/bank/deploy/k8s/overlays/prod/kustomization.yaml`
- Create: `templates/bank/deploy/k8s/overlays/prod/external-services.yaml`
- Create: `templates/bank/deploy/k8s/overlays/prod/secret-contract.yaml`
- Create: `templates/bank/deploy/k8s/base/network-policy.yaml`
- Modify: `templates/bank/deploy/k8s/base/kustomization.yaml`
- Modify: `templates/bank/deploy/k8s/test.sh`

**Interfaces:**
- Produces: `kubectl apply -k deploy/k8s/overlays/dev`.
- Produces: renderable prod contracts without plaintext credentials.

- [ ] **Step 1: Extend manifest tests**

Assert dev renders seven PostgreSQL StatefulSets and one RabbitMQ StatefulSet.
Assert prod renders no StatefulSet and contains no `stringData`. Assert the
public Ingress has no gRPC or admin route.

- [ ] **Step 2: Implement dev state**

Use one-replica StatefulSets with PVCs and dev-only credentials labeled:

```yaml
bank.jiade/unsafe: dev-only
```

- [ ] **Step 3: Implement production contracts**

ExternalName Services receive configurable DNS names through Kustomize
replacements. `secret-contract.yaml` contains only a documented
`SecretProviderClass` contract and no credential values.

- [ ] **Step 4: Add NetworkPolicies**

Default deny ingress/egress, then explicitly allow:

- Traefik to REST ports.
- payment to customer/core gRPC.
- applications to RabbitMQ.
- each application to its owned PostgreSQL.
- Prometheus/collector telemetry flows.
- operator namespace to payment admin gRPC.

- [ ] **Step 5: Render, test, and commit**

```bash
cd templates/bank
kubectl kustomize deploy/k8s/overlays/dev >/tmp/bank-dev.yaml
kubectl kustomize deploy/k8s/overlays/prod >/tmp/bank-prod.yaml
bash deploy/k8s/test.sh
git add templates/bank/deploy/k8s
git commit -m "feat(bank): add runnable and production Kubernetes overlays"
```

### Task 6: Add Bank Failure-Injection Smoke Gates

**Files:**
- Create: `templates/bank/test/smoke.sh`
- Create: `templates/bank/test/trace-smoke.sh`
- Modify: `templates/bank/Makefile`
- Modify: deterministic risk/core failure hooks under `templates/bank/internal`

**Interfaces:**
- Produces: `make smoke` and `make trace-smoke`.

- [ ] **Step 1: Add deterministic test-only failure controls**

Seed specific payment IDs whose risk decision rejects, whose balance is
insufficient, whose first transfer attempt fails transiently, and whose first
compensation attempt fails. Controls are fixture-derived and unavailable for
arbitrary external requests.

- [ ] **Step 2: Implement smoke gates**

The script:

1. verifies two gateway instances for core/payment/risk;
2. submits a successful workflow and polls `succeeded`;
3. submits risk rejection and asserts no hold/voucher;
4. submits insufficient funds and asserts authorization voided;
5. submits transient transfer failure and asserts hold release;
6. repeats commands/events and asserts one voucher;
7. kills one payment container during `waiting_result` and asserts takeover;
8. reverses a completed payment and asserts red entries;
9. injects compensation exhaustion and asserts `compensation_failed`;
10. probes internal routes and gRPC exposure negatively.

- [ ] **Step 3: Implement trace smoke**

Submit a payment with a known request ID, query Jaeger until a trace contains
REST, customer/core gRPC, messaging publish/consume, and workflow Action spans,
then exit 0. Query Jaeger without publishing its host port:

```bash
docker run --rm --network bank-obs curlimages/curl:8.10.1 \
  -fsS 'http://jaeger:16686/api/traces?service=payment&limit=20'
```

- [ ] **Step 4: Validate shell and run E2E**

Run:

```bash
cd templates/bank
bash -n test/smoke.sh test/trace-smoke.sh
make up
make smoke
make observability
make trace-smoke
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/test templates/bank/Makefile templates/bank/internal
git commit -m "test(bank): verify saga failure and recovery paths"
```

### Task 7: Add Complete Bank CI

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `Makefile`

**Interfaces:**
- Produces: `bank` static/unit job and `bank-e2e` topology job.

- [ ] **Step 1: Expand the bank static job**

Run:

```yaml
- uses: azure/setup-kubectl@v4
- run: cd templates/bank && go build ./...
- run: cd templates/bank && go test ./...
- run: cd templates/bank && go test -race ./internal/platform/...
- run: cd templates/bank && make config-check observability-check
- run: kubectl kustomize templates/bank/deploy/k8s/overlays/dev >/tmp/bank-dev.yaml
- run: kubectl kustomize templates/bank/deploy/k8s/overlays/prod >/tmp/bank-prod.yaml
- run: cd templates/bank && make proto && git diff --exit-code -- gen
```

- [ ] **Step 2: Add bank E2E**

Start bank, run smoke, start observability, run trace smoke, capture logs on
failure, and always run:

```bash
docker compose -f compose.yaml -f compose.observability.yaml down --volumes --remove-orphans
```

- [ ] **Step 3: Add embedded-template freshness gate**

Run `go generate ./internal/template` followed by `git diff --exit-code
-- internal/template/templates.tar`.

- [ ] **Step 4: Run local static equivalent**

Run:

```bash
cd templates/bank
go build ./...
go test ./...
go test -race ./internal/platform/...
make config-check observability-check
kubectl kustomize deploy/k8s/overlays/dev >/tmp/bank-dev.yaml
kubectl kustomize deploy/k8s/overlays/prod >/tmp/bank-prod.yaml
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/ci.yml Makefile
git commit -m "ci: verify bank workflow and deployment topology"
```

### Task 8: Final Documentation, Packaging, and Acceptance

**Files:**
- Modify: `templates/bank/README.md`
- Modify: `templates/bank/ARCHITECTURE.md`
- Modify: `templates/bank/template.yaml`
- Modify: `templates/commerce/README.md`
- Modify: `templates/commerce/ARCHITECTURE.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `internal/template/templates.tar`

**Interfaces:**
- Produces: final self-contained generated bank and commerce templates.

- [ ] **Step 1: Document exact operational contracts**

Cover payment request examples, workflow states, Action/Compensation sequence,
gRPC/MQ boundaries, replica/resource profiles, service discovery, scaling,
observability, DLQ, operator reconciliation, Kubernetes overlays, stateful HA
non-goals, and cleanup warnings.

- [ ] **Step 2: Regenerate embedded templates**

Run:

```bash
go generate ./internal/template
```

- [ ] **Step 3: Run complete acceptance**

Run:

```bash
go test ./...
cd templates/commerce && go test ./...
cd ../bank && go test ./...
go test -race ./internal/platform/...
cd ../..
git diff --check
```

Expected: PASS.

- [ ] **Step 4: Commit final artifacts**

```bash
git add templates README.md README.zh-CN.md internal/template/templates.tar
git commit -m "docs: complete bank workflow operations guide"
```
