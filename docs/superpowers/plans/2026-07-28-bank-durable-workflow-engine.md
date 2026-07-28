# Bank Durable Workflow Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a payment-owned durable linear workflow engine with immutable Preparation context, transactional command dispatch, event-driven resume, leases, retries, reverse compensation, and version safety.

**Architecture:** Domain-neutral workflow definitions produce dispatch intents; the engine persists state and writes commands into the payment Outbox in the same transaction. Result events arrive through the Inbox, lock one instance, and advance or compensate exactly once.

**Tech Stack:** Go 1.22, `database/sql` with the pgx stdlib driver, PostgreSQL 16, RabbitMQ Outbox/Inbox primitives from the bank chassis, Prometheus.

## Global Constraints

- The engine supports one Preparation phase and linear Actions only.
- At-least-once messages are made safe by Inbox and semantic idempotency keys.
- A timeout is `unknown_outcome`, never proof of remote failure.
- Compensation runs in reverse order and financial steps cannot be skipped automatically.
- Definition version is persisted and required at startup for every non-terminal instance.
- Every database transition that emits a command writes the Outbox in the same transaction.
- Every task ends with focused tests and a commit.

---

## File Map

- `templates/bank/internal/platform/workflow/types.go`: states, errors, views, and dispatch contracts.
- `templates/bank/internal/platform/workflow/registry.go`: definition version registry.
- `templates/bank/internal/platform/workflow/engine.go`: pure transition orchestration.
- `templates/bank/internal/platform/workflow/store.go`: persistence interface.
- `templates/bank/internal/platform/workflow/postgres.go`: `database/sql` implementation.
- `templates/bank/internal/platform/workflow/recovery.go`: leases and timeout scheduling.
- `templates/bank/db/migrations/pay_db.sql`: workflow schema.

### Task 1: Define Workflow Contracts and Version Registry

**Files:**
- Create: `templates/bank/internal/platform/workflow/types.go`
- Create: `templates/bank/internal/platform/workflow/registry.go`
- Create: `templates/bank/internal/platform/workflow/registry_test.go`

**Interfaces:**
- Produces: `Definition`, `Action`, `Dispatch`, `Outcome`, `View`, `ErrorClass`, `InstanceStatus`, and `ActionStatus`.
- Produces: `Registry.Register(Definition) error` and `Registry.Get(string, int) (Definition, bool)`.

- [ ] **Step 1: Write failing registry tests**

```go
func TestRegistryRejectsDuplicateVersion(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{workflowType: "payment-transfer", version: 1}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); !errors.Is(err, ErrDuplicateDefinition) {
		t.Fatalf("error=%v", err)
	}
}
```

Also reject empty workflow type, version below 1, empty Action names, and
duplicate Action names.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
```

Expected: compile failure because the package does not exist.

- [ ] **Step 3: Implement exact public types**

```go
type ErrorClass string
const (
	BusinessRejected ErrorClass = "business_rejected"
	TransientFailure ErrorClass = "transient_failure"
	UnknownOutcome ErrorClass = "unknown_outcome"
	InvariantViolation ErrorClass = "invariant_violation"
	InvalidMessage ErrorClass = "invalid_message"
)

type InstanceStatus string
const (
	StatusPreparing InstanceStatus = "preparing"
	StatusReady InstanceStatus = "ready"
	StatusRunning InstanceStatus = "running"
	StatusSucceeded InstanceStatus = "succeeded"
	StatusRejected InstanceStatus = "rejected"
	StatusCompensating InstanceStatus = "compensating"
	StatusCompensated InstanceStatus = "compensated"
	StatusCompensationFailed InstanceStatus = "compensation_failed"
)

type ActionStatus string
const (
	ActionPending ActionStatus = "pending"
	ActionWaitingResult ActionStatus = "waiting_result"
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed ActionStatus = "failed"
	ActionCompensating ActionStatus = "compensating"
	ActionCompensated ActionStatus = "compensated"
	ActionCompensationFailed ActionStatus = "compensation_failed"
)

type StartRequest struct {
	WorkflowID   string
	Type         string
	Version      int
	Input        json.RawMessage
	CorrelationID string
	CreatedAt    time.Time
}

type Instance struct {
	ID              string
	Type            string
	Version         int
	Status          InstanceStatus
	Input           json.RawMessage
	PreparedContext json.RawMessage
	CurrentAction   int
	Revision        int64
	LeaseOwner      string
	LeaseUntil      time.Time
	NextWakeupAt    time.Time
	OperationalDeadline time.Time
	LastErrorClass  ErrorClass
	LastError       string
	Actions         []ActionRecord
}

