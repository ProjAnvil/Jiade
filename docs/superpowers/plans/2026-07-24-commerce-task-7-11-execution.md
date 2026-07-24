# Commerce Task 7–11 Execution Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete the commerce production topology by implementing payment and fulfillment event workflows, a deterministic seed package, a scalable Docker Compose topology, Kubernetes equivalents with documentation, and packaged-template acceptance.

**Architecture:** Six service-owned PostgreSQL databases behind Traefik, synchronous HTTP only for checkout validation/reservation, RabbitMQ topic events plus transactional Outbox/Inbox for state propagation. Task 7 adds the payment and fulfillment domains that order (Task 6) already consumes; Tasks 8–11 add data, topology, deployment, and packaging. Payment and fulfillment are independent packages with no shared mutable state and are parallelized.

**Tech Stack:** Go 1.22 (pin via `GOTOOLCHAIN=go1.22.12`), `pgx/v5`, `amqp091-go`, `net/http`, PostgreSQL 16, RabbitMQ 4, Traefik 3, Docker Compose v2, Kubernetes + kustomize.

## Global Constraints

- Money is `int64` minor units; binary floating point is forbidden. (spec line 18)
- Event delivery is at least once; consumers must be idempotent. (spec line 19)
- External traffic enters only through Traefik on host port `18100`. (spec line 16)
- Each service owns a separate PostgreSQL container, database, schema, and volume. (spec line 17)
- The same seed, scale, and generator version produce the same data. (spec line 20)
- Exact order counts: dev=100, demo=10,000, load=1,000,000. (spec line 21)
- The million-order seed is opt-in and must stream data in bounded memory. (spec line 22)
- Do not implement authentication, a real payment provider, search, Redis, a promotion DSL, RabbitMQ clustering, or PostgreSQL HA. (spec line 23)
- Default replicas: catalog=2, customer=1, inventory=2, order=2, payment=1, fulfillment=1. (spec line 15)
- Default stack targets 4–6 GB memory. (spec line 14)

## Source of Truth

Detailed file lists, code sketches, TDD steps, and commit messages for each task live in the parent specification:
`docs/superpowers/plans/2026-07-24-commerce-production-topology.md` — **Task 7** (line 488), **Task 8** (line 557), **Task 9** (line 630), **Task 10** (line 708), **Task 11** (line 774).

This plan locks the **interface contracts**, **file map**, **sequencing**, and **verification gates**. Where a step says "implement per spec §N", open that spec section and follow its TDD steps; the contracts below are the non-negotiable boundaries that make the tasks compose.

## Current State (baseline `c94fd72`)

- Tasks 1–6 committed: platform (config/postgres/httpx/telemetry/client), messaging (event/outbox/inbox/rabbitmq), domain schemas, catalog/customer/inventory services, order saga + Task 6 remediation.
- `payment_db.sql` and `fulfillment_db.sql` schemas already exist and are committed — **Task 7 implements Go against existing tables, it does NOT create migrations.**
- `internal/payment/state.go` state machine already exists (Transition/State/Event) — Task 7 builds the service/store/provider on top of it.
- `go.mod` has only `pgx/v5` (lib/pq removed, old scaffolding deleted).
- Run all commands from `templates/commerce/` unless noted. `cd` is persistent in this session.

## Interface Contracts (critical — subagents must match these exactly)

### Event envelope (already provided by `platform/messaging`)
```go
type Event struct {
    ID            string          // uuid v4
    SchemaVersion int             // == messaging.CurrentSchemaVersion (1)
    Type          string          // e.g. "payment.captured.v1"
    Subject       string          // == order_id for all order-saga events
    OccurredAt    time.Time       // non-zero, UTC
    CorrelationID string
    CausationID   string          // optional
    Data          json.RawMessage // event-type-specific JSON
}
func NewEvent(eventType, subject, correlationID, causationID string, data json.RawMessage, clock func() time.Time) Event
```

### Payload shapes order decodes with `json.Decoder.DisallowUnknownFields()` — exact fields only

| Event type(s) order consumes | Payload JSON | Validated against |
|---|---|---|
| `payment.captured.v1`, `payment.succeeded.v1`, `payment.paid.v1` | `{"order_id","currency","amount_minor"}` | currency==order.currency AND amount_minor==order.total_minor |
| `payment.failed.v1` | `{"order_id","code"}` | code non-empty; ∈ {insufficient_funds, card_declined, provider_timeout, risk_rejection} |
| `payment.refunded.v1`, `refund.succeeded.v1` | `{"order_id","currency","amount_minor"}` | amount rules (see `applyRefunded`) |
| `fulfillment.cancelled.v1`, `fulfillment.cancellation-succeeded.v1`, `fulfillment.completed.v1`, `fulfillment.delivered.v1` | `{"order_id"}` | order_id==subject |

