# Bank Payment Workflow and Production Topology Upgrade

**Date:** 2026-07-28  
**Status:** Approved in conversation  
**Scope:** Upgrade the built-in `bank` template to the deployment and
operational depth of `commerce`, implement a durable payment Saga workflow,
replace service-to-service HTTP with gRPC and RabbitMQ, and close the internal
route, OpenTelemetry, and CI gaps in `commerce`.

## Goals

1. Give `bank` a production-shaped Compose and Kubernetes topology with
   explicit replica counts, service discovery, health-aware routing, resource
   budgets, availability controls, observability, and load profiles.
2. Implement a durable, restart-safe payment Saga inside the payment service.
   The Saga has a read-only Preparation phase, ordered execution Actions, and
   reverse-order Compensation functions.
3. Keep external APIs RESTful while replacing all service-to-service HTTP in
   `bank` with gRPC for synchronous reads and RabbitMQ for commands and state
   changes.
4. Preserve the core-banking local ACID boundary and double-entry ledger
   invariants while coordinating risk authorization, funds holds, posting, and
   reversal across services.
5. Make the generated template self-contained and locally runnable.
6. Apply the same route-isolation, real OpenTelemetry wiring, and CI quality
   gates to both `bank` and `commerce`.

## Non-goals

- A general-purpose workflow product, visual workflow editor, DAG engine,
  dynamic scripting language, or parallel workflow branches.
- Exactly-once message delivery. The delivery contract is at least once with
  idempotent processing.
- PostgreSQL or RabbitMQ high availability in the default local topology.
- A service mesh, Consul, Nacos, or a separate workflow microservice.
- Real payment-provider, card-network, or regulatory integrations.
- Automatic skipping of failed financial compensations.

## Chosen Architecture

### Communication boundaries

External clients call REST APIs through Traefik. Application containers do not
publish their own REST or gRPC ports to the host.

Synchronous internal reads use gRPC and Protobuf:

```text
external client -> Traefik -> REST :8080

payment Preparation -> customer gRPC :9090
                    -> core-banking gRPC :9090
```

Commands and state changes use RabbitMQ:

```text
payment workflow -> bank.commands -> risk/core-banking
risk/core-banking -> bank.events -> payment workflow
payment.completed.v1 -> bank.events -> reward/other projections
```

Docker DNS supplies replica addresses in Compose. Kubernetes uses separate REST
ClusterIP Services and internal gRPC headless Services. gRPC clients use
`dns:///service-name:9090` with `round_robin`, so a long-lived HTTP/2 connection
does not pin every request to one ClusterIP backend.

### Workflow ownership

The orchestrator lives inside the payment service. Generic mechanics live in
`internal/platform/workflow`, while payment-specific Preparation, Actions, and
Compensations live in `internal/payment/workflows`.

There is no separate workflow service. Risk, core-banking, and reward know only
their commands, domain state, and result events; they do not depend on the
workflow engine.

### Selected workflow approach

The project uses a constrained embedded durable workflow engine. It supports:

- one read-only Preparation phase;
- a linear ordered list of Actions;
- one awaited result event per dispatched command;
- reverse-order Compensation;
- persisted deadlines and bounded retries;
- crash recovery through renewable leases;
- workflow definition versioning;
- explicit manual recovery after compensation exhaustion.

It does not support DAGs, parallel branches, dynamic definitions, or a workflow
DSL.

## Durable Workflow Model

### Lifecycle

```text
preparing -> ready -> running -> succeeded
     |
     +--------------------------> rejected
                         |
                         v
                   compensating -> compensated
                         |
                         v
                compensation_failed
                         |
                    manual retry
                         |
                         v
                   compensating
```

Business rejection before any successful Action terminates without entering a
compensation loop and records `rejected`. A transient Preparation failure
remains `preparing` with a persisted wake-up time. Failure after one or more
successful Actions starts reverse-order Compensation. The terminal states are
`succeeded`, `rejected`, and `compensated`; `compensation_failed` is
non-terminal and requires operator intervention.

