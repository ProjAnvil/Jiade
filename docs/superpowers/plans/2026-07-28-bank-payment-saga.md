# Bank Payment Saga Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the risk authorization, funds hold, held ledger transfer, red reversal, and payment workflow Actions that form the durable payment Saga.

**Architecture:** Payment prepares immutable context via customer/core gRPC, then the workflow engine dispatches risk and core commands through RabbitMQ. Each command handler commits Inbox, domain mutation, and result Outbox atomically; failures trigger reverse compensation.

**Tech Stack:** Go 1.22, `database/sql` with the pgx stdlib driver, gRPC, RabbitMQ, durable workflow engine, PostgreSQL.

## Global Constraints

- Preparation is read-only and stores an immutable context snapshot.
- Authoritative balance checking occurs under a core-banking lock during funds hold.
- Ledger posting remains one local core-banking ACID transaction with balanced entries.
- Red reversal appends immutable entries and never edits the original voucher.
- Reward is a non-critical consumer of `payment.completed.v1` and cannot reverse money.
- Every command handler is idempotent by Inbox and domain idempotency key.
- Every task ends with focused tests and a commit.

---

## File Map

- `templates/bank/internal/risk`: authorization domain, repository, command handler.
- `templates/bank/internal/corebanking`: funds hold, held posting, and red reversal.
- `templates/bank/internal/payment/workflows`: Preparation and three Actions.
- `templates/bank/internal/payment/api`: public payment workflow endpoints.
- `templates/bank/cmd/payment`, `cmd/risk`, `cmd/core-banking`: runtime consumers and relay workers.
- `templates/bank/db/migrations/*.sql`: authorization, hold, payment, workflow, and idempotency tables.

### Task 1: Implement Risk Authorization Commands

**Files:**
- Modify: `templates/bank/db/migrations/risk_db.sql`
- Create: `templates/bank/internal/risk/domain/authorization.go`
- Create: `templates/bank/internal/risk/domain/authorization_test.go`
- Create: `templates/bank/internal/risk/repo/authorization.go`
- Create: `templates/bank/internal/risk/repo/authorization_integration_test.go`
- Create: `templates/bank/internal/risk/service/authorization.go`
- Create: `templates/bank/internal/risk/service/authorization_test.go`
- Create: `templates/bank/internal/risk/consumer.go`
- Create: `templates/bank/internal/risk/consumer_test.go`

**Interfaces:**
- Consumes: `risk.authorize-payment.v1` and `risk.void-payment-authorization.v1`.
- Produces: authorized, rejected, and authorization-voided result events.

- [ ] **Step 1: Write domain transition tests**

Assert:

```go
authorization.Authorize(decisionApproved) // pending -> authorized
authorization.Void()                      // authorized -> voided
authorization.Void()                      // voided -> voided, idempotent
```

