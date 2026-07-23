# Commerce Production Topology Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the built-in commerce seed into a locally runnable, production-shaped Go microservice system with health-aware Compose scaling, resilient HTTP calls, RabbitMQ Outbox/Inbox workflows, and realistic deterministic data.

**Architecture:** Keep six service-owned PostgreSQL databases. Put Traefik in front of horizontally scalable stateless services, use synchronous HTTP only for checkout validation and reservation, and use RabbitMQ topic events plus a transactional Outbox/Inbox for state propagation. Build domain-focused Go packages behind transport and persistence interfaces, and provide deterministic batched/COPY seed tiers.

**Tech Stack:** Go 1.22+, standard `net/http`, `pgx/v5`, `amqp091-go`, Prometheus client, PostgreSQL 16, RabbitMQ 4 management image, Traefik 3, Docker Compose v2, Kubernetes manifests.

## Global Constraints

- Docker Compose is the default deployment experience; Kubernetes is an equivalent deployment appendix.
- The default stack targets 4–6 GB memory.
- Default replicas are catalog=2, customer=1, inventory=2, order=2, payment=1, fulfillment=1.
- External traffic enters only through Traefik on host port `18100`.
- Each service owns a separate PostgreSQL container, database, user-facing schema, and volume.
- Money uses `int64` minor units; binary floating point is forbidden.
- Event delivery is at least once; consumers must be idempotent.
- The same seed, scale, and generator version must produce the same data.
- Exact order counts are dev=100, demo=10,000, load=1,000,000.
- The million-order seed is opt-in and must stream data in bounded memory.
- Do not implement authentication, a real payment provider, search, Redis, a promotion DSL, RabbitMQ clustering, or PostgreSQL HA.

---

## File Map

The implementation replaces `internal/platform/app` rather than growing it:

```text
templates/commerce/
  cmd/<service>/main.go              service-specific composition
  cmd/seed/main.go                   seed CLI only
  internal/catalog/                  catalog queries and price snapshots
  internal/customer/                 customer/address validation
  internal/inventory/                reservation state machine and store
  internal/order/                    carts, checkout, saga, projections
  internal/payment/                  intent/attempt/refund workflow
  internal/fulfillment/              warehouse split and shipment workflow
  internal/platform/config/          validated environment configuration
  internal/platform/httpx/           server, middleware, problem responses
  internal/platform/client/          resilient internal HTTP client
  internal/platform/postgres/        pgx pool and transaction helpers
  internal/platform/messaging/       event envelope, Outbox, Inbox, RabbitMQ
  internal/platform/telemetry/       JSON logging and Prometheus metrics
  internal/seed/                     deterministic generators/loaders/verifier
  db/migrations/*.sql                service-owned schemas
  deploy/traefik/                    gateway configuration
  deploy/rabbitmq/                   broker definitions
  deploy/k8s/                        Kubernetes equivalents
  compose.yaml                       default topology
  compose.observability.yaml         optional telemetry stack
  compose.load.yaml                  load-tier resource overrides
```

### Task 1: Platform Configuration, PostgreSQL, and HTTP Server

**Files:**
- Create: `templates/commerce/internal/platform/config/config.go`
- Create: `templates/commerce/internal/platform/config/config_test.go`
- Create: `templates/commerce/internal/platform/postgres/postgres.go`
- Create: `templates/commerce/internal/platform/httpx/problem.go`
- Create: `templates/commerce/internal/platform/httpx/middleware.go`
- Create: `templates/commerce/internal/platform/httpx/server.go`
- Create: `templates/commerce/internal/platform/httpx/server_test.go`
- Create: `templates/commerce/internal/platform/telemetry/log.go`
- Modify: `templates/commerce/go.mod`

**Interfaces:**
- Produces: `config.Load(service string) (config.Config, error)`.
- Produces: `postgres.Open(context.Context, config.Database) (*pgxpool.Pool, error)`.
- Produces: `httpx.NewServer(httpx.ServerConfig) *httpx.Server`.
- Produces: `httpx.WriteProblem(http.ResponseWriter, httpx.Problem)`.

- [ ] **Step 1: Write failing configuration and HTTP lifecycle tests**