### Storage

The payment database owns `workflow_instance`:

- `workflow_id`
- `workflow_type`
- `definition_version`
- `status`
- `input_json`
- `prepared_context_json`
- `current_action`
- `revision`
- `lease_owner`
- `lease_until`
- `next_wakeup_at`
- `last_error_class`
- `last_error`
- creation and update timestamps

It also owns `workflow_action`:

- unique `(workflow_id, action_index)`
- `action_name`
- `status`
- `direction` (`execute` or `compensate`)
- `attempt`
- `idempotency_key`
- `command_id`
- `result_event_id`
- `deadline_at`
- `output_json`
- `last_error_class`
- `last_error`
- creation and update timestamps

The existing message reliability schema is extended to every service:

- Transactional Outbox for domain changes and outgoing messages.
- Transactional Inbox keyed by consumer and message ID.
- Claim token and lease fields for safe multi-replica Outbox dispatch.

### Preparation

Preparation accepts the external payment input and performs only read-only gRPC
calls. It loads:

- payer customer state, KYC state, and risk-relevant customer tags;
- payer and payee account state, ownership, currency, and audit balance
  snapshots;
- payment amount, business type, and client idempotency key.

It validates that accounts differ, currencies match, the amount is positive,
and customer and account states permit a payment.

The balance read during Preparation is an audit and risk snapshot, not an
authorization. The funds-hold Action performs the authoritative balance check
under a core-banking database lock.

Because Preparation has no cross-service writes, it is safe to rerun after a
crash. Its completed context is persisted as one immutable JSON snapshot.
Actions may append their own outputs but may not rewrite the prepared payment
amount, payer, payee, or currency.

### Atomic workflow advancement

Dispatching an Action happens in one payment database transaction:

1. Lock the workflow instance.
2. Validate the expected `revision` and Action state.
3. Mark the Action `waiting_result`.
4. Insert its command into the Outbox.
5. Advance the instance state and wake-up deadline.
6. Commit.

The broker publisher never changes workflow state. It only claims and publishes
Outbox rows and marks them dispatched after Publisher Confirm succeeds.

Receiving a result event also happens in one transaction:

1. Insert the Inbox key or detect a duplicate.
2. Lock the workflow instance.
3. Match workflow ID, Action, command ID, allowed event type, and revision.
4. Record the result and Action output.
5. Complete the Action or classify its failure.
6. Insert the next command or first compensation into the Outbox.
7. Commit before acknowledging the broker delivery.

This prevents two payment replicas from advancing the same instance.

### Leases and restart recovery

Workers claim runnable instances using database leases. A worker renews its
lease while performing Preparation or scheduling work. If it crashes, another
replica can claim the instance after `lease_until`.

Waiting for a command result does not require a worker lease. A scheduler scans
persisted `next_wakeup_at` deadlines. A timeout is an unknown outcome rather
than proof that the downstream action failed. Re-dispatch creates a new
transport `message_id` while preserving the original `command_id` and
`idempotency_key`.

Each instance stores its workflow definition version. The payment process must
fail startup if the database contains a non-terminal instance for which no
matching `(workflow_type, definition_version)` is registered.

## Workflow API

The platform API is domain-neutral:

```go
type Definition interface {
	Type() string
	Version() int
	Prepare(context.Context, json.RawMessage) (json.RawMessage, error)
	Actions() []Action
}

type Action interface {
	Name() string
	Execute(context.Context, View) (Dispatch, error)
	ApplyResult(context.Context, View, Envelope) (Outcome, error)
	Compensate(context.Context, View) (Dispatch, error)
	ApplyCompensationResult(
		context.Context,
		View,
		Envelope,
	) (Outcome, error)
}
```

`Dispatch` contains the routing key, command payload, accepted result event
types, deadline, and idempotency key. An Action cannot publish directly or
modify workflow tables.