Reject `voided -> authorized` and preserve matched rule IDs and context digest.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/risk/...
```

Expected: compile failure for authorization types.

- [ ] **Step 3: Add schema and service**

Create `payment_authorization` keyed by `authorization_id`, unique
`idempotency_key`, with workflow ID, customer ID, amount, currency, decision,
matched rules JSON, context digest, status, and timestamps.

The deterministic template policy rejects inactive KYC, blacklisted customers,
non-positive amounts, and seeded high-risk tags; it records the rule IDs.

- [ ] **Step 4: Implement transactional command consumer**

The consumer decodes the envelope, inserts Inbox, calls the service, and writes
the result event Outbox in one risk transaction. Duplicate commands return
without a second authorization or event.

- [ ] **Step 5: Run unit and integration tests**

Run:

```bash
cd templates/bank
go test ./internal/risk/...
go test -tags=integration ./internal/risk/...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/bank/db/migrations/risk_db.sql templates/bank/internal/risk
git commit -m "feat(bank): authorize and void payment risk"
```

### Task 2: Implement Core Funds Holds

**Files:**
- Modify: `templates/bank/db/migrations/core_db.sql`
- Create: `templates/bank/internal/corebanking/domain/hold.go`
- Create: `templates/bank/internal/corebanking/domain/hold_test.go`
- Create: `templates/bank/internal/corebanking/repo/hold_repo.go`
- Create: `templates/bank/internal/corebanking/repo/hold_repo_integration_test.go`
- Create: `templates/bank/internal/corebanking/service/hold_service.go`
- Create: `templates/bank/internal/corebanking/service/hold_service_test.go`

**Interfaces:**
- Produces: `PlaceHold(context.Context, PlaceHold) (Hold, error)`.
- Produces: `ReleaseHold(context.Context, string, string) (Hold, error)`.
- Maintains: `available = ledger balance - active holds`.

- [ ] **Step 1: Write funds-hold invariants**

Test:

```go
func TestPlaceHoldRejectsInsufficientAvailableBalance(t *testing.T)
func TestDuplicatePlaceHoldReturnsExistingHold(t *testing.T)
func TestReleaseIsIdempotent(t *testing.T)
func TestCapturedHoldCannotBeReleased(t *testing.T)
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/corebanking/domain ./internal/corebanking/service
```

Expected: compile failure.

- [ ] **Step 3: Add hold schema**

Create `funds_hold` with status `active`, `released`, or `captured`; unique
idempotency key; account, amount, currency, workflow ID, expiry, and timestamps.
Index active holds by account.

- [ ] **Step 4: Implement transactional repository**

Lock the current balance row and active holds for the account. Compute available
minor units using integers. Insert or return the unique idempotent hold.

- [ ] **Step 5: Run unit and integration tests**

Run:

```bash
cd templates/bank
go test ./internal/corebanking/domain ./internal/corebanking/service
go test -tags=integration ./internal/corebanking/repo
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/bank/db/migrations/core_db.sql templates/bank/internal/corebanking
git commit -m "feat(bank): reserve and release available funds"
```

### Task 3: Post Held Transfers and Red Reversals

**Files:**
- Modify: `templates/bank/internal/corebanking/service/posting.go`
- Modify: `templates/bank/internal/corebanking/service/posting_test.go`
- Modify: `templates/bank/internal/corebanking/service/txn_service.go`
- Modify: `templates/bank/internal/corebanking/service/txn_service_test.go`
- Create: `templates/bank/internal/corebanking/service/held_transfer.go`
- Create: `templates/bank/internal/corebanking/service/held_transfer_test.go`
- Modify: `templates/bank/internal/corebanking/repo/ledger_repo.go`
- Modify: `templates/bank/internal/corebanking/repo/integration_test.go`

**Interfaces:**
- Produces: `PostHeldTransfer(context.Context, PostHeldTransfer) (Voucher, error)`.
- Produces: `ReverseTransfer(context.Context, ReverseTransfer) (Voucher, error)`.

- [ ] **Step 1: Write failing posting tests**

Assert one transaction:

- locks the active hold;
- creates exactly two entries;
- debit equals credit;
- captures the hold;
- returns the same voucher for duplicate idempotency key;
- rolls back every change when Outbox insertion fails.

For reversal, assert immutable opposite entries and a unique reference to the
original voucher.

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/corebanking/service
go test -tags=integration ./internal/corebanking/repo
```

Expected: failed assertions until held posting is implemented.

- [ ] **Step 3: Implement held posting**

Call `BuildEntries` and `LedgerService.Post` through repositories bound to the
same `pg.DBTX`. The orchestration sequence in that SQL transaction is:

```text
SELECT hold FOR UPDATE
validate active hold and exact amount/currency
insert debit and credit entries
update balances
mark hold captured
insert core.transfer-posted.v1 Outbox
commit
```

- [ ] **Step 4: Implement red reversal**

Create a new reversal voucher whose two entries invert the original debit and
credit. The original voucher status and entries remain unchanged. A unique
`reverses_voucher_no` prevents duplicate reversal.

- [ ] **Step 5: Run tests and commit**

Run:

```bash
cd templates/bank
go test ./internal/corebanking/...
go test -tags=integration ./internal/corebanking/...
```

Then:

```bash
git add templates/bank/internal/corebanking
git commit -m "feat(bank): post and reverse held transfers"
```

### Task 4: Add Core Command Consumer

**Files:**
- Create: `templates/bank/internal/corebanking/consumer.go`
- Create: `templates/bank/internal/corebanking/consumer_test.go`
- Modify: `templates/bank/cmd/core-banking/main.go`

**Interfaces:**
- Consumes: place hold, release hold, post held transfer, and reverse transfer commands.
- Produces: held, hold-failed, released, transfer-posted, and transfer-reversed events.

- [ ] **Step 1: Write routing and idempotency tests**