type ActionRecord struct {
	Index          int
	Name           string
	Status         ActionStatus
	Direction      string
	Attempt        int
	IdempotencyKey string
	CommandID      string
	ResultEventID  string
	DeadlineAt     time.Time
	Output         json.RawMessage
	LastErrorClass ErrorClass
	LastError      string
}

type View struct {
	Instance Instance
	Action   ActionRecord
}

type Dispatch struct {
	RoutingKey         string
	Payload            json.RawMessage
	AcceptedResultTypes []string
	Deadline           time.Duration
	IdempotencyKey     string
}

type Outcome struct {
	Succeeded bool
	Class     ErrorClass
	Output    json.RawMessage
	Message   string
}

type DefinitionRef struct {
	Type    string
	Version int
}

type Definition interface {
	Type() string
	Version() int
	Prepare(context.Context, json.RawMessage) (json.RawMessage, error)
	Actions() []Action
}

type Action interface {
	Name() string
	Execute(context.Context, View) (Dispatch, error)
	ApplyResult(context.Context, View, messaging.Envelope) (Outcome, error)
	Compensate(context.Context, View) (Dispatch, error)
	ApplyCompensationResult(context.Context, View, messaging.Envelope) (Outcome, error)
}

type EngineConfig struct {
	ExecuteMaxAttempts      int
	CompensationMaxAttempts int
	OperationalDeadline     time.Duration
	Now                     func() time.Time
}
```

`Dispatch` contains routing key, payload, accepted result types, deadline, and
idempotency key. `Outcome` contains `Succeeded`, `Class`, `Output`, and
`Message`. Engine defaults are three execution attempts, five compensation
attempts, and a two-minute operational deadline. Crossing that deadline records
an observable error and wake-up state; it never deletes or abandons an
instance.

Construct the engine with:

```go
func NewEngine(Store, *Registry, EngineConfig) *Engine
```

- [ ] **Step 4: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): define durable workflow contracts"
```

### Task 2: Add Workflow Database Schema

**Files:**
- Modify: `templates/bank/db/migrations/pay_db.sql`
- Create: `templates/bank/internal/platform/workflow/schema_test.go`

**Interfaces:**
- Produces: `workflow_instance` and `workflow_action` tables with the columns and constraints from the approved design.

- [ ] **Step 1: Write a failing schema contract test**

Parse the migration and require:

```go
required := []string{
	"CREATE TABLE workflow_instance",
	"definition_version INTEGER NOT NULL",
	"prepared_context_json JSONB",
	"revision BIGINT NOT NULL",
	"lease_owner TEXT",
	"lease_until TIMESTAMPTZ",
	"CREATE TABLE workflow_action",
	"UNIQUE (workflow_id, action_index)",
	"idempotency_key TEXT NOT NULL",
	"command_id TEXT",
	"result_event_id TEXT",
}
```

- [ ] **Step 2: Run the test and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run TestPaymentMigrationContainsWorkflowSchema
```

Expected: FAIL with missing schema fragments.

- [ ] **Step 3: Add schema and constraints**

Use enum-like checks for instance and Action states, foreign key Actions to
instances with `ON DELETE RESTRICT`, unique non-null command IDs, indexes on
`(status, next_wakeup_at)` and `(lease_until)`, and JSON object checks for input
and prepared context.

- [ ] **Step 4: Run schema and migration tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow ./internal/platform/migrate
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/db/migrations/pay_db.sql templates/bank/internal/platform/workflow/schema_test.go
git commit -m "feat(bank): add workflow persistence schema"
```

### Task 3: Implement Preparation and Initial Dispatch

**Files:**
- Create: `templates/bank/internal/platform/workflow/store.go`
- Create: `templates/bank/internal/platform/workflow/engine.go`
- Create: `templates/bank/internal/platform/workflow/engine_test.go`

**Interfaces:**
- Produces: `Engine.Start(context.Context, StartRequest) (Instance, error)`.
- Produces: `Engine.Prepare(context.Context, string) error`.
- Consumes: `Store.WithInstance(context.Context, string, func(Tx) error)`.

- [ ] **Step 1: Write failing engine tests**

Test:

```go
func TestPreparePersistsImmutableContextAndFirstCommand(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-1", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{"amount":100}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	got := store.instance("wf-1")
	assertStatus(t, got, StatusRunning)
	assertActionStatus(t, got, 0, ActionWaitingResult)
	assertOutboxCount(t, store, 1)
}
```

