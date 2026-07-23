# Commerce Production-Shaped Topology Design

**Date:** 2026-07-24  
**Status:** Approved in conversation  
**Scope:** Upgrade the built-in `commerce` template from six read-oriented
services into a production-shaped, locally runnable commerce system.

## Goals

1. Make Docker Compose the default deployment experience while demonstrating
   horizontal scaling, health-aware load balancing, service discovery, reliable
   synchronous calls, and asynchronous workflows.
2. Replace the generic SQL query shell with maintainable Go domain and
   application code for realistic commerce reads and writes.
3. Provide deterministic `dev`, `demo`, and `load` datasets whose entity
   relationships, state distributions, monetary totals, and inventory movements
   remain internally consistent.
4. Keep the default stack usable on a typical development machine with a target
   memory footprint of 4–6 GB.
5. Supply Kubernetes manifests that map the Compose concepts to a production
   orchestrator without making Kubernetes the default local workflow.

## Non-goals

This iteration does not implement customer authentication or authorization,
real payment-provider integrations, a search engine, Redis caching, a complete
promotion rules language, a RabbitMQ cluster, or PostgreSQL high availability.
The documentation must distinguish the production-shaped learning topology from
production-ready infrastructure.

## Chosen Approach

Use Traefik as the sole host-facing API entry point, RabbitMQ for asynchronous
domain events, one PostgreSQL container and persistent volume per service, and
an order-orchestrated saga backed by transactional Outbox and idempotent Inbox
records. Observability infrastructure is optional so the default topology stays
inside the resource budget.

Rejected alternatives:

- A service-mesh and clustered-infrastructure design would demonstrate more
  platform features but exceed the local resource budget and obscure the
  commerce code.
- A shared PostgreSQL container with six logical databases would be lighter but
  retain a shared database failure domain and weaken the production analogy.

## Runtime Topology

Traefik listens on host port `18100`. It routes the following path prefixes:

- `/api/v1/products` to catalog
- `/api/v1/customers` to customer
- `/api/v1/inventory` and `/api/v1/reservations` to inventory
- `/api/v1/carts`, `/api/v1/checkouts`, and `/api/v1/orders` to order
- `/api/v1/payments` and simulated payment webhooks to payment
- `/api/v1/fulfillments` to fulfillment

Application containers do not publish host ports. Traefik discovers replicas
through the Docker provider, performs active health checks, and uses round-robin
load balancing. Responses include `X-Service-Instance` so users can verify
distribution with repeated requests.

The default replica counts are:

| Service | Replicas |
|---|---:|
| catalog | 2 |
| customer | 1 |
| inventory | 2 |
| order | 2 |
| payment | 1 |
| fulfillment | 1 |

`make scale SERVICE=order REPLICAS=4` is the documented interface for changing a
replica count. Compose services must not set `container_name`, because fixed
container names prevent scaling.

Networks have explicit purposes:

- `edge`: Traefik and HTTP application endpoints.
- `service`: application-to-application HTTP and RabbitMQ.
- `data`: each application and its owned PostgreSQL container.

Database containers are not attached to `edge` or exposed on the host by
default. A debugging override may publish database ports, but it is opt-in.

Each application exposes:

- `/livez`, which checks that the process can serve requests without contacting
  dependencies.
- `/readyz`, which checks its database and dependencies required to accept new
  work.
- `/metrics`, which exposes Prometheus-format process, HTTP, client, Outbox, and
  consumer measurements.

Graceful shutdown marks the instance unready, stops accepting new work, waits
for in-flight HTTP requests and consumers up to the configured grace period, and
then closes database and broker connections.

## Communication and Checkout Saga

Immediate validation uses synchronous HTTP:

1. The client submits a checkout to order through Traefik.
2. Order validates the customer and shipping address through customer.
3. Order obtains current SKU and price data from catalog.
4. Order makes an idempotent inventory reservation request.
5. Order commits the pending order, immutable snapshots, saga state, and Outbox
   event in one database transaction.

State propagation uses RabbitMQ topic events:

1. Payment consumes `order.placed.v1`, creates or reuses a payment intent, runs
   the deterministic simulated provider, and publishes a payment result.