**Rules:** `Subject == order_id` (payload order_id must equal event Subject). Envelope must have non-zero schema version, non-empty subject, non-zero OccurredAt, non-empty valid-JSON Data. Malformed/mismatched → `messaging.NonRetryable`.

### Event exchange topology (shared constant)
- Exchange: `commerce.events` (topic, durable).
- Each service owns: a result queue, a retry queue (`<svc>.retry`, TTL 2000ms, DLX back to `commerce.events`), a DLQ (`<svc>.dlq`).
- Order's bindings are the authoritative list of result event types (see `cmd/order/main.go:orderEventBindings`); payment/fulfillment **produce** exactly those types.

### Wiring pattern (follow `cmd/order/main.go` exactly)
`config.Load(svc)` → `postgres.Open` → `amqp.Dial` → publisher channel + `messaging.NewRabbitPublisher` → consumer channel + `declareXxxConsumerTopology` → retry channel + `messaging.NewConfirmedRouter` → store + service → outbox relay (`messaging.RunRelay`) + consumer (`Xxx.RunConsumer`) as `WorkerLifecycle` → `httpx.NewServer` with `NewRuntimeReadinessWithDependencies` → graceful shutdown joining server/relay/consumer errors.

---

## File Map

```text
templates/commerce/
  internal/payment/{service,store,provider,http,consumer}.go   Task 7a (par. A)
  internal/payment/service_test.go                             Task 7a
  internal/fulfillment/{service,store,http,consumer}.go         Task 7b (par. B)
  internal/fulfillment/service_test.go                         Task 7b
  cmd/payment/main.go, cmd/fulfillment/main.go                 Task 7a / 7b
  internal/seed/{config,vocabulary,catalog,customers,orders,
                 payments,fulfillment,load,verify}.go          Task 8
  internal/seed/{seed_test,verify_test}.go                     Task 8
  internal/seed/testdata/dev-summary.json                      Task 8
  cmd/seed/main.go                                             Task 8
  Dockerfile, .dockerignore                                    Task 9
  compose.yaml, compose.observability.yaml, compose.load.yaml  Task 9
  deploy/{traefik,rabbitmq,otel,prometheus,grafana}/...        Task 9
  test/smoke.sh, Makefile                                      Task 9
  deploy/k8s/{namespace,config,apps,services,gateway,
              availability,kustomization}.yaml                 Task 10
  README.md, ARCHITECTURE.md, template.yaml                    Task 10
  README.md, README.zh-CN.md (root)                            Task 10
  internal/template/templates.tar (root, regenerated)          Task 11
```

---

## Task 7a: Payment Event Workflow (parallel track A)

**Files:**
- Create: `templates/commerce/internal/payment/service.go`
- Create: `templates/commerce/internal/payment/store.go`
- Create: `templates/commerce/internal/payment/provider.go`
- Create: `templates/commerce/internal/payment/http.go`
- Create: `templates/commerce/internal/payment/consumer.go`
- Create: `templates/commerce/internal/payment/service_test.go`
- Create: `templates/commerce/cmd/payment/main.go`

**Interfaces:**
- Consumes: `internal/payment` (existing `state.go`: `State`, `Event`, `Transition`); `platform/messaging` (`Event`, `NewEvent`, `NewRabbitPublisher`, `RunRelay`, `NewConfirmedRouter`, `ProcessRabbitDeliveryForRetryQueue`, `RetryPolicy`, `HandleOnce`, `NonRetryable`); `platform/config`, `platform/postgres`, `platform/httpx`; existing `payment_db.sql` (payment_intent, payment_method_snapshot, payment_attempt, refund, webhook_inbox, outbox_event, inbox_event).
- Produces events with **exact** payloads (see contract table): `payment.captured.v1`/`payment.succeeded.v1` `{order_id,currency,amount_minor}`; `payment.failed.v1` `{order_id,code}`; `refund.succeeded.v1` `{order_id,currency,amount_minor}`.
- Consumes events: `order.placed.v1` (create intent + attempt via deterministic provider), `payment.refund-requested.v1` (refund), `order.cancelled.v1` (cancel intent).
- Exposes: `NewService(store, provider, opts)`, `NewPostgresStore(pool, clock)`, `NewHandler(service)`, `NewConsumer(store, policy)`, `RunConsumer(...)`, `NewRuntimeReadinessWithDependencies(...)`.