Payment workflow code is organized as:

```text
internal/payment/workflows/
  transfer.go
  prepare.go
  risk_authorize.go
  funds_hold.go
  ledger_transfer.go
```

Generic code is organized as:

```text
internal/platform/workflow/
  definition.go
  engine.go
  store.go
  recovery.go
  timeout.go
```

## Payment Action Chain

### Action 1: AuthorizeRisk

Execution command and results:

```text
risk.authorize-payment.v1
  -> risk.payment-authorized.v1
  -> risk.payment-rejected.v1
```

Risk stores the authorization decision, matched rules, and a digest of the
prepared context.

Compensation:

```text
risk.void-payment-authorization.v1
  -> risk.payment-authorization-voided.v1
```

A rejected authorization never enters the successful Action stack.

### Action 2: PlaceFundsHold

Execution command and results:

```text
core.place-funds-hold.v1
  -> core.funds-held.v1
  -> core.funds-hold-failed.v1
```

Core-banking locks the account, performs the authoritative balance check, and
creates a hold in one transaction:

```text
available_balance = ledger_balance - active_holds
available_balance >= 0
```

Compensation:

```text
core.release-funds-hold.v1
  -> core.funds-hold-released.v1
```

Release is idempotent by hold ID and workflow Action idempotency key.

### Action 3: PostLedgerTransfer

Execution command and result:

```text
core.post-held-transfer.v1
  -> core.transfer-posted.v1
```

Core-banking atomically:

1. locks and consumes the active hold;
2. creates payer debit and payee credit entries;
3. verifies double-entry balance;
4. updates authoritative balances;
5. inserts the result event into its Outbox.

Compensation:

```text
core.reverse-transfer.v1
  -> core.transfer-reversed.v1
```

Reversal uses immutable red entries. It never deletes or mutates the original
voucher. Because posting is the final critical Action, a non-critical reward or
notification failure cannot reverse a valid transfer. The posting Compensation
is used by an explicit cancellation/reversal workflow or authorized manual
rollback. A completed transfer workflow is never reopened. A separate
`payment-reversal` workflow references the completed payment and invokes the
original posting Action's Compensation contract.

### Completion and non-critical consumers

After the third Action succeeds, payment marks the workflow and payment record
complete in one transaction and emits `payment.completed.v1`.

Reward, reporting, and notification consumers process that event independently.
They use Inbox, bounded retry, and DLQ, but their failure cannot compensate a
completed payment.

## Message Contract and Topology

Every command and event carries:

```text
message_id
message_type
schema_version
workflow_id
action_name
command_id
idempotency_key
correlation_id
causation_id
occurred_at
payload
```

RabbitMQ topology:

```text
bank.commands  topic exchange
bank.events    topic exchange
bank.retry     direct exchange
bank.dlx       topic exchange

risk.commands
core-banking.commands
payment.workflow.events
reward.payment-events
```

Each command queue has a retry queue and DLQ. Topology declarations exist both
in a definitions file and as idempotent application startup declarations.

Every command handler commits Inbox dedupe, domain mutation, and result Outbox
in one local database transaction before acknowledging delivery.

## gRPC and Protobuf

The first version exposes only the read contracts required by Preparation:

```text
proto/bank/customer/v1/customer_query.proto
proto/bank/core/v1/account_query.proto
```

Services:

```protobuf
service CustomerQueryService {
  rpc GetCustomer(GetCustomerRequest) returns (CustomerSnapshot);
}

service AccountQueryService {
  rpc GetAccount(GetAccountRequest) returns (AccountSnapshot);
}
```

Generated Go files are committed so generated projects build without installing
`protoc`. `make proto` regenerates them, and CI verifies a clean diff.

All services use internal container ports:

- `8080` for externally routed REST;
- `9090` for internal gRPC.

