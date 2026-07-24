# Commerce Task 6 Remediation Implementation Plan

**Goal:** Make order checkout, cancellation, saga consumption, retry routing, and API idempotency durable under crashes and concurrency while preserving the seeded table arities.

**Architecture:** PostgreSQL owns every command claim and saga transition. Checkout uses a durable phase record created before any dependency call and stores immutable prepared input/output snapshots before inventory reservation. Cancellation derives its actions only after locking the order in the same transaction that records idempotency, state, history, saga steps, and outbox events. Consumer transitions validate typed event payloads and advance only on explicit result events. RabbitMQ retry/dead-letter routing uses a dedicated confirmed publisher channel.

**Tech Stack:** Go 1.22, pgx v5, RabbitMQ/amqp091-go, PostgreSQL, `net/http`.

---

## 1. Persistence and domain model

**Files:**
- Modify: `templates/commerce/db/migrations/order_db.sql`
- Modify: `templates/commerce/internal/order/service.go`
- Modify: `templates/commerce/internal/order/money.go`
- Test: `templates/commerce/internal/order/money_test.go`
- Test: `templates/commerce/internal/order/integration_test.go`

1. Add failing reconciliation and largest-remainder allocation tests.
2. Add companion tables for checkout phases/prepared JSON, product snapshots, inventory allocations, tax lines, payment/refund projection, and generic command idempotency. Do not alter the positional columns of `cart`, `sales_order`, or `order_item`.
3. Extend order/catalog/reservation types with product/variant snapshot fields and allocation status.
4. Implement checked integer percentage allocation, regional tax computation, and exact reconciliation.
5. Run focused money tests and migration-backed snapshot tests.

## 2. Catalog lifecycle-aware pricing

**Files:**
- Modify: `templates/commerce/internal/catalog/store.go`
- Modify: `templates/commerce/internal/catalog/service.go`
- Modify: `templates/commerce/internal/catalog/http.go`
- Test: `templates/commerce/internal/catalog/store_integration_test.go`
- Test: `templates/commerce/internal/catalog/http_test.go`

1. Add failing tests for inactive product/variant, wrong channel/currency, and expired/future price lists.
2. Query an active `variant_detail` and a current active web price list/variant price.
3. Fall back to legacy variant price only when no richer lifecycle/price rows exist at all for that SKU.
4. Return product ID/title, variant title/attributes/weight, channel, and currency in the checkout snapshot.
5. Correct the integration fixture to insert an active product and run catalog tests.

## 3. Durable checkout re-entry

**Files:**
- Modify: `templates/commerce/internal/order/service.go`
- Modify: `templates/commerce/internal/order/store.go`
- Modify: `templates/commerce/internal/order/clients.go`
- Test: `templates/commerce/internal/order/service_test.go`
- Test: `templates/commerce/internal/order/integration_test.go`

1. Add failing tests for concurrent same-key claims, changed fingerprint conflicts before inventory, replay without rematerializing prices, terminal reservation rejection, cart CAS compensation, and release failure.
2. Replace lookup-then-side-effect with `ClaimCheckout`, which atomically inserts/locks the idempotency record before dependency calls.
3. Build the fingerprint from canonical request fields plus locked cart identity, revision, normalized lines, customer, address, currency, and coupon.
4. Persist immutable prepared customer/catalog/order/reservation snapshots and totals before reserve; retries reload them.
5. Decode reservation results and require matching active allocations.
6. Persist phase transitions (`claimed`, `prepared`, `reserved`, `committed`, `compensation_needed`, `failed`) with deterministic order/reservation identities.
7. Commit order/cart/outbox with cart CAS; on conflict persist compensation state and release, mapping release failures to an upstream/compensation error.
8. Run focused unit tests, race tests, and PostgreSQL concurrent checkout/cart-vs-checkout tests.

## 4. Transactional cancellation and result-driven saga

**Files:**
- Modify: `templates/commerce/internal/order/service.go`
- Modify: `templates/commerce/internal/order/store.go`
- Modify: `templates/commerce/db/migrations/order_db.sql`
- Modify: `templates/commerce/cmd/order/main.go`
- Test: `templates/commerce/internal/order/service_test.go`
- Test: `templates/commerce/internal/order/integration_test.go`