```go
func TestLoadRejectsMissingDatabaseName(t *testing.T) {
    t.Setenv("DB_NAME", "")
    _, err := Load("order")
    if !errors.Is(err, ErrInvalidConfig) {
        t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
    }
}

func TestLiveAndReadyHaveDifferentDependencySemantics(t *testing.T) {
    ready := atomic.Bool{}
    s := NewServer(ServerConfig{Service: "order", Instance: "order-1",
        Ready: func(context.Context) error {
            if ready.Load() { return nil }
            return errors.New("database unavailable")
        }})
    assertStatus(t, s.Handler(), "/livez", http.StatusOK)
    assertStatus(t, s.Handler(), "/readyz", http.StatusServiceUnavailable)
    ready.Store(true)
    assertStatus(t, s.Handler(), "/readyz", http.StatusOK)
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/platform/config ./internal/platform/httpx`

Expected: FAIL because the packages and exported APIs do not exist.

- [ ] **Step 3: Implement validated config and bounded server primitives**

Implement explicit structs for database, broker, HTTP server, clients, Outbox,
and shutdown settings. Parse durations and integers with field-specific errors.
Build a standard-library server with `/livez`, `/readyz`, `/metrics`,
`X-Request-ID`, `X-Service-Instance`, panic recovery, JSON access logging,
request body limits, timeouts, and graceful `Shutdown`.

Core API:

```go
type ServerConfig struct {
    Service, Instance string
    Addr string
    Handler http.Handler
    Ready func(context.Context) error
    Registry *prometheus.Registry
    ShutdownTimeout time.Duration
}

type Problem struct {
    Type, Title, Code, Detail, Instance string
    Status int
}
```

- [ ] **Step 4: Run focused and module tests**

Run: `cd templates/commerce && go test ./internal/platform/...`

Expected: PASS with no data races or leaked listener goroutines.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/go.mod templates/commerce/go.sum templates/commerce/internal/platform
git commit -m "feat(commerce): add production HTTP platform"
```

### Task 2: Resilient Internal HTTP Client

**Files:**
- Create: `templates/commerce/internal/platform/client/client.go`
- Create: `templates/commerce/internal/platform/client/breaker.go`
- Create: `templates/commerce/internal/platform/client/client_test.go`
- Create: `templates/commerce/internal/platform/client/breaker_test.go`

**Interfaces:**
- Consumes: request ID and trace headers installed by `httpx`.
- Produces: `client.New(client.Config) *client.Client`.
- Produces: `(*client.Client).Do(context.Context, *http.Request, client.Policy) (*http.Response, error)`.

- [ ] **Step 1: Write failing retry eligibility, deadline, propagation, and breaker tests**

```go
func TestClientRetriesIdempotentRequestOnly(t *testing.T) {
    var calls atomic.Int32
    upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if calls.Add(1) == 1 { http.Error(w, "temporary", 503); return }
        w.WriteHeader(204)
    }))
    defer upstream.Close()

    req, _ := http.NewRequest(http.MethodPost, upstream.URL, nil)
    req.Header.Set("Idempotency-Key", "checkout-1")
    res, err := newTestClient().Do(context.Background(), req, Policy{MaxAttempts: 2})
    if err != nil || res.StatusCode != 204 || calls.Load() != 2 {
        t.Fatalf("status=%v calls=%d err=%v", res.StatusCode, calls.Load(), err)
    }
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
    b := NewBreaker(BreakerConfig{FailureThreshold: 3, OpenFor: time.Second})
    for range 3 { b.Record(false) }
    if err := b.Allow(); !errors.Is(err, ErrCircuitOpen) {
        t.Fatalf("Allow() = %v", err)
    }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/platform/client -count=1`

Expected: FAIL because `Client`, `Policy`, and `Breaker` are undefined.

- [ ] **Step 3: Implement bounded retry and circuit breaking**

Retries are allowed only for GET/HEAD or requests carrying
`Idempotency-Key`. Retry connection errors, 429, 502, 503, and 504. Respect the
context deadline and `Retry-After`; otherwise use capped exponential backoff
with injectable deterministic jitter for tests. Drain and close retryable
responses. Propagate `X-Request-ID`, `traceparent`, and idempotency headers.
Implement closed/open/half-open breaker states per upstream.

- [ ] **Step 4: Run focused tests with the race detector**

Run: `cd templates/commerce && go test -race ./internal/platform/client -count=1`

Expected: PASS and exactly the asserted attempt counts.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/internal/platform/client
git commit -m "feat(commerce): add resilient service client"
```

### Task 3: Event Envelope, Transactional Outbox, and Idempotent Inbox