2. Order consumes the payment result and advances its saga and order state.
3. Inventory consumes failure or cancellation events and releases reservations.
4. Fulfillment consumes paid-order events and creates warehouse-scoped
   fulfillment work.
5. Fulfillment events update the order's fulfillment projection.

Business failures are compensated in reverse order. A failed payment releases
inventory and cancels the order. A cancellation after capture creates a refund,
releases uncommitted inventory, and cancels open fulfillment work. State
transitions are conditional and idempotent, so duplicate or late events cannot
move an aggregate backward.

All events carry:

- `event_id`
- `event_type`
- `schema_version`
- `aggregate_id`
- `correlation_id`
- `causation_id`
- `occurred_at`
- JSON `payload`

The delivery contract is at least once. Domain updates and Outbox inserts share
a local database transaction. An Outbox relay publishes persistent messages
with publisher confirms and marks rows published only after confirmation.
Consumers use manual acknowledgements and acknowledge only after their local
transaction commits. Each consumer stores `(consumer_name, event_id)` in an
Inbox table with a unique constraint. Transient failures use bounded delayed
retry queues; exhausted and non-retryable messages go to a dead-letter queue.

HTTP clients propagate `X-Request-ID`, W3C `traceparent`, and the idempotency
key. Every call has a total deadline and a shorter per-attempt timeout. GET calls
and writes with an idempotency key may receive a small bounded number of retries
with exponential backoff and jitter. Other writes are never retried
automatically. A per-upstream circuit breaker stops retry storms and recovers
through a half-open probe. The implementation must expose retry and breaker
state through metrics and structured logs.

## Domain Data Model

Every service owns its schema. Identifiers crossing a service boundary are
references, not cross-database foreign keys.

### Catalog

- Category hierarchy with three levels and materialized paths.
- Brands, products, product media, product options, and option values.
- Between one and six variants per product with SKU, barcode, dimensions,
  weight, attributes, and lifecycle status.
- Channel-aware price lists with currency, validity windows, compare-at price,
  and sale price.

### Customer

- Customers with active, disabled, and guest states.
- Multiple shipping addresses and one explicit default address.
- Membership tiers, marketing consent history, and realistic but fictional
  regional contact details.

### Inventory

- Warehouses and stores with region, priority, and fulfillment capability.
- Inventory levels containing `on_hand`, `reserved`, and computed availability.
- Reservations with expiry, state, order reference, and idempotency key.
- Replenishment, sale, return, adjustment, and transfer stock movements.

Reservation creation locks the selected inventory rows and preserves
`on_hand >= reserved >= 0`. Reservation commit, release, and expiry are
conditional state transitions. Movement and reservation quantities must
reconcile with the inventory-level snapshot.

### Order

- Active, converted, abandoned, and expired carts with multiple lines.
- Sales orders with immutable product, price, customer, and address snapshots.
- Per-line and order-level discount allocation, shipping, tax, and integer
  minor-unit totals.
- Status history, saga instances and steps, Outbox, and Inbox.

For every order:

`total_minor = subtotal_minor - discount_minor + shipping_minor + tax_minor`

The order-line totals and allocated discounts must sum to their order totals.

### Payment

- Payment intents, payment method snapshots, one to three attempts, provider
  references, authorization/capture state, partial and full refunds.
- Provider webhook Inbox with unique provider event identifiers.
- Realistic failure codes such as insufficient funds, card declined, provider
  timeout, and risk rejection.

### Fulfillment

- Fulfillment orders split by warehouse.
- Pick items, packages, shipment labels, carrier and tracking numbers.
- Five to twelve tracking events for shipped packages.
- Delivered, delayed, exception, returned, and lost-package examples.

## HTTP API

The template supports both reads and representative writes:

- Create and retrieve a cart.
- Add, change, and remove cart items.
- Submit an idempotent checkout.
- List and retrieve products, customers, inventory, orders, payments, and
  fulfillments.
- Reserve and release inventory through internal endpoints.
- Cancel an order.
- Submit deterministic simulated payment webhooks.