Also test Preparation retry after transient failure and `rejected` after
business validation.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run 'TestPrepare|TestStart'
```

Expected: compile failure for missing engine and store contracts.

- [ ] **Step 3: Implement store transaction contracts**

```go
type Store interface {
	Create(context.Context, StartRequest) (Instance, error)
	WithInstance(context.Context, string, func(Tx) error) error
}

type Tx interface {
	Instance() *Instance
	InsertInbox(consumer string, envelope messaging.Envelope) (inserted bool, err error)
	SaveInstance(Instance) error
	SaveAction(ActionRecord) error
	AppendOutbox(messaging.Envelope, string) error
}
```

`Prepare` calls the registered definition outside the transaction, then locks
the instance and verifies it remains `preparing` before saving the immutable
context and first dispatch.

The concrete PostgreSQL store additionally exposes:

```go
func (s *PostgresStore) CreateInTx(context.Context, *sql.Tx, StartRequest) (Instance, error)
```

This lets payment create its local intent and workflow instance in one
transaction without putting payment-domain methods on the generic workflow
`Tx` interface.

- [ ] **Step 4: Run focused tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run 'TestPrepare|TestStart'
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): prepare workflows and dispatch first action"
```

### Task 4: Apply Results and Advance Exactly Once

**Files:**
- Modify: `templates/bank/internal/platform/workflow/store.go`
- Modify: `templates/bank/internal/platform/workflow/engine.go`
- Modify: `templates/bank/internal/platform/workflow/engine_test.go`

**Interfaces:**
- Produces: `Engine.ApplyResult(context.Context, messaging.Envelope) error`.
- Consumes: Inbox insertion, row lock, Action result parser, and Outbox append in one store transaction.

- [ ] **Step 1: Write failing result tests**

Cover successful first Action, duplicate event, wrong command ID, unexpected
event type, concurrent duplicate calls, and final success:

```go
func TestDuplicateResultAdvancesOnlyOnce(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	event := successEvent("wf-1", "first", "cmd-1")
	runConcurrently(t, 2, func() error {
		return engine.ApplyResult(context.Background(), event)
	})
	assertActionStatus(t, store.instance("wf-1"), 0, ActionSucceeded)
	assertOutboxCount(t, store, 2) // first command plus exactly one second command
}
```

- [ ] **Step 2: Run focused tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run 'TestDuplicateResult|TestApplyResult'
```

Expected: compile failure or failed assertions.

- [ ] **Step 3: Implement transactional result application**

Insert Inbox first. On duplicate, return nil. Validate workflow ID, Action name,
command ID, accepted event type, and current Action state. Save output and
either dispatch the next Action or mark the instance `succeeded`.

- [ ] **Step 4: Run focused and race tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
go test -race ./internal/platform/workflow
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): resume workflows from result events"
```

### Task 5: Implement Reverse Compensation

**Files:**
- Modify: `templates/bank/internal/platform/workflow/engine.go`
- Modify: `templates/bank/internal/platform/workflow/engine_test.go`

**Interfaces:**
- Produces: reverse compensation stack and `compensated` or `compensation_failed` states.

- [ ] **Step 1: Write failing compensation tests**

Test a three-step workflow where step 3 returns a terminal failure. Assert
compensation dispatch order is step 2 then step 1. Also assert a rejected step
is never compensated and a transient compensation failure retries the same
semantic idempotency key.

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run 'TestCompens'
```

Expected: failed assertions because compensation is not implemented.

- [ ] **Step 3: Implement compensation transitions**

On terminal execution failure:

```text
running -> compensating
last succeeded Action -> compensating -> compensated
previous succeeded Action -> compensating
no remaining Actions -> workflow compensated
```

After five transient attempts, mark the Action and workflow
`compensation_failed`, preserve `current_action`, and emit a failure metric.

- [ ] **Step 4: Run tests and race detector**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
go test -race ./internal/platform/workflow
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): compensate workflows in reverse order"
```

### Task 6: Add Leases, Timeouts, and Version Safety

**Files:**
- Create: `templates/bank/internal/platform/workflow/recovery.go`
- Create: `templates/bank/internal/platform/workflow/recovery_test.go`
- Modify: `templates/bank/internal/platform/workflow/store.go`
- Modify: `templates/bank/internal/platform/workflow/engine.go`