**Deterministic provider contract:** provider outcomes derive from explicit scenario inputs (scenario enum / seeded stream), never wall-clock randomness. `provider.go` defines a `Provider` interface and a deterministic simulator that yields `provider_timeout` then success for the transient scenario, `card_declined` for hard decline, etc. Webhook provider event IDs and payment event IDs are unique and replay-safe (idempotency_key UNIQUE on intent/refund).

- [ ] **Step 1: Write failing payment tests** (per spec §Task 7 Step 1, lines 511–525). Cover: transient payment → 2 attempts → success; hard decline → `payment.failed.v1`; full/partial refund → `refund.succeeded.v1`; duplicate `order.placed.v1` replay is idempotent; money payload exactness.
- [ ] **Step 2: Verify RED** — `go test ./internal/payment` fails (undefined).
- [ ] **Step 3: Implement** store (pgx against payment_db.sql tables, atomic intent+method+attempt+refund+outbox in one tx), service (uses `payment.Transition`), provider (deterministic simulator), http (query/webhook endpoints, `application/problem+json`), consumer (`ProcessDelivery` → `HandleOnce` + `applyEvent`), `cmd/payment/main.go` (wiring per pattern, bindings: `order.placed.v1`, `order.cancelled.v1`, `payment.refund-requested.v1`). Per spec §Task 7 Step 3 (lines 533–539).
- [ ] **Step 4: Verify GREEN** — `go test ./internal/payment -count=1` PASS; `go test -tags=integration ./internal/payment` PASS (PostgreSQL cases skip without `TEST_DATABASE_URL`).
- [ ] **Step 5: Static check** — `go vet ./internal/payment ./cmd/payment`; `go build ./cmd/payment`.
- [ ] **Step 6: Commit** — `feat(commerce): add payment workflow`.

## Task 7b: Fulfillment Event Workflow (parallel track B)

**Files:**
- Create: `templates/commerce/internal/fulfillment/service.go`
- Create: `templates/commerce/internal/fulfillment/store.go`
- Create: `templates/commerce/internal/fulfillment/http.go`
- Create: `templates/commerce/internal/fulfillment/consumer.go`
- Create: `templates/commerce/internal/fulfillment/service_test.go`
- Create: `templates/commerce/cmd/fulfillment/main.go`

**Interfaces:**
- Consumes: `platform/messaging`, `platform/{config,postgres,httpx}`; existing `fulfillment_db.sql` (fulfillment_order, fulfillment_item, pick_item, package, package_item, shipment, shipment_package, tracking_event, outbox_event, inbox_event).
- Produces events with **exact** payloads: `fulfillment.completed.v1`/`fulfillment.delivered.v1` `{order_id}`; `fulfillment.cancelled.v1`/`fulfillment.cancellation-succeeded.v1` `{order_id}`.
- Consumes events: `order.paid.v1` (split reserved lines by `location_id` → one fulfillment_order per location, create picks/packages, advance shipment), `fulfillment.cancel-requested.v1` (cancel), `order.cancelled.v1` (cancel).
- Exposes: `NewService(store, opts)`, `NewPostgresStore(pool, clock)`, `NewHandler(service)`, `NewConsumer(store, policy)`, `RunConsumer(...)`, `NewRuntimeReadinessWithDependencies(...)`.

**Split rule:** group order lines by `location_id`; `fulfillment_order` has `UNIQUE(order_id, location_id)`; one shipment per fulfillment order with deterministic carrier/tracking; duplicate `order.paid.v1` replay is idempotent.

- [ ] **Step 1: Write failing fulfillment tests** (per spec §Task 7 Step 1, lines 521–525). Cover: paid order with LOC-1+LOC-2 → 2 fulfillment orders; multi-package; carrier exception; cancellation; duplicate delivery idempotent.
- [ ] **Step 2: Verify RED** — `go test ./internal/fulfillment` fails.
- [ ] **Step 3: Implement** per spec §Task 7 Step 3 (lines 533–539) + wiring in `cmd/fulfillment/main.go` (bindings: `order.paid.v1`, `order.cancelled.v1`, `fulfillment.cancel-requested.v1`).
- [ ] **Step 4: Verify GREEN** — `go test ./internal/fulfillment -count=1` PASS; integration tag PASS (skip without URL).
- [ ] **Step 5: Static check** — `go vet ./internal/fulfillment ./cmd/fulfillment`; `go build ./cmd/fulfillment`.
- [ ] **Step 6: Commit** — `feat(commerce): add fulfillment workflow`.