No gRPC port is published to the host or routed through Ingress.
Every gRPC server implements the standard gRPC health service. Client-side
round-robin health checking excludes unavailable replicas.

## Error Semantics and Retry Policy

Errors have explicit classes:

```text
business_rejected
transient_failure
unknown_outcome
invariant_violation
invalid_message
```

- `business_rejected`: do not retry; compensate completed Actions.
- `transient_failure`: bounded retry with exponential backoff and jitter.
- `unknown_outcome`: remain waiting and re-dispatch with the same semantic
  idempotency key; never assume the remote operation did not happen.
- `invariant_violation`: stop automatic progress and alert; never blindly retry
  a ledger invariant failure.
- `invalid_message`: route to DLQ without changing workflow state.

Configurable defaults:

- Preparation gRPC attempt: 2 seconds.
- Preparation total budget: 5 seconds.
- Read-only gRPC retry count: 2.
- Action result check: 15 seconds.
- Execute transient retry count: 3.
- Compensation retry count: 5.
- Workflow lease: 30 seconds with renewal.
- Operational workflow deadline: 2 minutes.

An operational deadline does not delete or abandon an instance. It makes the
instance observable and eligible for operator intervention.

If Compensation exhausts retries, the instance enters
`compensation_failed`. The remaining compensation stack is preserved, the
message is visible in DLQ, and a protected operator API can retry the failed
Compensation. The operator API is an internal payment gRPC service, is not
routed through Ingress, is restricted by NetworkPolicy, and requires an
operator credential supplied through Secret configuration. The dev credential
is explicitly labeled unsafe; the production overlay delegates identity to the
deployment platform.

An operator may record an externally completed reconciliation only by supplying
an immutable reconciliation reference and passing a current-state validation.
This records how the Compensation was completed; it does not skip a financial
Compensation. Funds-hold and ledger-transfer Compensations can never be marked
resolved without this validation.

## Bank Deployment Topology

Default application replicas:

| Service | Replicas |
|---|---:|
| core-banking | 2 |
| customer | 1 |
| payment | 2 |
| risk | 2 |
| reward | 1 |
| loan | 1 |
| wealth | 1 |

Each service gets its own PostgreSQL container and volume. RabbitMQ is a single
local node. Traefik is the sole published entry point.

The default topology targets a 6-8 GB local memory budget. The load profile has
separate resource settings and does not claim to fit that budget.

Networks have explicit purposes:

```text
edge       gateway to public REST services
grpc       internal gRPC
message    applications to RabbitMQ
data-*     one application to its owned PostgreSQL
obs        telemetry components
```

Application containers use a read-only root filesystem, memory-backed `/tmp`,
dropped capabilities, `no-new-privileges`, resource limits, and graceful
shutdown.

### Kubernetes

The base manifests provide:

- stateless Deployments;
- REST ClusterIP Services;
- internal gRPC headless Services;
- startup, readiness, and liveness probes;
- topology spread and preferred anti-affinity;
- PDBs for multi-replica services;
- HPAs with declared metrics dependencies;
- a REST-only Ingress/Gateway;
- resource requests and limits;
- dev-only credentials explicitly labeled unsafe.

`overlays/dev` contains runnable single-node PostgreSQL and RabbitMQ resources.
`overlays/prod` references external stateful services and external Secret
management. Neither overlay claims to implement stateful HA.

## Internal Route Isolation

Route isolation is a shared bank and commerce quality gate.

For bank:

- Traefik and Ingress use an explicit public REST allowlist.
- Internal calls use gRPC only; there are no `/internal/v1` HTTP routes.
- gRPC Services are not Ingress backends.
- gRPC ports are not published to the host.
- NetworkPolicy permits only documented caller-to-service relationships.

For commerce:

- Remove every `/internal/v1/...` rule from Traefik and Kubernetes Ingress.
- Existing internal HTTP calls use application Service DNS directly.
- Gateway probes for internal routes must return 404.

## Observability