**Interfaces:**
- Produces: `Recovery.Run(context.Context) error`, lease claim/renew/release, and startup version audit.

- [ ] **Step 1: Write failing recovery tests**

Use an injected clock. Assert that a lease cannot be stolen before expiry, can
be claimed after expiry, and a waiting Action timeout re-dispatches with a new
message ID but the same command ID and idempotency key.

Assert startup audit fails:

```go
if !errors.Is(err, ErrDefinitionUnavailable) {
	t.Fatalf("error=%v", err)
}
```

- [ ] **Step 2: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow -run 'TestLease|TestTimeout|TestDefinition'
```

Expected: compile failure.

- [ ] **Step 3: Implement recovery API**

```go
type RecoveryConfig struct {
	Owner        string
	Lease        time.Duration
	PollInterval time.Duration
	Now          func() time.Time
}

func NewRecovery(store Store, engine *Engine, registry *Registry, cfg RecoveryConfig) *Recovery
func (r *Recovery) AuditDefinitions(context.Context) error
func (r *Recovery) Run(context.Context) error
```

Defaults are lease 30 seconds and poll interval 1 second.

Extend `Store` with these exact recovery methods:

```go
ClaimRunnable(context.Context, string, time.Time, time.Duration, int) ([]string, error)
RenewLease(context.Context, string, string, time.Time, time.Duration) error
ReleaseLease(context.Context, string, string) error
TimedOut(context.Context, time.Time, int) ([]string, error)
NonTerminalDefinitions(context.Context) ([]DefinitionRef, error)
```

- [ ] **Step 4: Run full workflow tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
go test -race ./internal/platform/workflow
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): recover leased and timed out workflows"
```

### Task 7: Implement the PostgreSQL Store

**Files:**
- Create: `templates/bank/internal/platform/workflow/postgres.go`
- Create: `templates/bank/internal/platform/workflow/postgres_integration_test.go`
- Modify: `templates/bank/internal/platform/workflow/store.go`

**Interfaces:**
- Produces: `NewPostgresStore(*sql.DB) *PostgresStore`.
- Enforces: Inbox, row locks, revision updates, Action persistence, and Outbox append in one SQL transaction.

- [ ] **Step 1: Write tagged integration tests**

Tests create a migrated payment database and assert:

- two concurrent result transactions produce one revision increment;
- Outbox rollback occurs if Action save fails;
- expired lease uses `FOR UPDATE SKIP LOCKED`;
- duplicate Inbox event does not invoke the handler.

- [ ] **Step 2: Run integration tests and verify failure**

Run:

```bash
cd templates/bank
go test -tags=integration ./internal/platform/workflow
```

Expected: compile failure for `NewPostgresStore`.

- [ ] **Step 3: Implement SQL persistence**

Use `SELECT ... FOR UPDATE`, `UPDATE ... WHERE revision=$expected`, and a unique
Inbox insert with `ON CONFLICT DO NOTHING RETURNING message_id`. Outbox insert
stores the complete serialized envelope and routing key. `CreateInTx` uses the
caller-supplied `*sql.Tx`; `Create` opens and commits its own transaction
through `pg.RunInTx`.

- [ ] **Step 4: Run integration, unit, and race tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
go test -race ./internal/platform/workflow
go test -tags=integration ./internal/platform/workflow
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/workflow
git commit -m "feat(bank): persist durable workflows in PostgreSQL"
```

### Task 8: Add Workflow Metrics and Package the Engine

**Files:**
- Create: `templates/bank/internal/platform/workflow/metrics.go`
- Create: `templates/bank/internal/platform/workflow/metrics_test.go`
- Modify: `templates/bank/internal/platform/workflow/engine.go`
- Modify: `internal/template/templates.tar`

**Interfaces:**
- Produces: workflow status, duration, attempt, waiting age, compensation, and failure metrics.

- [ ] **Step 1: Write failing metric tests**

Use a new Prometheus registry and assert gathered names include:

```text
workflow_instances
workflow_action_duration_seconds
workflow_action_attempts_total
workflow_compensation_total
workflow_compensation_failures_total
workflow_waiting_age_seconds
```

- [ ] **Step 2: Implement and verify metrics**

Run:

```bash
cd templates/bank
go test ./internal/platform/workflow
go test -race ./internal/platform/workflow
cd ../..
go generate ./internal/template
go test ./...
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add templates/bank/internal/platform/workflow internal/template/templates.tar
git commit -m "build(bank): package durable workflow engine"
```