Collection endpoints use cursor pagination with a configurable default and a
hard maximum page size. Mutations require JSON content types and enforce a
request body limit. Errors use `application/problem+json` with stable error
codes. External retryable mutations require `Idempotency-Key`.

The API does not expose service databases or perform cross-database joins.
Order detail responses use local immutable snapshots and local event-driven
projections where appropriate.

## Go Code Structure

The standalone template remains one Go module and avoids a heavyweight
framework:

```text
cmd/<service>/                 process composition
internal/<domain>/             entities, state machines, use cases, ports
internal/platform/httpx/       routing, middleware, problems, shutdown
internal/platform/client/      deadlines, retries, breakers, propagation
internal/platform/postgres/    pgx pools, transactions, migrations
internal/platform/messaging/   Outbox relay, RabbitMQ, Inbox consumers
internal/platform/telemetry/   structured logs, metrics, trace propagation
internal/seed/                 generators, COPY writers, verification
```

Use `pgx/v5` for PostgreSQL and `amqp091-go` for RabbitMQ. Prefer the standard
library HTTP server and Go 1.22 routing patterns. Business packages depend on
interfaces and do not import transport or database packages.

Configuration comes from environment variables and is validated before the
server becomes ready. Database pool sizes account for replica counts. HTTP
servers configure header, read, write, and idle timeouts; request body limits;
panic recovery; structured access logs; and bounded concurrency. SQL encodes
important invariants through constraints and indexes in addition to application
validation.

## Deterministic Data Generation

The seed supports three exact order-count tiers:

| Tier | Products | SKUs | Customers | Orders | Expected order lines |
|---|---:|---:|---:|---:|---:|
| `dev` | 80 | 250–350 | 250 | 100 | 220–320 |
| `demo` | 2,000 | 8,000–12,000 | 25,000 | 10,000 | 25,000–35,000 |
| `load` | 50,000 | 200,000–300,000 | 1,500,000 | 1,000,000 | 2,500,000–3,500,000 |

The same seed, tier, and generator version produce the same identifiers, values,
and base timestamps. All emails and phone numbers are fictional. The generator
uses a curated bilingual vocabulary and regional datasets stored in the
template; it does not depend on network access.

The generator models:

- Zipf-like product popularity and long-tail sales.
- One to eight cart lines, repeat customers, guest carts, and abandoned carts.
- Region-aware warehouse selection.
- Weekday, hour-of-day, and seasonal promotion traffic patterns.
- Roughly 88% successful payments, 5% failed payments, 4% cancellations, and
  3% pending payments.
- Paid orders distributed among unfulfilled, partially fulfilled, delivered,
  and carrier-exception states.
- Multiple attempts for selected failed or transient payment flows.
- Multi-warehouse fulfillment and multiple packages where the order contents
  require them.

Prices originate in catalog price lists. Order amounts, discounts, tax, and
shipping are derived and rechecked, not independently randomized. Inventory
reservations and movements derive from order lines and fulfillment state.

`dev` and `demo` use batched inserts. `load` streams deterministic rows with
PostgreSQL COPY in bounded chunks and does not retain the full dataset in
memory. The million-order tier is opt-in and never runs as part of `make up`.

`seed verify` checks:

- Cross-service identifier references.
- Order-line and order monetary equations.
- Reservation and inventory non-negativity.
- Inventory movement reconciliation.
- Allowed order, payment, and fulfillment state combinations.
- Refund amounts not exceeding captured amounts.
- Event, Inbox, Outbox, webhook, idempotency, and provider-key uniqueness.

## Container and Deployment Artifacts

The container image uses a shared multi-stage Dockerfile, pinned image versions,
a non-root distroless runtime, and one binary selected at build time. Runtime
containers use a read-only root filesystem, `tmpfs` for temporary files,
resource reservations and limits, an init process, health checks, and a stop
grace period. Secrets are read from Compose secret files rather than embedded in
image layers or committed production credentials.

Files are separated by responsibility:

- `compose.yaml`: default topology and health dependencies.
- `compose.observability.yaml`: optional Prometheus, Grafana, OpenTelemetry
  Collector, and trace backend.
