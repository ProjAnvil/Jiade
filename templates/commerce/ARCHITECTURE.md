# Commerce Template — Architecture

This document describes the runtime topology of the commerce template, the
data flow for the checkout saga, the eventing guarantees, and the rationale
behind each deployment-shape decision. For operational commands, see
[README.md](README.md).

## Component map

```
                          External traffic
                                  |
                                  v
                          +---------------+
                          |   Traefik 3   |   (host port 18100, only published port)
                          |   gateway     |
                          +---------------+
                                  |
        +-----------+-------------+-------------+-----------+-----------+
        |           |             |             |           |           |
        v           v             v             v           v           v
   +---------+ +----------+ +-----------+ +---------+ +---------+ +-----------+
   | catalog | | customer | | inventory | |  order  | | payment | |fulfillment|
   |  (x2)   | |   (x1)   | |   (x2)    | |  (x2)   | |  (x1)   | |   (x1)    |
   +---------+ +----------+ +-----------+ +---------+ +---------+ +-----------+
        |           |             |             |           |           |
        |           |             |      HTTP (sync, for validation)
        |           |             |      +------>|<-------->|<--------+
        |           |             |      |       customer  catalog  inventory
        |           |             |      |
        v           v             v      v       v           v           v
   +---------+ +----------+ +-----------+ +---------+ +---------+ +-----------+
   |catalog- | |customer- | |inventory- | | order-  | |payment- | |fulfillment|
   |  db     | |   db     | |   db      | |  db     | |  db     | |    -db    |
   | Postgres| | Postgres | | Postgres  | | Postgres| | Postgres| | Postgres  |
   +---------+ +----------+ +-----------+ +---------+ +---------+ +-----------+

                              RabbitMQ 4 (single broker)
                              commerce.events topic exchange
                                      ^   ^
                                      |   |
                            Outbox dispatch (per service)
                            + Inbox dedupe (per consumer)
```

Each service is a stateless Go process owning a single PostgreSQL database
(catalog, customer, inventory, order, payment, fulfillment). Cross-service
**synchronous** reads happen over HTTP for things that must be true *now*
(customer exists, catalog price is current, inventory is reservable). All
**state propagation** goes through RabbitMQ topic events on the
`commerce.events` exchange, decoupling publishers from subscribers.

## Service responsibilities

| Service | Owns | Reads from (HTTP) | Subscribes to |
|---------|------|--------------------|---------------|
| catalog | products, SKUs, price snapshots | — | — |
| customer | customers, addresses | — | — |
| inventory | SKU stock, reservation state machine | — | `order.*`, `payment.*` (release on failure) |
| order | carts, checkout saga, order projections | customer, catalog, inventory | `payment.captured.v1`, `payment.failed.v1`, `fulfillment.*` |
| payment | intents, attempts, refunds, webhooks | — | `order.placed.v1` |
| fulfillment | warehouse splits, shipments, tracking | inventory | `payment.captured.v1` |

## Checkout saga

The order service is the saga orchestrator. The happy path:

1. **Validate** — `POST /api/v1/checkouts` calls customer (exists?),
   catalog (price current?), inventory (reservable?) synchronously. Any
   failure here aborts before any state is written.
2. **Reserve** — the order transactionally writes the order row plus an
   `outbox_event` carrying `inventory.reserve.v1`. The inventory consumer
   reserves stock (`active`) and replies via `inventory.reserved.v1`.
3. **Capture** — order publishes `payment.capture.v1`. The payment consumer
   runs its attempt workflow and publishes `payment.captured.v1` (or
   `payment.failed.v1`).
4. **Fulfill** — on `payment.captured.v1`, fulfillment splits the order
   across warehouses and creates shipments; order marks the order `paid`
   then `fulfilled` on the `fulfillment.shipped.v1` confirmation.

The compensation path mirrors this in reverse: `payment.failed.v1` is caught
by order, which publishes `inventory.release.v1`; inventory moves the
reservation from `active` to `released`. The saga is implemented as a small
state machine per order, not as a generic saga framework — the operations are
fixed and few.

## Eventing model