**Files:**
- Create: `templates/commerce/internal/platform/messaging/event.go`
- Create: `templates/commerce/internal/platform/messaging/outbox.go`
- Create: `templates/commerce/internal/platform/messaging/inbox.go`
- Create: `templates/commerce/internal/platform/messaging/rabbitmq.go`
- Create: `templates/commerce/internal/platform/messaging/messaging_test.go`
- Create: `templates/commerce/internal/platform/messaging/integration_test.go`
- Create: `templates/commerce/db/migrations/shared.sql`
- Modify: `templates/commerce/go.mod`

**Interfaces:**
- Produces: `messaging.Event`, `messaging.NewEvent(...)`.
- Produces: `messaging.InsertOutbox(context.Context, pgx.Tx, Event) error`.
- Produces: `messaging.RunRelay(context.Context, *pgxpool.Pool, Publisher, RelayConfig) error`.
- Produces: `messaging.HandleOnce(context.Context, pgx.Tx, consumer string, Event, func() error) error`.

- [ ] **Step 1: Write failing envelope and duplicate-consumer tests**

```go
func TestEventRoundTripPreservesTracingFields(t *testing.T) {
    e := NewEvent("order.placed.v1", "ORD-1", "corr-1", "cause-1",
        json.RawMessage(`{"total_minor":1200}`), fixedClock)
    got := decode(encode(e))
    if diff := cmp.Diff(e, got); diff != "" { t.Fatal(diff) }
}

func TestHandleOnceSkipsDuplicateEvent(t *testing.T) {
    calls := 0
    consumeTwice(t, event, func() error { calls++; return nil })
    if calls != 1 { t.Fatalf("calls=%d, want 1", calls) }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/platform/messaging -count=1`

Expected: FAIL because the messaging package APIs are missing.

- [ ] **Step 3: Implement reliable messaging**

Add `outbox_event` and `inbox_event` DDL suitable for inclusion in every
service migration. Claim relay rows with `FOR UPDATE SKIP LOCKED`, publish
persistent messages with `mandatory=true`, wait for publisher confirmation,
and update `published_at` and attempt metadata. Consumers use manual ack after
their transaction commits. Reject non-retryable events to DLQ and send transient
failures through bounded TTL retry queues.

- [ ] **Step 4: Run unit and broker/database integration tests**

Run: `cd templates/commerce && go test ./internal/platform/messaging -count=1`

Run: `cd templates/commerce && go test -tags=integration ./internal/platform/messaging -count=1`

Expected: PASS; the integration test demonstrates a duplicate delivery causing
one database mutation and one Inbox row.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/go.mod templates/commerce/go.sum templates/commerce/internal/platform/messaging templates/commerce/db/migrations/shared.sql
git commit -m "feat(commerce): add reliable event delivery"
```

### Task 4: Expand Service-Owned Schemas and Domain Invariants

**Files:**
- Replace: `templates/commerce/db/migrations/catalog_db.sql`
- Replace: `templates/commerce/db/migrations/customer_db.sql`
- Replace: `templates/commerce/db/migrations/inventory_db.sql`
- Replace: `templates/commerce/db/migrations/order_db.sql`
- Replace: `templates/commerce/db/migrations/payment_db.sql`
- Replace: `templates/commerce/db/migrations/fulfillment_db.sql`
- Create: `templates/commerce/internal/inventory/model.go`
- Create: `templates/commerce/internal/inventory/model_test.go`
- Create: `templates/commerce/internal/order/money.go`
- Create: `templates/commerce/internal/order/money_test.go`
- Create: `templates/commerce/internal/order/state.go`
- Create: `templates/commerce/internal/order/state_test.go`
- Create: `templates/commerce/internal/payment/state.go`
- Create: `templates/commerce/internal/payment/state_test.go`

**Interfaces:**
- Produces: `order.CalculateTotals(lines []Line, shipping, tax int64) (Totals, error)`.
- Produces: conditional `inventory.Reservation` and `order.State` transitions.
- Produces: six idempotent migrations including Outbox/Inbox tables and indexes.

- [ ] **Step 1: Write failing invariant and state transition tests**

```go
func TestCalculateTotalsAllocatesDiscountWithoutLosingMinorUnits(t *testing.T) {
    got, err := CalculateTotals([]Line{
        {Quantity: 2, UnitPriceMinor: 999, DiscountMinor: 101},
        {Quantity: 1, UnitPriceMinor: 500, DiscountMinor: 50},
    }, 800, 125)
    if err != nil { t.Fatal(err) }
    if got.Subtotal != 2498 || got.Discount != 151 || got.Total != 3272 {
        t.Fatalf("totals=%+v", got)
    }
}