1. Add failing tests for cancellation key replay/mismatch, cancel-vs-event races, payload mismatch, amount/currency mismatch, partial refund accumulation, and result-event sequencing.
2. Move all cancellation validation and event derivation into `CancelOrder` after `FOR UPDATE`; check every conditional update’s `RowsAffected`.
3. Persist cancellation fingerprint/result in command idempotency within that transaction.
4. Validate strict typed payloads (`order_id`, currency, amount) against the locked order and payment/refund projection; classify malformed/mismatched events as non-retryable.
5. Payment capture records the captured amount and requests inventory commit without completing the saga.
6. Inventory committed confirms/marks paid and completes the checkout saga; inventory released completes compensation only after required refund/cancel steps.
7. For captured cancellation, request fulfillment cancellation first; its success requests only the remaining refund; refund completion then requests inventory release.
8. Bind all required inventory, refund, and fulfillment result event types.
9. Run state-machine and PostgreSQL race/rollback tests.

## 5. Confirmed raw-delivery routing

**Files:**
- Modify: `templates/commerce/internal/platform/messaging/rabbitmq.go`
- Modify: `templates/commerce/internal/platform/messaging/rabbitmq_test.go`
- Modify: `templates/commerce/internal/order/consumer.go`
- Modify: `templates/commerce/cmd/order/main.go`
- Test: `templates/commerce/internal/order/service_test.go`
- Test: `templates/commerce/internal/platform/messaging/integration_test.go`

1. Add failing raw publish tests for mandatory return, nack, channel closure, cancellation, and sequence mismatch.
2. Extract a reusable confirmed raw publisher that correlates sequence, return, publish confirmation, and close notification; retire on ambiguous outcomes.
3. Make retry/DLQ publication use a dedicated confirmed channel and acknowledge the original only after positive confirmation.
4. Count `x-death` only for the configured retry queue and intended expiration reason.
5. Separate consume and retry-publish channel lifecycle and include both in readiness/shutdown.
6. Add `TEST_AMQP_URL` integration coverage for confirmed retry and dead-letter routing.

## 6. API command idempotency and dependency readiness

**Files:**
- Modify: `templates/commerce/internal/order/http.go`
- Modify: `templates/commerce/internal/order/service.go`
- Modify: `templates/commerce/internal/order/store.go`
- Modify: `templates/commerce/internal/order/clients.go`
- Modify: `templates/commerce/cmd/order/main.go`
- Test: `templates/commerce/internal/order/http_test.go`
- Test: `templates/commerce/internal/order/integration_test.go`

1. Add failing tests requiring `Idempotency-Key` on cart creation/item mutation/cancel, exact response replay, and fingerprint mismatch conflicts.
2. Route create/mutate/cancel through the generic command-idempotency table in the same transaction as their mutation.
3. Include the idempotency key in service commands and set replay markers/status codes consistently.
4. Add bounded `/readyz` probes to customer, catalog, and inventory clients using the resilient client APIs.
5. Compose database, broker, worker, and dependency checks in runtime readiness.

## 7. Integration verification and handoff

**Files:**
- Modify: `templates/commerce/internal/order/integration_test.go`
- Modify: `templates/commerce/internal/platform/messaging/integration_test.go`
- Modify: `.superpowers/sdd/commerce-task-6-report.md`

1. Remove source-string assertions and replace them with behavior tests.
2. Add gated PostgreSQL tests for same-key concurrency, cart-vs-checkout, cancel-vs-event, idempotent replays, and rollback/no-outbox-on-failure.
3. Add gated RabbitMQ confirmed retry/DLQ tests.
4. Run:
   - focused order/catalog/messaging tests,
   - `go test -race` on changed packages,
   - Go 1.22 focused and full tests,
   - PostgreSQL/RabbitMQ integration tests when their URLs are configured,
   - full template test suite.
5. Append red/green evidence and verification output to the Task 6 report.
6. Review the diff for unrelated workspace changes, commit only Task 6 remediation files, and report the commit hash.
