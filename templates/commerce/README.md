# Commerce Template

A complete commerce backend microcosm: catalog/SKUs, customers, inventory
reservations, orders, payments/refunds, split fulfillment, and tracking. Six
Go microservices, six service-owned PostgreSQL databases, RabbitMQ, and a
single Traefik gateway — small enough to reason about, real enough to run end
to end.

The default stack targets a **4–6 GB** memory budget on a single Docker host
(or a kind/minikube node). Bigger tiers exist for load tests; see
[Seed tiers](#seed-tiers).

This template is part of [jiade](../../README.md). Generate a runnable copy
with:

```bash
jiade init --template commerce --dir ./myshop
cd myshop && make up
```

## Quick start

Requirements: Docker with compose, `make`, `curl`, `jq`.

```bash
# 1. Build, migrate, and seed the topology with default replicas. The Makefile
#    target waits for every service to be healthy before returning.
make up

# 2. Probe the gateway (host port 18100 is the only published port).
curl -fsS http://localhost:18100/api/v1/products?limit=1 | jq .
curl -fsS http://localhost:18100/api/v1/customers?limit=1 | jq .

# 3. Run the phase-B acceptance script (see "Failure injection" below).
make smoke

# 4. Tear everything down (volumes included).
make down
```

`make up` does **build → migrate → seed → wait-for-healthy** in one shot. It
is idempotent: re-running it rebuilds changed images, re-applies migrations,
and re-seeds with `--reset` (which drops and recreates the deterministic
fixtures).

## Topology

Six Go services plus their backing stores. Traefik is the only externally
reachable entrypoint — no service or database port is published.

| Service | Replicas | Database | Owns |
|---------|---------:|----------|------|
| catalog | 2 | catalog | Products, SKUs, price snapshots |
| customer | 1 | customer | Customers, addresses |
| inventory | 2 | inventory | SKU stock, reservation state machine |
| order | 2 | order | Carts, checkout saga, order projections |
| payment | 1 | payment | Payment intents/attempts, refunds, webhooks |
| fulfillment | 1 | fulfillment | Warehouse split, shipments, tracking |

Each service owns a dedicated PostgreSQL 16 container, database, schema, and
named volume (no shared database). Cross-service reads go over HTTP; state
propagation goes through RabbitMQ topic events. See
[ARCHITECTURE.md](ARCHITECTURE.md) for the data flow.

## Endpoints

External routes (only reachable through the Traefik gateway on `:18100`):

| Method | Path | Service | Notes |
|--------|------|---------|-------|
| GET | `/api/v1/products` | catalog | list/search products |
| GET | `/api/v1/customers` | customer | list customers |
| GET | `/api/v1/inventory` | inventory | SKU stock levels |
| POST/GET | `/api/v1/reservations/{order_id}` | inventory | reservation state |
| POST | `/api/v1/carts` / `/api/v1/carts/{id}/items` | order | cart lifecycle |
| POST | `/api/v1/checkouts` | order | checkout saga entrypoint |
| GET | `/api/v1/orders` / `/api/v1/orders/{id}` | order | order projections |
| GET | `/api/v1/payments/orders/{id}` | payment | payment intent view |
| POST | `/api/v1/payments/webhooks` | payment | idempotent webhook intake |
| GET | `/api/v1/fulfillment/orders/{id}` | fulfillment | fulfillment + shipments |

Internal-only routes (`/internal/v1/...`) are also routed through Traefik for
service-to-service calls.

Every response includes an `X-Service-Instance` header naming the replica
that served it. That header is the load-balancing verification probe — see
the next section.

## Scaling

Each service has an approved default replica count (see the table above). You
can override any of them per-invocation:

```bash
# Re-scale a single service without touching the others.
make scale SERVICE=order REPLICAS=4

# Or override the defaults for a `make up` invocation.
make up ORDER_REPLICAS=4 INVENTORY_REPLICAS=3
```

The Makefile passes the `--scale <svc>=<n>` flags to `docker compose up`. The
compose file's `deploy.replicas` is the source of truth for the defaults.

Stateful backing services (PostgreSQL, RabbitMQ) are always single-replica —
see [Non-goals](#non-goals).

### Load balancing verification

Traefik load-balances across replicas round-robin (no session affinity).
Because every replica sets a unique `INSTANCE_ID` and the service returns it
on the `X-Service-Instance` response header, you can verify the balancer is
distributing traffic by reading that header over several requests:

```bash
# Expect two or more distinct instance IDs when catalog is at replicas=2.
for i in $(seq 1 12); do
  curl -fsS -D - http://localhost:18100/api/v1/products?limit=1 -o /dev/null \
    | awk -F': ' 'tolower($1)=="x-service-instance" {gsub("\r","",$2); print $2}'
  sleep 0.2
done | sort -u
```

This is gate 1 of `test/smoke.sh`. `make smoke` runs the same check (and five
more) end to end.

## Checkout flow

The happy-path checkout is a short saga orchestrated by the order service:

1. **Cart** — `POST /api/v1/carts` creates a cart;
   `POST /api/v1/carts/{id}/items` adds a SKU + quantity.
2. **Checkout** — `POST /api/v1/checkouts` (with an `Idempotency-Key` header)
   validates the cart against customer/catalog/inventory and starts the saga.
   The endpoint returns the new `order_id` immediately; the saga runs
   asynchronously.
3. **Reservation** — inventory reserves stock for each line item (status
   `active` → `committed`).
4. **Payment** — payment captures the intent; success produces a
   `payment.captured.v1` event.
5. **Fulfillment** — fulfillment splits the order across warehouses and
   creates shipments; success marks the order `paid` → `fulfilled`.

Failures are compensated: a payment failure publishes
`payment.failed.v1`, which the order consumer catches and which triggers
inventory to release the reservation (`active` → `released`). See gate 5 in
`test/smoke.sh`.

## Event guarantees

Event delivery is **at least once**. The Outbox/Inbox pattern provides the
two guarantees that make this safe:

- **Transactional Outbox** — domain writes insert an `outbox_event` row in
  the same database transaction as their state change. A polling dispatcher
  reads new rows and publishes them to RabbitMQ. This means a service never
  commits a state change without also recording the event that describes it.
- **Idempotent Inbox** — every consumer records an `inbox_event` key before
  applying its mutation. Duplicate deliveries of the same `event_id` are
  detected and skipped. This is what makes at-least-once delivery safe.

Operational consequences:

- Re-delivery is safe. The seed CLI and the smoke test both rely on this —
  the duplicate-webhook gate (gate 4) replays the same `Idempotency-Key`
  twice and asserts the returned payment id is identical.
- Outbox dispatch is the at-least-once boundary. If a pod crashes after
  publishing but before marking a row as dispatched, the row will be
  re-published on restart; the Inbox on the consumer side dedupes.

See `db/migrations/shared.sql` for the Outbox/Inbox schema that every service
migration includes verbatim.

## Failure injection

The seed data deterministically produces a mix of order lifecycles, including
a fraction with `payment_status=failed`. This is the failure injection path:
no manual chaos engineering is required.

```bash
# Discover a seeded order whose payment failed.
curl -fsS http://localhost:18100/api/v1/orders?page_size=100 \
  | jq -r '.items[] | select(.payment_status=="failed") | .order_id' | head -n1

# Inspect its failed payment intent and the failed attempt's failure_code.
ORDER_ID=...  # from the previous command
curl -fsS http://localhost:18100/api/v1/payments/orders/${ORDER_ID} | jq .

# Assert its inventory reservation is NOT active (compensation released it).
curl -fsS http://localhost:18100/api/v1/reservations/${ORDER_ID} | jq .
```

Failure codes observed in the seeded data: `card_declined`,
`insufficient_funds`, `risk_rejection`, `provider_timeout`. The smoke test's
gate 3 asserts a deterministic one is present.

## Broker inspection

RabbitMQ ships with the management UI enabled
(`rabbitmq:4.0-management`). The compose file maps the broker to the internal
`commerce-data` network only — there is no published management port. To
inspect the broker:

```bash
# Open a shell in the rabbitmq container and use rabbitmqctl / rabbitmqadmin.
docker compose exec rabbitmq rabbitmqctl list_queues name messages consumers
docker compose exec rabbitmq rabbitmqctl list_exchanges name type
docker compose exec rabbitmq rabbitmqctl list_bindings source_name routing_key destination_name

# Or bring up the management UI by port-forwarding (one-shot, ad-hoc):
docker compose port rabbitmq 15672   # then docker run -p 15672:15672 ... if needed
```

Topology (matches `deploy/rabbitmq/definitions.json`):

- Topic exchange **`commerce.events`** — the main bus.
- Topic exchange **`commerce.events.dlx`** — dead-letter sink.
- Direct exchange **`commerce.events.retry`** — backs the per-queue retry
  pattern (TTL 2000ms, then dead-letter back to `commerce.events`).
- Per-service queues: `order.saga`, `payment.intents`, `fulfillment.orders`,
  each with a `.retry` (TTL + DLX) and `.dlq` companion.

## Seed tiers

Seed data is deterministic: the same seed value plus scale produces
byte-identical rows. Three tiers are shipped:

| Scale | Orders | Use case |
|-------|-------:|----------|
| `dev` | 100 | default; full lifecycle mix incl. failures; fits the 4–6 GB budget |
| `demo` | 10 000 | demos / integration tests |
| `load` | 1 000 000 | load tests; streams via `COPY FROM` in bounded memory |

```bash
make seed                # dev scale, seed 42 (idempotent with --reset)
make verify-seed         # verify seeded data integrity
SEED=99 make seed        # different seed -> different deterministic dataset

# The load tier uses a separate compose overlay that raises per-replica
# memory and pushes order to 4 replicas and inventory to 3.
make load
```

### Load-tier resource warning: 4–6 GB budget does NOT apply

The default `make up` profile targets a **4–6 GB** memory budget. The load
profile (`make load`, `--scale load`) deliberately raises per-service memory
(order to 2 GiB limit, Postgres shared_buffers to 512 MB, etc.) and runs
1 000 000 orders through `COPY FROM`. **Plan for substantially more than
4–6 GB on the load tier** — see `compose.load.yaml` for the exact overrides.
Do not run the load tier on a laptop that is also running other heavy
workloads.

## Observability

Each Go service exposes:

- `/livez` — liveness probe. Always 200 once the process is up.
- `/readyz` — readiness probe. Returns 503 during shutdown drain, 200
  otherwise. Traefik and the Ingress controller use this to drain.
- `/metrics` — Prometheus-format metrics (request latency, Outbox dispatch
  lag, consumer inbox dedupe counters, pgx pool stats).

Bring up the full observability stack (otel collector, Prometheus, Grafana,
Jaeger) with the overlay:

```bash
make observability         # brings up otel/prometheus/grafana/jaeger
make observability-down    # removes only the observability containers
```

The overlay only adds containers; it does not change the base topology.
Prometheus scrapes each service's `/metrics` via the internal network, and
the otel collector receives traces over OTLP.

## Kubernetes mapping

The same topology runs on Kubernetes. The manifests in `deploy/k8s/` mirror
compose.yaml 1:1 — same replicas, same resource envelopes, same probes, same
env vars, same gateway routing.

```bash
# Render the rendered manifest without applying (Phase A gate):
kubectl kustomize deploy/k8s > /tmp/commerce-k8s.yaml

# Apply the bundle (assumes an ingress controller is installed):
kubectl apply -k deploy/k8s
```

| Compose concept | Kubernetes equivalent | File |
|-----------------|----------------------|------|
| project name `commerce` | Namespace `commerce` | `namespace.yaml` |
| `x-service-env` anchor | ConfigMap `commerce-shared` | `config.yaml` |
| per-service `environment:` | ConfigMap `<svc>-env` (one per service) | `config.yaml` |
| `DB_PASSWORD` / `BROKER_URL` | Secret `commerce-dev-secret` (DEV-ONLY, labeled unsafe) | `config.yaml` |
| service container | Deployment with matching replica count | `apps.yaml` |
| `healthcheck: wget /livez` | startup/readiness/liveness probes on `/livez` and `/readyz` | `apps.yaml` |
| `mem_reservation`/`mem_limit`/`cpus` | resource `requests`/`limits` | `apps.yaml` |
| `read_only: true` + `tmpfs:/tmp` | `readOnlyRootFilesystem: true` + `emptyDir` Memory for `/tmp` | `apps.yaml` |
| `security_opt: no-new-privileges` + `cap_drop: ALL` | runAsNonRoot, seccompProfile, drop ALL caps | `apps.yaml` |
| Traefik router labels | Ingress with matching path rules | `gateway.yaml` |
| per-service DNS name | ClusterIP Service per app | `services.yaml` |
| `*-db` service / `rabbitmq` service | ExternalName Services (override before non-dev) | `services.yaml` |
| `deploy.replicas: N` (N >= 2) | PodDisruptionBudget `minAvailable: 1` | `availability.yaml` |
| (horizontal scale-out) | HorizontalPodAutoscaler per stateless service | `availability.yaml` |

**Stateful backing services are not re-deployed here.** The `*-db` and
`rabbitmq` Services are ExternalName aliases pointing at the default
namespace — replace them with your operator-managed StatefulSet (or a managed
cloud offering) before serving any real traffic. The manifests deliberately
do **not** claim to operate PostgreSQL or RabbitMQ HA.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the component diagram and the
rationale behind each deployment-shape decision.

## Cleanup

```bash
make down                                  # compose: stop + remove + volumes
make observability-down                    # observability overlay only

# Kubernetes (separate cluster, if you applied the manifests):
kubectl delete -k deploy/k8s
```

`make down` passes `--volumes --remove-orphans`, so it wipes all six
PostgreSQL data volumes and the RabbitMQ mnesia volume. Re-running `make up`
after `make down` produces a clean topology.

## Non-goals

This template intentionally does **not** implement any of the following —
they are out of scope and adding them would obscure the patterns the template
exists to demonstrate:

- **Authentication / authorization.** No JWT, OAuth, mTLS, or API keys. All
  routes are open by design.
- **A real payment provider.** The payment service simulates capture/refund
  with deterministic outcomes; there is no Stripe/Adyen/Braintree
  integration.
- **Search.** Catalog listing is a SQL `LIMIT/OFFSET` query, not
  Elasticsearch / OpenSearch / Algolia.
- **Redis.** No cache layer. Inventory reservations live in PostgreSQL with
  `SELECT ... FOR UPDATE` plus a status state machine.
- **A promotion DSL.** No coupon engine, no cart-level discount rules.
- **PostgreSQL HA.** Single-replica per service. No streaming replication,
  no Patroni, no automated failover. (Same on Kubernetes — see above.)
- **RabbitMQ clustering.** Single broker node. No quorum queues across
  multiple nodes, no federation.
- **Cross-cluster federation / multi-region.** Out of scope.

If you need any of these for a real deployment, treat this template as a
starting point and layer them on top — the per-service ownership boundary
makes each addition a local change.