> **Parallelization note (Track A ∥ Track B):** the two packages share no mutable state, no migrations, no overlapping files, and only the pre-built `platform/messaging` primitives. Dispatch as two concurrent subagents. Merge conflicts are impossible at the file level; the only shared symbol is the `commerce.events` exchange string constant. After both land, run `go test ./...` to confirm the whole module still builds.

---

## Task 8: Deterministic Seed and Integrity Verifier (after 7a+7b merged)

**Files:** per spec §Task 8 (lines 559–572): `internal/seed/{config,vocabulary,catalog,customers,orders,payments,fulfillment,load,verify}.go`, `{seed,verify}_test.go`, `testdata/dev-summary.json`, `cmd/seed/main.go`.

**Interfaces:**
- Produces CLI: `seed generate --scale dev|demo|load --seed N --reset` and `seed verify --scale dev|demo|load`.
- Produces `GenerateSummary(Config) Summary` with exact counts (dev: 80 products, 100 orders).
- `VerifyFixture(fixture) error` classifies integrity violations (`ErrMoneyMismatch`, etc.).
- Uses **separate deterministic random streams per domain** (adding a customer field must not reshuffle orders). pgx batch for dev/demo, `CopyFrom` in bounded chunks for load (never retain all load rows).

- [ ] **Step 1: Write failing determinism/verifier tests** (spec §Task 8 Step 1, lines 581–597): `TestDevSeedMatchesGoldenSummary` (seed 42 twice → identical; 80 products, 100 orders; golden JSON), `TestVerifyRejectsBrokenOrderEquation`.
- [ ] **Step 2: Verify RED** — `go test ./internal/seed` fails.
- [ ] **Step 3: Implement** generators + streaming loaders + verifier per spec §Task 8 Step 3 (lines 605–612). Catalog/customer/payment/fulfillment generators must emit rows consistent with the schemas and the event lifecycle (statuses that satisfy the CHECK constraints and triggers).
- [ ] **Step 4: Verify GREEN** — `go test ./internal/seed -count=1` PASS; golden JSON committed.
- [ ] **Step 5: CLI smoke** (needs PostgreSQL): if `TEST_DATABASE_URL` set, `go run ./cmd/seed generate --scale dev --seed 42 --reset && go run ./cmd/seed verify --scale dev`; else skip runtime and note it.
- [ ] **Step 6: Static + race** — `go vet ./internal/seed ./cmd/seed`; `go test -race ./internal/seed`.
- [ ] **Step 7: Commit** — `feat(commerce): generate realistic deterministic data`.

---

## Task 9: Production-Shaped Docker Compose Topology (after 8)

**Files:** per spec §Task 9 (lines 632–644): `Dockerfile`, `.dockerignore`, `compose.yaml` (replaces docker-compose.yaml), `compose.observability.yaml`, `compose.load.yaml`, `deploy/{traefik,rabbitmq,otel,prometheus,grafana}/...`, `test/smoke.sh`, `Makefile`.

**Interfaces:** `make up/scale/smoke/observability/seed/verify-seed/down`; Traefik entry at `localhost:18100`; six PostgreSQL services+volumes; RabbitMQ definitions; edge/service/data networks; no fixed container names; app/db ports unpublished.

- [ ] **Step 1: Write smoke script** (spec §Task 9 Step 1, lines 651–666): proves ≥2 instance IDs, checkout success, deterministic payment failure, duplicate webhook replay, inventory compensation, fulfillment check.
- [ ] **Step 2: Verify RED** — `docker compose config` fails (no compose.yaml).
- [ ] **Step 3: Implement** images/networks/dependencies/scaling/profiles per spec §Task 9 Step 3 (lines 675–682). Pinned Go builder, non-root distroless runtime, read-only fs, tmpfs, init, health checks, stop grace, resource controls, `--scale` defaults + `--wait`.
- [ ] **Step 4: Static verification** — `docker compose config --quiet` exits 0; `docker compose -f compose.observability.yaml config --quiet`; `docker compose -f compose.load.yaml config --quiet`. (Acceptance gate A; runtime `make smoke` deferred to phase B.)
- [ ] **Step 5: Commit** — `feat(commerce): add scalable compose topology`.