**At least once** delivery, made safe by two patterns:

### Transactional Outbox (publisher side)

Every state-changing transaction inserts an `outbox_event` row in the **same**
PostgreSQL transaction as the domain write. A per-service dispatcher polls
the table, publishes rows to RabbitMQ, and marks them dispatched. The
two-row insert and the domain mutation share a transaction, so the service
can never commit a state change without also recording the event that
describes it.

Schema lives in `db/migrations/shared.sql` (included verbatim by every
service migration):

- `outbox_event(event_id, event_type, schema_version, subject, correlation_id,
  causation_id, occurred_at, payload, claim_token, claimed_at, attempts,
  last_error, ...)`
- `claim_token` / `claimed_at` support multi-instance dispatch without
  double-publish: a dispatcher claims a row by writing its token, and only
  the claimant publishes.

### Idempotent Inbox (consumer side)

Every consumer records an `inbox_event(event_id, ...)` row before applying
its mutation. If the same `event_id` is delivered twice (dispatcher restart,
network retry, RabbitMQ redelivery), the second delivery finds the row
already present and skips. This is what makes at-least-once safe.

The duplicate-webhook smoke gate (gate 4) exercises this end-to-end: it
POSTs the same `Idempotency-Key` twice and asserts the returned payment id is
identical and the underlying state is applied once.

### Retry and dead-letter topology

RabbitMQ is configured in `deploy/rabbitmq/definitions.json`:

- `commerce.events` (topic) — main bus.
- `commerce.events.dlx` (topic) — dead-letter sink for poison messages.
- `commerce.events.retry` (direct) — backs the per-queue retry pattern.

Each consumer queue has a `<queue>.retry` companion with `x-message-ttl:
2000` and `x-dead-letter-exchange: commerce.events`. A consumer that wants
to retry a message publishes to the retry exchange; after the TTL the
message dead-letters back to the main exchange and is redelivered. After a
fixed number of attempts the message is routed to `<queue>.dlq` for human
inspection.

## Data ownership

- **Database-per-service**: a service queries only its own database. There
  is no shared schema, no cross-database foreign key, no read replica of
  another service's data.
- **Cross-domain reads go over HTTP.** Order calls customer, catalog, and
  inventory synchronously during checkout validation. The HTTP client
  (`internal/platform/client/`) is resilient: bounded retries with jitter,
  per-attempt and per-request timeouts, and a circuit breaker.
- **Money is int64 minor units** everywhere. Binary floating point is
  forbidden; non-monetary decimals (rates, NAV-like values) are stored as
  NUMERIC text. This mirrors the convention used across all jiade templates.

## Deployment shapes

### Compose (default)

`compose.yaml` is the source of truth. Each service has a fixed replica
default (`deploy.replicas`), a `mem_reservation`/`mem_limit`/`cpus` envelope,
a `read_only: true` root filesystem with a `tmpfs:/tmp`, `cap_drop: [ALL]`,
and `no-new-privileges`. Traefik labels declare the per-service routing rule
and the load-balancer healthcheck on `/livez`.

The Makefile wraps compose so `make up` is the one-shot bring-up. Two
overlays exist:

- `compose.observability.yaml` — adds otel/prometheus/grafana/jaeger.
- `compose.load.yaml` — raises per-service resources for the load tier.

### Kubernetes (equivalent)