Both templates use the same self-contained platform API, copied into each
generated template rather than importing one template from the other:

```text
internal/platform/telemetry/
  config.go
  provider.go
  propagation.go
  logging.go
  metrics.go
```

Shared behavior:

- OpenTelemetry SDK initialization and shutdown.
- OTLP trace export.
- W3C trace context and baggage propagation.
- RabbitMQ trace header injection and extraction.
- JSON logs containing trace and span IDs.
- Collector export to Jaeger rather than debug-only trace output.
- Prometheus scraping and provisioned Grafana datasources and dashboards.

Commerce instruments HTTP server/client, Outbox/Inbox, RabbitMQ, and checkout
Saga paths.

Bank additionally instruments gRPC server/client, Preparation, every workflow
Action and Compensation, funds holds, ledger posting, and reversal.

Bank metrics include:

```text
workflow_instances
workflow_action_duration_seconds
workflow_action_attempts_total
workflow_compensation_total
workflow_compensation_failures_total
workflow_waiting_age_seconds
outbox_oldest_age_seconds
inbox_duplicates_total
grpc_client_duration_seconds
rabbitmq_consumer_lag
ledger_posting_total
ledger_invariant_failures_total
```

Grafana provides at least workflow, message reliability, and ledger dashboards,
plus baseline alerts for stuck workflows, compensation failure, old Outbox
rows, queue backlog, and ledger invariant failure.

## CI and Verification

### Static and unit jobs

Both template jobs run:

- `go build ./...`
- `go test ./...`
- race tests for concurrency-sensitive platform packages
- Compose config validation
- shell syntax validation
- `kubectl kustomize` rendering
- generated Protobuf diff checks where applicable
- embedded `templates.tar` freshness checks

### Commerce end-to-end

The Commerce CI topology retains at least two catalog replicas and runs the
existing smoke gates. On failure it captures Compose logs and always removes
containers and volumes.

It additionally proves:

- Gateway `/internal/v1/...` requests return 404.
- A checkout trace spans HTTP, RabbitMQ, and consumers and reaches Jaeger.

### Bank end-to-end

The Bank CI topology proves:

1. A successful payment creates one workflow, one hold, one balanced voucher,
   and one completion event.
2. Risk rejection creates no hold and no ledger entries.
3. Insufficient funds voids risk authorization and creates no ledger entries.
4. Transfer failure releases the hold and voids risk authorization.
5. Duplicate commands and events create only one domain mutation and voucher.
6. Killing payment while an Action waits allows another replica to resume it.
7. A completed payment reversal creates immutable red entries.
8. A transient Compensation failure recovers through retry.
9. An exhausted Compensation enters `compensation_failed`, DLQ, and alert
   surfaces.
10. Two payment replicas receiving the same result advance only once.
11. Gateway cannot reach internal gRPC or internal routes.
12. gRPC DNS reaches multiple healthy replicas.
13. A trace crosses REST, Preparation gRPC, RabbitMQ, Action handling, and
    Compensation.

## Implementation Sequence

The work proceeds through five independently verifiable checkpoints:

1. **Commerce baseline:** isolate internal routes, wire real OpenTelemetry, and
   add complete CI coverage.
2. **Bank chassis:** add dedicated state containers, RabbitMQ, Traefik,
   networks, replicas, probes, resource limits, Protobuf/gRPC, and Kubernetes
   foundations.
3. **Durable engine:** implement the persisted workflow state machine, leases,
   Outbox/Inbox, timeouts, version registration, and reverse Compensation using
   tests first.
4. **Payment integration:** implement risk authorization, core funds holds,
   held transfer posting, red reversal, and the three payment Actions.
5. **Operational closure:** add failure injection, end-to-end gates, dashboards,
   alerts, documentation, load/scaling verification, and regenerate the
   embedded template archive.

Every checkpoint must leave the repository buildable and its relevant tests
passing. The implementation plan will give each checkpoint its own test and
review gates.