---

## Task 10: Kubernetes Equivalents and Documentation (after 9)

**Files:** per spec §Task 10 (lines 711–722): `deploy/k8s/{namespace,config,apps,services,gateway,availability,kustomization}.yaml`, `README.md`, `ARCHITECTURE.md`, `template.yaml`, root `README.md` + `README.zh-CN.md`.

**Interfaces:** `kubectl apply -k deploy/k8s`; docs cover quick start, endpoints, scaling, load-balancing verification, checkout flow, event guarantees, failure injection, broker inspection, seed tiers, load-tier resource warning, observability, K8s mapping, cleanup, explicit non-goals.

- [ ] **Step 1: Verification probes** (spec §Task 10 Step 1, lines 728–736): `test -f deploy/k8s/kustomization.yaml`; `rg -n "make scale|make smoke|make verify-seed|non-goals|at least once" README.md ARCHITECTURE.md`.
- [ ] **Step 2: Verify RED** — manifests/docs absent.
- [ ] **Step 3: Implement** K8s resources + docs per spec §Task 10 Steps 2–3 (lines 740–755). Replica defaults, ClusterIP services, probes, resource requests/limits, topology spread, PDBs, HPAs, Gateway/Ingress; dev-only Secret examples labeled unsafe; no PG/RabbitMQ HA claims.
- [ ] **Step 4: Static verification** — `kubectl kustomize deploy/k8s >/tmp/commerce-k8s.yaml` exits 0; `docker compose config --quiet`; `rg -n "make scale|make smoke|make verify-seed|at least once|4–6 GB" README.md ARCHITECTURE.md` finds every topic.
- [ ] **Step 5: Commit** — `docs(commerce): document deployment and operations`.

---

## Task 11: Package Template and Full Acceptance (after 10)

**Files:** `internal/template/templates.tar` (root, regenerated). Tests run in a temp dir outside the repo.

- [ ] **Step 1: Pre-package tests** — from repo root: `go test ./...`; from `templates/commerce`: `go test -race ./...`. Both exit 0.
- [ ] **Step 2: Rebuild archive** — `go generate ./internal/template`; `templates.tar` changes and contains the new commerce files.
- [ ] **Step 3: Parity check** — `tmp=$(mktemp -d)`; `go run ./cmd/jiade init --template commerce --dir "$tmp/shop"`; `diff -qr templates/commerce "$tmp/shop"` exits 0.
- [ ] **Step 4: Generated-project acceptance** (phase B, needs Docker): `make -C "$tmp/shop" up && make smoke && make verify-seed SCALE=dev`; then `docker compose -f "$tmp/shop/compose.yaml" down --volumes --remove-orphans`. If runtime deferred, record as pending.
- [ ] **Step 5: Final diff review** — `git diff --check`; `git status --short`; `git diff --stat`. No whitespace errors; changes limited to commerce template, its archive, root summaries, and Superpowers docs.
- [ ] **Step 6: Commit** — `build: package scalable commerce template`.

---

## Final Verification Checklist (spec lines 847–858)

- [ ] `go test ./...` passes in Jiade root.
- [ ] `go test -race ./...` passes in `templates/commerce`.
- [ ] Every migration applies twice without error.
- [ ] Dev seed has exactly 100 orders and passes every integrity check.
- [ ] `docker compose config --quiet` passes.
- [ ] `make smoke` proves multi-instance routing, failure removal, duplicate suppression, payment compensation, successful fulfillment. (phase B)
- [ ] Generated template files exactly match `templates/commerce`.
- [ ] Runtime containers/networks/volumes removed.
- [ ] README and ARCHITECTURE state the resource budget and non-goals.

## Phase gates (acceptance strategy C)

- **Phase A (must):** all code written; unit + integration-tag tests pass; `docker compose config --quiet`; `kubectl kustomize`; `go test -race` on changed packages; parity `diff`. Each task commits.
- **Phase B (if context/resources permit):** `make up && make smoke`, `verify-seed`, generated-project acceptance. Heavy (4–6 GB Docker); record evidence or mark pending if not reached.

## Execution order

1. Task 7a ∥ Task 7b (two subagents, merge, then `go test ./...`).
2. Task 8 (sequential).
3. Task 9 (sequential).
4. Task 10 (sequential).
5. Task 11 (sequential; phase B runtime optional).