- `compose.load.yaml`: resource overrides for the load dataset.
- `deploy/traefik/`: entry point, routing, load-balancer, and health settings.
- `deploy/rabbitmq/`: exchange, queue, delayed-retry, and dead-letter topology.
- `deploy/k8s/`: Deployments, Services, Gateway or Ingress, ConfigMaps, Secret
  examples, PodDisruptionBudgets, HorizontalPodAutoscalers, probes, and resource
  requests and limits.

The Kubernetes manifests preserve the same service names and environment
contract. ClusterIP Services replace Compose DNS and Traefik Docker discovery;
readiness probes govern endpoint membership. Stateful production database and
RabbitMQ operations remain explicitly outside the sample manifests.

## Testing and Acceptance

Implementation follows test-driven development.

Unit tests cover domain state transitions, money allocation, inventory
invariants, event envelope validation, retry eligibility, breaker transitions,
and deterministic seed distributions. HTTP contract tests cover validation,
pagination, idempotency, error documents, and propagation headers. Integration
tests use real PostgreSQL and RabbitMQ containers.

The repository provides:

- `make test` for Go unit and contract tests.
- `make test-integration` for PostgreSQL and RabbitMQ integration tests.
- `make smoke` for the running Compose topology.
- `make verify-seed SCALE=dev|demo|load` for data integrity checks.
- `make scale SERVICE=<name> REPLICAS=<n>` for scaling.

The Compose smoke test must prove:

1. Repeated requests reach multiple healthy replicas.
2. An unhealthy replica is removed from routing.
3. A synchronous timeout is bounded and does not create an unbounded retry
   storm.
4. Duplicate events produce one local state change.
5. A failed payment cancels the order and releases inventory.
6. A successful payment creates fulfillment and advances the order projection.
7. Graceful shutdown completes in-flight work within the configured grace
   period.

CI runs unit tests, API contracts, the `dev` seed, seed verification, and a
Compose smoke path. The heavier `demo` and `load` tiers are manually invoked or
scheduled jobs.

## Documentation

The template README must include quick-start, scale, failure-injection, message
inspection, data-tier, and cleanup commands. Architecture documentation must
show the synchronous checkout path, asynchronous event path, compensation path,
network boundaries, and the Compose-to-Kubernetes mapping. It must state the
non-goals and explain that the topology demonstrates production patterns rather
than providing production operations.

## Research Basis

The design was checked against both English and Chinese official material:

- Google Cloud Online Boutique demonstrates a cloud-first commerce application
  with independently deployed services and Kubernetes deployment artifacts:
  <https://github.com/GoogleCloudPlatform/microservices-demo>
- Docker Compose documents service scaling with `docker compose up --scale` and
  `docker compose scale`:
  <https://docs.docker.com/reference/cli/docker/compose/up/>
- Traefik's Docker provider and HTTP service documentation describe container
  discovery, health-aware routing, and load balancing:
  <https://doc.traefik.io/traefik/reference/install-configuration/providers/overview/>
  and <https://doc.traefik.io/traefik/reference/routing-configuration/http/load-balancing/service/>
- RabbitMQ documents publisher confirms, manual consumer acknowledgements,
  requeueing, and dead lettering:
  <https://www.rabbitmq.com/docs/confirms>
- AWS Prescriptive Guidance documents the transactional Outbox dual-write
  problem and idempotent consumers:
  <https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html>
- AWS Prescriptive Guidance documents saga orchestration for transactions that
  span service-owned databases:
  <https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/saga-orchestration.html>
- Kubernetes Chinese documentation explains stable Services and DNS for dynamic
  Pods, plus readiness-based endpoint removal:
  <https://kubernetes.io/zh-cn/docs/concepts/services-networking/service/>
  and <https://kubernetes.io/zh-cn/docs/concepts/configuration/liveness-readiness-startup-probes/>
- Microsoft Chinese microservices guidance warns that unbounded retries can
  amplify an outage and motivates combining bounded retries with a circuit
  breaker:
  <https://learn.microsoft.com/zh-cn/dotnet/architecture/microservices/implement-resilient-applications/implement-circuit-breaker-pattern>