Use table tests mapping exact command types to service methods. Deliver each
message twice and assert one domain mutation and one result Outbox row.

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/corebanking -run Consumer
```

Expected: compile failure.

- [ ] **Step 3: Implement transactional routing**

Unknown schema or command types return `invalid_message`. Domain invariant
failures return `invariant_violation`; insufficient funds returns
`business_rejected`; database/broker failures return `transient_failure`.

- [ ] **Step 4: Compose consumer lifecycle**

`cmd/core-banking` starts HTTP, gRPC, Outbox relay, and command consumer under
one cancellation context. Readiness fails if the database, broker, relay, or
consumer stops unexpectedly. Shutdown waits for all workers.

- [ ] **Step 5: Run tests and commit**

```bash
cd templates/bank
go test ./internal/corebanking ./cmd/core-banking
git add templates/bank/internal/corebanking templates/bank/cmd/core-banking/main.go
git commit -m "feat(bank): process core payment commands"
```

### Task 5: Implement Payment Preparation

**Files:**
- Create: `templates/bank/internal/payment/workflows/context.go`
- Create: `templates/bank/internal/payment/workflows/prepare.go`
- Create: `templates/bank/internal/payment/workflows/prepare_test.go`
- Modify: `templates/bank/internal/platform/serviceclient/customer.go`
- Modify: `templates/bank/internal/platform/serviceclient/account.go`

**Interfaces:**
- Produces: immutable `TransferContext`.
- Consumes: customer and account gRPC query clients.

- [ ] **Step 1: Write Preparation tests**

Test parallel customer/account reads and reject:

- same payer/payee account;
- non-positive amount;
- mismatched currency;
- inactive customer/KYC;
- closed or frozen account.

Assert the balance snapshot is stored but does not authorize funds.

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/payment/workflows -run Prepare
```

Expected: compile failure.

- [ ] **Step 3: Implement exact context**

```go
type TransferContext struct {
	PaymentID             string `json:"payment_id"`
	PayerCustomerID       string `json:"payer_customer_id"`
	PayerAccountNo        string `json:"payer_account_no"`
	PayeeAccountNo        string `json:"payee_account_no"`
	Currency              string `json:"currency"`
	AmountMinor           int64  `json:"amount_minor"`
	PayerLedgerSnapshot   int64  `json:"payer_ledger_snapshot"`
	PayerAvailableSnapshot int64 `json:"payer_available_snapshot"`
	CustomerKYC           string `json:"customer_kyc"`
	ContextDigest         string `json:"context_digest"`
}
```

Use `errgroup.WithContext` for independent reads and SHA-256 over canonical
prepared JSON for `ContextDigest`. Wrap the whole Preparation in a five-second
deadline. Each gRPC attempt has a two-second deadline; read-only `Unavailable`
and `ResourceExhausted` responses receive at most two attempts with exponential
backoff and jitter. `InvalidArgument`, `NotFound`, and business-state
rejections are not retried.

- [ ] **Step 4: Run tests and commit**

```bash
cd templates/bank
go test ./internal/payment/workflows -run Prepare
git add templates/bank/internal/payment/workflows templates/bank/internal/platform/serviceclient
git commit -m "feat(bank): prepare immutable payment workflow context"
```

### Task 6: Implement the Three Workflow Actions

**Files:**
- Create: `templates/bank/internal/payment/workflows/risk_authorize.go`
- Create: `templates/bank/internal/payment/workflows/risk_authorize_test.go`
- Create: `templates/bank/internal/payment/workflows/funds_hold.go`
- Create: `templates/bank/internal/payment/workflows/funds_hold_test.go`
- Create: `templates/bank/internal/payment/workflows/ledger_transfer.go`
- Create: `templates/bank/internal/payment/workflows/ledger_transfer_test.go`
- Create: `templates/bank/internal/payment/workflows/transfer.go`
- Create: `templates/bank/internal/payment/workflows/transfer_test.go`

**Interfaces:**
- Produces: workflow definition `payment-transfer` version 1 with ordered Actions `AuthorizeRisk`, `PlaceFundsHold`, `PostLedgerTransfer`.

- [ ] **Step 1: Write exact dispatch contract tests**

Assert routing keys, allowed result events, 15-second deadline, and semantic
idempotency keys:

```text
wf:<workflow_id>:authorize-risk
wf:<workflow_id>:place-funds-hold
wf:<workflow_id>:post-ledger-transfer
wf:<workflow_id>:compensate:place-funds-hold
wf:<workflow_id>:compensate:authorize-risk
wf:<workflow_id>:compensate:post-ledger-transfer
```

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/payment/workflows
```

Expected: compile failure.

- [ ] **Step 3: Implement Actions and result classification**

Map rejected and insufficient-funds results to `business_rejected`; broker or
dependency results explicitly marked retryable to `transient_failure`; ledger
balance errors to `invariant_violation`; unmatched command/event identity to
`invalid_message`.

- [ ] **Step 4: Test compensation order through the real engine**

Register the definition with the workflow engine and simulate failure after the
hold. Assert the next Outbox command releases the hold, followed by voiding the
risk authorization.

- [ ] **Step 5: Run tests and commit**

```bash
cd templates/bank
go test ./internal/payment/workflows ./internal/platform/workflow
git add templates/bank/internal/payment/workflows
git commit -m "feat(bank): define payment transfer workflow actions"
```

### Task 7: Add Payment API and Runtime

**Files:**
- Modify: `templates/bank/db/migrations/pay_db.sql`
- Create: `templates/bank/internal/payment/api/workflows.go`
- Create: `templates/bank/internal/payment/api/workflows_test.go`
- Create: `templates/bank/internal/payment/consumer.go`
- Create: `templates/bank/internal/payment/consumer_test.go`
- Modify: `templates/bank/cmd/payment/main.go`

**Interfaces:**
- Produces: `POST /api/v1/payments/workflows`, `GET /api/v1/payments/workflows/{id}`, and `POST /api/v1/payments/workflows/{id}/reverse`.
- Consumes: workflow result events from RabbitMQ.

- [ ] **Step 1: Write API tests**

Require `Idempotency-Key`, JSON content type, positive integer minor units, and
stable `application/problem+json` codes. Repeating the same key and body returns
the same workflow ID; same key with different body returns 409.

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/payment/api
```

Expected: compile failure.

- [ ] **Step 3: Implement payment and API persistence**

Add `payment_intent` with unique client idempotency key, request hash, workflow
ID, amount, currency, payer/payee, and status. Creating payment intent and
workflow instance occurs in one `*sql.Tx`: insert the intent through the payment
repository, call `workflowStore.CreateInTx(ctx, tx, startRequest)`, and commit.

- [ ] **Step 4: Implement event consumer and runtime composition**

The payment consumer passes result envelopes to `Engine.ApplyResult`. The
process starts REST, Outbox relay, workflow result consumer, and recovery
scheduler. Readiness covers every worker.

- [ ] **Step 5: Run tests and commit**

```bash
cd templates/bank
go test ./internal/payment/... ./cmd/payment
git add templates/bank/db/migrations/pay_db.sql templates/bank/internal/payment templates/bank/cmd/payment/main.go
git commit -m "feat(bank): expose and run payment workflows"
```

### Task 8: Add Completion, Reward Consumer, and Reversal Workflow

**Files:**
- Create: `templates/bank/internal/payment/workflows/reversal.go`
- Create: `templates/bank/internal/payment/workflows/reversal_test.go`
- Modify: `templates/bank/internal/reward/consumer.go`
- Create: `templates/bank/internal/reward/consumer_test.go`
- Modify: `templates/bank/cmd/reward/main.go`

**Interfaces:**
- Produces: `payment.completed.v1`.
- Produces: `payment-reversal` version 1 referencing a succeeded payment.
- Consumes: payment completion in reward without affecting payment status.

- [ ] **Step 1: Write completion and reversal tests**

Assert completion and payment status commit with one Outbox event. Assert a
reversal creates a new workflow, does not reopen the succeeded transfer
workflow, and dispatches `core.reverse-transfer.v1`.

- [ ] **Step 2: Write reward isolation test**

Make reward processing fail permanently and assert the payment remains
`succeeded`; the reward message reaches reward DLQ.

- [ ] **Step 3: Implement and run tests**

Run:

```bash
cd templates/bank
go test ./internal/payment/workflows ./internal/reward
```

Expected: PASS after implementation.

- [ ] **Step 4: Commit**

```bash
git add templates/bank/internal/payment templates/bank/internal/reward templates/bank/cmd/reward/main.go
git commit -m "feat(bank): complete reward and reversal flows"
```

### Task 9: Add Saga Integration Tests and Package

**Files:**
- Create: `templates/bank/test/payment_saga_integration_test.go`
- Modify: `internal/template/templates.tar`

**Interfaces:**
- Produces: database-and-broker integration coverage for happy path, rejection, insufficient funds, duplicate delivery, and compensation.

- [ ] **Step 1: Add tagged integration scenarios**

Each scenario asserts exact workflow, Action, authorization, hold, voucher,
Inbox, and Outbox row counts. The duplicate scenario publishes identical
command and result envelopes twice.

- [ ] **Step 2: Run integration and full unit tests**

Run:

```bash
cd templates/bank
go test ./...
go test -tags=integration -p 1 ./...
cd ../..
go generate ./internal/template
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add templates/bank/test internal/template/templates.tar
git commit -m "build(bank): package durable payment saga"
```