func TestPaidOrderCannotTransitionBackToPending(t *testing.T) {
    if _, err := Transition(StateConfirmed, EventCheckoutStarted); !errors.Is(err, ErrInvalidTransition) {
        t.Fatalf("error=%v", err)
    }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/inventory ./internal/order ./internal/payment`

Expected: FAIL because domain types and migrations are not implemented.

- [ ] **Step 3: Implement schemas and pure domain logic**

Create the exact tables approved in the design: category hierarchy, brands,
media/options/variants/prices; customers/addresses/tiers/consents; locations,
levels/reservations/movements; carts/orders/items/allocations/history/saga;
intents/methods/attempts/refunds/webhooks; fulfillment/picks/packages/shipments/
tracking. Add check constraints for money and quantities, partial indexes for
pending Outbox and active reservations, and unique idempotency/provider keys.

- [ ] **Step 4: Apply all migrations twice against PostgreSQL**

Run: `cd templates/commerce && go test -tags=integration ./internal/... -run 'Migration|Invariant' -count=1`

Expected: PASS; running every migration twice makes no destructive change and
all database constraints reject invalid fixtures.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/db/migrations templates/commerce/internal/inventory templates/commerce/internal/order templates/commerce/internal/payment
git commit -m "feat(commerce): model realistic commerce domains"
```

### Task 5: Catalog, Customer, and Inventory HTTP Services

**Files:**
- Create: `templates/commerce/internal/catalog/service.go`
- Create: `templates/commerce/internal/catalog/store.go`
- Create: `templates/commerce/internal/catalog/http.go`
- Create: `templates/commerce/internal/catalog/http_test.go`
- Create: `templates/commerce/internal/customer/service.go`
- Create: `templates/commerce/internal/customer/store.go`
- Create: `templates/commerce/internal/customer/http.go`
- Create: `templates/commerce/internal/customer/http_test.go`
- Create: `templates/commerce/internal/inventory/service.go`
- Create: `templates/commerce/internal/inventory/store.go`
- Create: `templates/commerce/internal/inventory/http.go`
- Create: `templates/commerce/internal/inventory/http_test.go`
- Replace: `templates/commerce/cmd/catalog/main.go`
- Replace: `templates/commerce/cmd/customer/main.go`
- Replace: `templates/commerce/cmd/inventory/main.go`

**Interfaces:**
- Produces: `GET /api/v1/products`, `GET /api/v1/products/{id}`.
- Produces: internal customer/address validation.
- Produces: idempotent `POST /internal/v1/reservations` and release endpoint.

- [ ] **Step 1: Write failing API contract tests**

```go
func TestReserveReturnsExistingReservationForSameKey(t *testing.T) {
    body := `{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":2}]}`
    first := postJSON(t, handler, "/internal/v1/reservations", body, "reserve-1")
    second := postJSON(t, handler, "/internal/v1/reservations", body, "reserve-1")
    if first.Code != 201 || second.Code != 200 || first.Body.String() != second.Body.String() {
        t.Fatalf("first=%d second=%d", first.Code, second.Code)
    }
}
```

Also test cursor pagination, page-size caps, missing SKU, insufficient stock,
address ownership, JSON problems, and instance/request headers.

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/catalog ./internal/customer ./internal/inventory`

Expected: FAIL because handlers and services are missing.

- [ ] **Step 3: Implement ports, pgx stores, use cases, and handlers**

Inventory reservation must lock candidate rows, allocate by location priority,
insert movements and reservations, and preserve availability. No handler may
contain SQL. Command packages only load config, open dependencies, install
routes, start consumers/relay, and run the HTTP server.

- [ ] **Step 4: Run service and integration tests**

Run: `cd templates/commerce && go test ./internal/catalog ./internal/customer ./internal/inventory`

Run: `cd templates/commerce && go test -tags=integration ./internal/catalog ./internal/customer ./internal/inventory`

Expected: PASS, including concurrent reservation tests that never oversell.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/cmd/catalog templates/commerce/cmd/customer templates/commerce/cmd/inventory templates/commerce/internal/catalog templates/commerce/internal/customer templates/commerce/internal/inventory
git commit -m "feat(commerce): implement catalog customer inventory services"
```

### Task 6: Cart, Checkout, and Order Saga

**Files:**
- Create: `templates/commerce/internal/order/service.go`
- Create: `templates/commerce/internal/order/store.go`
- Create: `templates/commerce/internal/order/clients.go`
- Create: `templates/commerce/internal/order/http.go`
- Create: `templates/commerce/internal/order/consumer.go`
- Create: `templates/commerce/internal/order/service_test.go`
- Create: `templates/commerce/internal/order/http_test.go`
- Create: `templates/commerce/internal/order/integration_test.go`
- Replace: `templates/commerce/cmd/order/main.go`

**Interfaces:**
- Consumes: catalog snapshot, customer validation, and inventory reservation HTTP APIs.
- Consumes: payment and fulfillment result events.
- Produces: cart mutation, checkout, cancellation, and order query APIs.
- Produces: `order.placed.v1`, `order.cancelled.v1`, and `order.paid.v1`.

- [ ] **Step 1: Write failing checkout and compensation tests**

```go
func TestCheckoutSnapshotsPricesAndWritesOutboxAtomically(t *testing.T) {
    svc := newCheckoutFixture(t)
    got, err := svc.Checkout(ctx, CheckoutCommand{CartID: "CART-1", IdempotencyKey: "checkout-1"})
    if err != nil { t.Fatal(err) }
    if got.TotalMinor != 3272 { t.Fatalf("total=%d", got.TotalMinor) }
    assertOutboxEvent(t, "order.placed.v1", got.OrderID)
}

func TestPaymentFailureCancelsAndEmitsInventoryRelease(t *testing.T) {
    consumePaymentFailedTwice(t, "evt-1", "ORD-1")
    assertOrderState(t, "ORD-1", "cancelled", "failed")
    assertSingleOutboxEvent(t, "inventory.release-requested.v1", "ORD-1")
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/order -count=1`

Expected: FAIL on missing checkout and saga behavior.

- [ ] **Step 3: Implement carts, checkout transaction, saga, and projections**

Implement cursor-paginated reads and idempotent mutations. Snapshot customer,
address, product, SKU, price, discount allocation, and tax. Store saga steps
with conditional updates. On business failure, emit compensation in reverse
order. Late or duplicate events must be no-ops after Inbox recording.

- [ ] **Step 4: Run unit and real-dependency integration tests**

Run: `cd templates/commerce && go test ./internal/order -count=1`

Run: `cd templates/commerce && go test -tags=integration ./internal/order -count=1`

Expected: PASS for success, payment failure, cancellation-after-capture, timeout,
idempotent replay, and duplicate events.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/cmd/order templates/commerce/internal/order
git commit -m "feat(commerce): implement checkout saga"
```

### Task 7: Payment and Fulfillment Event Workflows

**Files:**
- Create: `templates/commerce/internal/payment/service.go`
- Create: `templates/commerce/internal/payment/store.go`
- Create: `templates/commerce/internal/payment/provider.go`
- Create: `templates/commerce/internal/payment/http.go`
- Create: `templates/commerce/internal/payment/consumer.go`
- Create: `templates/commerce/internal/payment/service_test.go`
- Create: `templates/commerce/internal/fulfillment/service.go`
- Create: `templates/commerce/internal/fulfillment/store.go`
- Create: `templates/commerce/internal/fulfillment/http.go`
- Create: `templates/commerce/internal/fulfillment/consumer.go`
- Create: `templates/commerce/internal/fulfillment/service_test.go`
- Replace: `templates/commerce/cmd/payment/main.go`
- Replace: `templates/commerce/cmd/fulfillment/main.go`
- Delete: `templates/commerce/internal/platform/app/app.go`

**Interfaces:**
- Consumes: `order.placed.v1`, `order.cancelled.v1`, `order.paid.v1`.
- Produces: payment success/failure/refund events and fulfillment state events.
- Produces: deterministic simulated provider webhook and query APIs.

- [ ] **Step 1: Write failing payment and warehouse split tests**

```go
func TestTransientPaymentGetsTwoAttemptsThenSucceeds(t *testing.T) {
    got := runScenario(t, ScenarioProviderTimeoutThenSuccess)
    if len(got.Attempts) != 2 || got.Intent.Status != StatusSucceeded {
        t.Fatalf("result=%+v", got)
    }
}

func TestPaidOrderSplitsFulfillmentByWarehouse(t *testing.T) {
    got := fulfill(t, paidOrderWithLines("LOC-1", "LOC-2"))
    if len(got.FulfillmentOrders) != 2 { t.Fatalf("count=%d", len(got.FulfillmentOrders)) }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/payment ./internal/fulfillment`

Expected: FAIL because workflows do not exist.

- [ ] **Step 3: Implement deterministic provider and event workflows**

Derive simulated provider outcomes from explicit scenario inputs, never wall
clock randomness. Persist intent, method snapshot, attempts, refund, and Outbox
atomically. Fulfillment groups reserved lines by location, creates picks and
packages, and advances shipment projections from events. Webhook provider IDs
and event IDs are unique and replay-safe.

- [ ] **Step 4: Run unit and integration tests**

Run: `cd templates/commerce && go test ./internal/payment ./internal/fulfillment`

Run: `cd templates/commerce && go test -tags=integration ./internal/payment ./internal/fulfillment`

Expected: PASS for success, hard decline, transient retry, full/partial refund,
multi-warehouse split, duplicate delivery, and carrier exception.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/cmd/payment templates/commerce/cmd/fulfillment templates/commerce/internal/payment templates/commerce/internal/fulfillment
git commit -m "feat(commerce): add payment and fulfillment workflows"
```

### Task 8: Deterministic Realistic Seed and Integrity Verifier

**Files:**
- Replace: `templates/commerce/cmd/seed/main.go`
- Create: `templates/commerce/internal/seed/config.go`
- Create: `templates/commerce/internal/seed/vocabulary.go`
- Create: `templates/commerce/internal/seed/catalog.go`
- Create: `templates/commerce/internal/seed/customers.go`
- Create: `templates/commerce/internal/seed/orders.go`
- Create: `templates/commerce/internal/seed/payments.go`
- Create: `templates/commerce/internal/seed/fulfillment.go`
- Create: `templates/commerce/internal/seed/load.go`
- Create: `templates/commerce/internal/seed/verify.go`
- Create: `templates/commerce/internal/seed/seed_test.go`
- Create: `templates/commerce/internal/seed/verify_test.go`
- Create: `templates/commerce/internal/seed/testdata/dev-summary.json`

**Interfaces:**
- Produces: `seed Generate --scale dev|demo|load --seed N --reset`.
- Produces: `seed Verify --scale dev|demo|load`.
- Produces exact order counts and bounded SKU/order-line ranges from the spec.

- [ ] **Step 1: Write failing determinism, distribution, and verifier tests**

```go
func TestDevSeedMatchesGoldenSummary(t *testing.T) {
    a := GenerateSummary(Config{Scale: Dev, Seed: 42})
    b := GenerateSummary(Config{Scale: Dev, Seed: 42})
    if diff := cmp.Diff(a, b); diff != "" { t.Fatal(diff) }
    assertGoldenJSON(t, "testdata/dev-summary.json", a)
    if a.Orders != 100 || a.Products != 80 { t.Fatalf("summary=%+v", a) }
}

func TestVerifyRejectsBrokenOrderEquation(t *testing.T) {
    fixture := validFixture()
    fixture.Orders[0].TotalMinor++
    if err := VerifyFixture(fixture); !errors.Is(err, ErrMoneyMismatch) {
        t.Fatalf("error=%v", err)
    }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `cd templates/commerce && go test ./internal/seed -count=1`

Expected: FAIL because generator and verifier are missing.

- [ ] **Step 3: Implement domain generators and streaming loaders**

Use separate deterministic random streams per domain so adding a customer field
does not reshuffle order outcomes. Generate curated bilingual catalog and
fictional regional customer data, Zipf-like product demand, time-of-day traffic,
repeat buyers, carts, derived money, stock movements, attempts, packages, and
tracking histories. Use pgx batch for dev/demo and `CopyFrom` in bounded chunks
for load. Never retain all load rows in memory.

- [ ] **Step 4: Run seed tests and database verification**

Run: `cd templates/commerce && go test ./internal/seed -count=1`

Run: `cd templates/commerce && go run ./cmd/seed generate --scale dev --seed 42 --reset && go run ./cmd/seed verify --scale dev`

Expected: PASS and a summary containing exactly 80 products, 100 orders, SKU and
line counts inside their specified ranges, with zero integrity violations.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/cmd/seed templates/commerce/internal/seed
git commit -m "feat(commerce): generate realistic deterministic data"
```

### Task 9: Production-Shaped Docker Compose Topology

**Files:**
- Replace: `templates/commerce/Dockerfile`
- Replace: `templates/commerce/docker-compose.yaml` with `templates/commerce/compose.yaml`
- Create: `templates/commerce/compose.observability.yaml`
- Create: `templates/commerce/compose.load.yaml`
- Create: `templates/commerce/.dockerignore`
- Create: `templates/commerce/deploy/traefik/traefik.yaml`
- Create: `templates/commerce/deploy/rabbitmq/definitions.json`
- Create: `templates/commerce/deploy/otel/collector.yaml`
- Create: `templates/commerce/deploy/prometheus/prometheus.yaml`
- Create: `templates/commerce/deploy/grafana/provisioning/datasources/datasources.yaml`
- Create: `templates/commerce/test/smoke.sh`
- Replace: `templates/commerce/Makefile`

**Interfaces:**
- Produces: `make up`, `make scale`, `make smoke`, `make observability`,
  `make seed`, `make verify-seed`, `make down`.
- Produces: Traefik entry point at `localhost:18100`.

- [ ] **Step 1: Add a smoke script that fails against the old topology**

The script must:

```sh
instances=""
for _ in $(seq 1 12); do
  instance=$(curl -fsS -D - http://localhost:18100/api/v1/products?limit=1 -o /dev/null |
    awk -F': ' 'tolower($1)=="x-service-instance" {gsub("\r","",$2); print $2}')
  instances="$instances $instance"
done
test "$(printf '%s\n' $instances | sort -u | wc -l | tr -d ' ')" -ge 2
```

It must also perform a checkout success, a deterministic payment failure,
duplicate webhook replay, inventory compensation check, and fulfillment check.

- [ ] **Step 2: Run static Compose and smoke validation to verify RED**

Run: `cd templates/commerce && docker compose config`

Expected: FAIL because `compose.yaml` and the new services/networks do not exist,
or smoke FAIL because only one instance is reachable.

- [ ] **Step 3: Implement images, networks, dependencies, scaling, and profiles**

Use a pinned Go builder and non-root distroless runtime. Configure read-only
filesystems, tmpfs, init, health checks, stop grace periods, resource controls,
six PostgreSQL services/volumes, RabbitMQ definitions, Traefik Docker discovery,
and edge/service/data networks. Do not set fixed container names or publish app
and database ports. `make up` must seed dev data and invoke Compose with the
approved default `--scale` values and `--wait`.

- [ ] **Step 4: Run Compose acceptance tests**

Run: `cd templates/commerce && docker compose config --quiet`

Run: `cd templates/commerce && make up && make smoke`

Run: `cd templates/commerce && make scale SERVICE=order REPLICAS=4 && make smoke`

Expected: all commands exit 0; repeated responses contain at least two instance
IDs before scaling and at least four order instance IDs after scaling.

- [ ] **Step 5: Tear down test resources**

Run: `cd templates/commerce && docker compose down --volumes --remove-orphans`

Expected: the project containers, networks, and test volumes are removed.

- [ ] **Step 6: Commit**

```bash
git add templates/commerce/Dockerfile templates/commerce/.dockerignore templates/commerce/compose*.yaml templates/commerce/deploy templates/commerce/test templates/commerce/Makefile
git commit -m "feat(commerce): add scalable compose topology"
```

### Task 10: Kubernetes Equivalents and Documentation

**Files:**
- Create: `templates/commerce/deploy/k8s/namespace.yaml`
- Create: `templates/commerce/deploy/k8s/config.yaml`
- Create: `templates/commerce/deploy/k8s/apps.yaml`
- Create: `templates/commerce/deploy/k8s/services.yaml`
- Create: `templates/commerce/deploy/k8s/gateway.yaml`
- Create: `templates/commerce/deploy/k8s/availability.yaml`
- Create: `templates/commerce/deploy/k8s/kustomization.yaml`
- Replace: `templates/commerce/README.md`
- Replace: `templates/commerce/ARCHITECTURE.md`
- Modify: `templates/commerce/template.yaml`
- Modify: `README.md`
- Modify: `README.zh-CN.md`

**Interfaces:**
- Produces: `kubectl apply -k deploy/k8s`.
- Documents Compose-to-Kubernetes mapping and all operational commands.

- [ ] **Step 1: Write documentation and manifest validation checks**

Run before implementation:

```bash
cd templates/commerce
test -f deploy/k8s/kustomization.yaml
rg -n "make scale|make smoke|make verify-seed|non-goals|at least once" README.md ARCHITECTURE.md
```

Expected: FAIL because manifests and required documentation are absent.

- [ ] **Step 2: Add Kubernetes resources**

Create Deployments with the approved replica defaults, ClusterIP Services,
readiness/liveness probes, resource requests/limits, topology spread,
PodDisruptionBudgets, HPAs for stateless services, and Gateway/Ingress routing.
Use concrete development-only values in `Secret` examples and label them as
unsafe for non-development use.
Do not claim to operate PostgreSQL or RabbitMQ HA.

- [ ] **Step 3: Rewrite user and architecture documentation**

Document quick start, endpoint examples, scaling, load balancing verification,
checkout flow, event guarantees, failure injection, broker inspection, seed
tiers, load-tier resource warning, observability profile, Kubernetes mapping,
cleanup, and explicit non-goals. Update the root English and Chinese template
summaries without duplicating the full commerce README.

- [ ] **Step 4: Validate YAML and documentation**

Run: `cd templates/commerce && kubectl kustomize deploy/k8s >/tmp/commerce-k8s.yaml`

Run: `cd templates/commerce && docker compose config --quiet`

Run: `cd templates/commerce && rg -n "make scale|make smoke|make verify-seed|at least once|4–6 GB" README.md ARCHITECTURE.md`

Expected: commands exit 0 and every required topic is found.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/deploy/k8s templates/commerce/README.md templates/commerce/ARCHITECTURE.md templates/commerce/template.yaml README.md README.zh-CN.md
git commit -m "docs(commerce): document deployment and operations"
```

### Task 11: Package Jiade Template and Run Full Acceptance

**Files:**
- Modify: `internal/template/templates.tar`
- Test generated output in a temporary directory outside the repository.

**Interfaces:**
- Consumes: the completed `templates/commerce` tree.
- Produces: `jiade init --template commerce` with the exact updated files.

- [ ] **Step 1: Run all Go tests before packaging**

Run: `go test ./...`

Run: `cd templates/commerce && go test -race ./...`

Expected: both commands exit 0 with no test failures.

- [ ] **Step 2: Rebuild the embedded template archive**

Run: `go generate ./internal/template`

Expected: `internal/template/templates.tar` changes and contains the new
commerce files.

- [ ] **Step 3: Verify generated template parity**

Run:

```bash
tmp_dir=$(mktemp -d)
go run ./cmd/jiade init --template commerce --dir "$tmp_dir/shop"
diff -qr templates/commerce "$tmp_dir/shop"
```

Expected: `diff` exits 0.

- [ ] **Step 4: Run generated-project acceptance**

Run:

```bash
tmp_dir=$(mktemp -d)
go run ./cmd/jiade init --template commerce --dir "$tmp_dir/shop"
make -C "$tmp_dir/shop" up
make -C "$tmp_dir/shop" smoke
make -C "$tmp_dir/shop" verify-seed SCALE=dev
docker compose -f "$tmp_dir/shop/compose.yaml" down --volumes --remove-orphans
```

Expected: build, seed, multi-instance smoke, and integrity verification all exit
0; cleanup removes temporary runtime resources.

- [ ] **Step 5: Review the final diff against the approved specification**

Run:

```bash
git diff --check
git status --short
git diff --stat
```

Expected: no whitespace errors; changes are limited to the commerce template,
its embedded archive, root template summaries, and Superpowers documents.

- [ ] **Step 6: Commit the packaged template**

```bash
git add internal/template/templates.tar
git commit -m "build: package scalable commerce template"
```

## Final Verification Checklist

- [ ] `go test ./...` passes in the Jiade root.
- [ ] `go test -race ./...` passes in `templates/commerce`.
- [ ] Every migration applies twice without error.
- [ ] The dev seed has exactly 100 orders and passes every integrity check.
- [ ] `docker compose config --quiet` passes.
- [ ] `make smoke` proves multi-instance routing, failure removal, duplicate
  event suppression, payment compensation, and successful fulfillment.
- [ ] Generated template files exactly match `templates/commerce`.
- [ ] Runtime containers, networks, and temporary volumes are removed.
- [ ] README and architecture docs state the resource budget and non-goals.