`deploy/k8s/` mirrors compose.yaml. The mapping is documented in
[README.md](README.md#kubernetes-mapping); the highlights:

- **Deployments**, not StatefulSets, for all six services. They are
  stateless; their state lives in PostgreSQL.
- **ClusterIP Services** for intra-cluster calls. Session affinity is OFF so
  the Ingress round-robins across endpoints (the load-balancing
  verification probe in the smoke test works the same way).
- **Readiness/liveness probes** mirror compose's `healthcheck`: startup +
  readiness on `/readyz` (drains on shutdown), liveness on `/livez`.
- **Topology spread** (`maxSkew: 1` on `kubernetes.io/hostname`) plus a
  preferred podAntiAffinity push replicas of the same service onto different
  nodes when the cluster has them.
- **Resource requests/limits** mirror `mem_reservation`/`mem_limit`/`cpus`.
- **PodDisruptionBudgets** for every service with replicas >= 2 (catalog,
  inventory, order) so voluntary disruptions leave at least one healthy pod.
  Services at replicas=1 deliberately have no PDB.
- **HPAs** for every stateless service, scaling on CPU at 70% of the
  request. `minReplicas` matches the compose default; `maxReplicas` allows
  the headroom the load profile exercises.
- **Ingress** with path rules that mirror the Traefik labels 1:1. Gateway
  API is the production alternative (sketch in `gateway.yaml` comments).

### What the manifests do NOT do

- No PostgreSQL or RabbitMQ StatefulSet. The `*-db` and `rabbitmq` Services
  are ExternalName aliases so app Pods can resolve the same DNS names
  compose uses; replace them with your operator-managed backing services
  before non-dev use.
- No HA claims for the stateful backing services. compose ships them as
  single replicas and the k8s bundle does the same.
- No Secret management. The included `commerce-dev-secret` carries plaintext
  credentials that mirror compose.yaml and is labeled
  `commerce.jiade/unsafe: dev-only`. Replace with an external secret store.

## Observability surface

Per service, three HTTP endpoints are always exposed:

- `/livez` — liveness. Always 200 once the process is up.
- `/readyz` — readiness. 503 during shutdown drain; 200 otherwise.
- `/metrics` — Prometheus exposition: request latency histograms, Outbox
  dispatch lag, Inbox dedupe counters, pgx pool stats.

The observability overlay (`make observability`) wires these into a full
stack: an otel collector receiving traces over OTLP, Prometheus scraping
`/metrics`, Grafana for dashboards, and Jaeger for trace inspection. The
overlay only adds containers; it does not change the base topology.

## Configuration

All configuration flows through environment variables, parsed and validated
by `internal/platform/config/config.go`. Required keys per service:

- `SERVICE`, `INSTANCE_ID`, `DB_HOST`, `DB_NAME` — identity + database.
- `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_SSLMODE` — connection.
- `BROKER_URL` — RabbitMQ URL.
- `HTTP_ADDR` — listen address.
- `OUTBOX_BATCH_SIZE`, `OUTBOX_POLL_INTERVAL` — Outbox dispatcher tuning.
- `CLIENT_REQUEST_TIMEOUT`, `CLIENT_ATTEMPT_TIMEOUT` — downstream HTTP.
- `SHUTDOWN_TIMEOUT` — graceful shutdown drain.

Optional keys (with sensible defaults) exist for pool sizing, HTTP timeouts,
and connection healthcheck intervals. The compose ConfigMap sets the shared
defaults; per-service ConfigMaps set `DB_HOST`/`DB_NAME`/`INSTANCE_ID`/any
`*_URL` the service needs.

## Decisions worth recording

- **Why HTTP for checkout validation, events for everything else?** The
  checkout needs *current* truth: the customer must exist right now, the
  catalog price must be current, the SKU must be reservable. Events model
  eventual state propagation, which is wrong for these reads. Everything
  that does not need to be true right now (order placed, payment captured,
  fulfillment shipped) goes through events.
- **Why per-service databases?** A service that owns its database can change
  its schema without coordinating with anyone else, and a failure in one
  database cannot corrupt another service's data. The cost is more
  operational surface (six databases) — acceptable for a template whose
  purpose is to demonstrate the pattern.
- **Why a transactional Outbox instead of just publishing to RabbitMQ?** A
  service that publishes to RabbitMQ outside its write transaction can
  either (a) commit then crash before publishing, losing the event, or (b)
  publish then crash before committing, producing a phantom event. The
  Outbox makes the event part of the write transaction, eliminating both
  failure modes. The cost is a polling dispatcher per service — acceptable.
- **Why single-replica databases?** HA Postgres is a real engineering
  effort (streaming replication, failover, split-brain avoidance) that is
  unrelated to the patterns this template demonstrates. Single-replica
  keeps the focus on the application architecture. See [Non-goals](README.md#non-goals).
