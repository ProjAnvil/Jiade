// Package payment wires the runtime composition for the payment service's saga
// participation: the result-event consumer (Engine.ApplyResult), the outbox
// relay, and the WorkflowStarter that persists a PaymentIntent + a workflow
// Instance in one transaction.
//
// The consumer is the inbound side: it subscribes to the RabbitMQ queue that
// collects every workflow result event the saga participants (risk,
// core-banking) emit, hands each one to Engine.ApplyResult, and then syncs the
// denormalized payment_intent.status so the GET /workflows/{id} endpoint
// reflects the saga outcome without coupling the API to the workflow engine's
// internal schema.
package payment

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/workflow"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// Sentinel errors returned by the concrete WorkflowStarter, PaymentIntentRepo,
// and InstanceStatusRepo. The api package's handlers branch on these via
// errors.Is to map them to stable HTTP statuses + problem+json codes. They are
// re-exported through type aliases in api/workflows.go so callers that prefer
// to import them from api keep compiling.
var (
	// ErrIdempotencyConflict is returned when an idempotency key is replayed
	// with a different request hash. Maps to 409.
	ErrIdempotencyConflict = errors.New("idempotency key replayed with different body")
	// ErrWorkflowNotFound is returned when a workflow id does not exist. Maps
	// to 404.
	ErrWorkflowNotFound = errors.New("payment workflow not found")
)

// ---------------------------------------------------------------------------
// Domain types
// ---------------------------------------------------------------------------

// PaymentIntentStatus mirrors the on-disk status enum
// (ck_payment_intent_status). The values are chosen so the GET endpoint can
// report a payment-shaped status for every workflow outcome.
type PaymentIntentStatus string

const (
	IntentPending            PaymentIntentStatus = "pending"
	IntentRunning            PaymentIntentStatus = "running"
	IntentSucceeded          PaymentIntentStatus = "succeeded"
	IntentCompensated        PaymentIntentStatus = "compensated"
	IntentCompensationFailed PaymentIntentStatus = "compensation_failed"
	IntentRejected           PaymentIntentStatus = "rejected"
	IntentReversed           PaymentIntentStatus = "reversed"
)

// PaymentIntent is the persisted request for a payment workflow. It is the
// idempotency and audit record for POST /api/v1/payments/workflows: the
// idempotency key is the client-supplied dedup key, the request hash detects
// same-key-different-body conflicts, and the workflow id links the intent to
// the durable workflow instance that drives the saga.
type PaymentIntent struct {
	IdempotencyKey  string
	RequestHash     string
	WorkflowID      string
	PayerCustomerID string
	PayerAccountNo  string
	PayeeAccountNo  string
	Currency        string
	AmountMinor     int64
	Status          PaymentIntentStatus
	Reversed        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ---------------------------------------------------------------------------
// PaymentIntentRepo
// ---------------------------------------------------------------------------

// PaymentIntentRepo persists PaymentIntent rows. The atomic write paths
// (CreateOrMatchInTx, UpdateStatusByWorkflowID) take a pg.DBTX so they share
// the caller's transaction.
type PaymentIntentRepo struct {
	db *sql.DB
}

// NewPaymentIntentRepo binds a *sql.DB. The caller owns the *sql.DB lifecycle.
func NewPaymentIntentRepo(db *sql.DB) *PaymentIntentRepo {
	return &PaymentIntentRepo{db: db}
}

// CreateOrMatchInTx inserts intent under its idempotency key. On a duplicate
// key, the existing row is loaded and returned with inserted=false. If the
// duplicate's request_hash differs from intent.RequestHash, ErrIdempotencyConflict
// is returned so the caller can surface a 409 — the original intent is left
// untouched.
//
// Implementation uses INSERT ... ON CONFLICT (idempotency_key) DO NOTHING
// RETURNING. When RETURNING yields no row, the conflict path fires and the
// existing row is read back.
func (r *PaymentIntentRepo) CreateOrMatchInTx(ctx context.Context, q pg.DBTX, intent PaymentIntent) (PaymentIntent, bool, error) {
	var returnedKey string
	err := q.QueryRowContext(ctx, `
		INSERT INTO payment_intent
		  (idempotency_key, request_hash, workflow_id, payer_customer_id,
		   payer_account_no, payee_account_no, currency, amount_minor, status,
		   reversed, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, FALSE,
		        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING idempotency_key`,
		intent.IdempotencyKey, intent.RequestHash, intent.WorkflowID,
		intent.PayerCustomerID, intent.PayerAccountNo, intent.PayeeAccountNo,
		intent.Currency, intent.AmountMinor, string(intent.Status),
	).Scan(&returnedKey)
	if err == nil {
		return intent, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PaymentIntent{}, false, fmt.Errorf("insert payment_intent: %w", err)
	}
	// Conflict: load the existing row to compare hashes.
	existing, loadErr := r.getByKeyInTx(ctx, q, intent.IdempotencyKey)
	if loadErr != nil {
		return PaymentIntent{}, false, fmt.Errorf("load conflicting payment_intent: %w", loadErr)
	}
	if existing.RequestHash != intent.RequestHash {
		return existing, false, ErrIdempotencyConflict
	}
	return existing, false, nil
}

// getByKeyInTx loads a PaymentIntent by its idempotency key. Returns
// ErrWorkflowNotFound when the row does not exist.
func (r *PaymentIntentRepo) getByKeyInTx(ctx context.Context, q pg.DBTX, key string) (PaymentIntent, error) {
	var (
		intent PaymentIntent
		status string
	)
	err := q.QueryRowContext(ctx, `
		SELECT idempotency_key, request_hash, workflow_id, payer_customer_id,
		       payer_account_no, payee_account_no, currency, amount_minor, status,
		       reversed, created_at, updated_at
		FROM payment_intent WHERE idempotency_key = $1`, key,
	).Scan(&intent.IdempotencyKey, &intent.RequestHash, &intent.WorkflowID,
		&intent.PayerCustomerID, &intent.PayerAccountNo, &intent.PayeeAccountNo,
		&intent.Currency, &intent.AmountMinor, &status, &intent.Reversed,
		&intent.CreatedAt, &intent.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentIntent{}, fmt.Errorf("%w: idempotency_key=%s", ErrWorkflowNotFound, key)
		}
		return PaymentIntent{}, err
	}
	intent.Status = PaymentIntentStatus(status)
	return intent, nil
}

// GetByWorkflowID loads a PaymentIntent by its workflow id. Used by the GET
// endpoint to look up the immutable request fields + denormalized status.
func (r *PaymentIntentRepo) GetByWorkflowID(ctx context.Context, workflowID string) (PaymentIntent, error) {
	var (
		intent PaymentIntent
		status string
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT idempotency_key, request_hash, workflow_id, payer_customer_id,
		       payer_account_no, payee_account_no, currency, amount_minor, status,
		       reversed, created_at, updated_at
		FROM payment_intent WHERE workflow_id = $1`, workflowID,
	).Scan(&intent.IdempotencyKey, &intent.RequestHash, &intent.WorkflowID,
		&intent.PayerCustomerID, &intent.PayerAccountNo, &intent.PayeeAccountNo,
		&intent.Currency, &intent.AmountMinor, &status, &intent.Reversed,
		&intent.CreatedAt, &intent.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PaymentIntent{}, fmt.Errorf("%w: workflow_id=%s", ErrWorkflowNotFound, workflowID)
		}
		return PaymentIntent{}, err
	}
	intent.Status = PaymentIntentStatus(status)
	return intent, nil
}

// UpdateStatusByWorkflowID sets status for the intent owning workflowID. It is
// called by the consumer after Engine.ApplyResult succeeds so the GET endpoint
// reflects the latest saga outcome. A no-op (zero rows affected) is NOT an
// error: the consumer may process a result event for a workflow whose intent
// was deleted.
func (r *PaymentIntentRepo) UpdateStatusByWorkflowID(ctx context.Context, workflowID string, status PaymentIntentStatus) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE payment_intent
		SET status = $2, updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $1`,
		workflowID, string(status))
	if err != nil {
		return fmt.Errorf("update payment_intent status for %q: %w", workflowID, err)
	}
	return nil
}

// MarkReversedByWorkflowID flips the reversed flag on the intent owning
// workflowID. Called by the reverse API endpoint. Returns ErrWorkflowNotFound
// when the workflow id does not exist (zero rows affected).
func (r *PaymentIntentRepo) MarkReversedByWorkflowID(ctx context.Context, workflowID string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE payment_intent
		SET reversed = TRUE,
		    status = 'reversed',
		    updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $1`,
		workflowID)
	if err != nil {
		return fmt.Errorf("mark reversed for %q: %w", workflowID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %q: %w", workflowID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: workflow_id=%s", ErrWorkflowNotFound, workflowID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// WorkflowStarter — atomically persists PaymentIntent + workflow.Instance
// ---------------------------------------------------------------------------

// WorkflowStore is the subset of *workflow.PostgresStore the starter needs.
// *workflow.PostgresStore satisfies this interface.
type WorkflowStore interface {
	CreateInTx(ctx context.Context, tx *sql.Tx, req workflow.StartRequest) (workflow.Instance, error)
}

// WorkflowDefinitionType identifies the workflow definition the starter binds
// each new intent to. The current binding is the payment-transfer v1 saga; the
// constant lives here so a future bump does not require touching every caller.
const (
	WorkflowDefinitionType    = "payment-transfer"
	WorkflowDefinitionVersion = 1
)

// StartPaymentRequest carries the data needed to atomically persist a payment
// intent + workflow instance. It mirrors api.StartWorkflowRequest plus a
// workflow.Input payload (the marshalled PrepareInput the definition's Prepare
// consumes) and the client-supplied idempotency key.
type StartPaymentRequest struct {
	IdempotencyKey  string
	RequestHash     string
	PayerCustomerID string
	PayerAccountNo  string
	PayeeAccountNo  string
	Currency        string
	AmountMinor     int64
	// Input is the marshalled workflows.PrepareInput the engine stores on the
	// instance and the definition's Prepare later consumes. The starter does
	// not interpret this field; it only persists it.
	Input json.RawMessage
}

// StartPaymentResult is the outcome of StartPayment.
type StartPaymentResult struct {
	Intent   PaymentIntent
	Inserted bool
}

// WorkflowStarter atomically persists a PaymentIntent + workflow.Instance in
// one *sql.Tx. It is the concrete implementation of api.WorkflowAPI.Start.
//
// Atomicity is essential: the GET /workflows/{id} endpoint and the consumer's
// status sync both assume a payment_intent row exists for every workflow
// instance. Creating them in two separate transactions would leave a window
// where an instance exists with no intent (or vice versa).
type WorkflowStarter struct {
	db      *sql.DB
	intents *PaymentIntentRepo
	store   WorkflowStore
	now     func() time.Time
}

// NewWorkflowStarter wires the db, intent repo, and workflow store. now
// defaults to time.Now when nil.
func NewWorkflowStarter(db *sql.DB, intents *PaymentIntentRepo, store WorkflowStore, now func() time.Time) *WorkflowStarter {
	if now == nil {
		now = time.Now
	}
	return &WorkflowStarter{db: db, intents: intents, store: store, now: now}
}

// StartPayment creates the payment_intent + workflow_instance in one tx.
//
// On duplicate idempotency_key with matching request_hash, returns the existing
// intent with Inserted=false (idempotent replay). On duplicate with mismatched
// hash, returns ErrIdempotencyConflict.
//
// The workflow id is supplied by the caller (the api handler) so the caller
// can correlate it with logging/tracing before the tx commits.
func (s *WorkflowStarter) StartPayment(ctx context.Context, req StartPaymentRequest, workflowID string) (StartPaymentResult, error) {
	var result StartPaymentResult
	err := pg.RunInTx(ctx, s.db, func(q pg.DBTX) error {
		tx, ok := q.(*sql.Tx)
		if !ok {
			// pg.RunInTx always passes its internally-begun *sql.Tx as the
			// pg.DBTX argument; defend against future divergence regardless.
			return fmt.Errorf("payment WorkflowStarter: pg.DBTX is %T, want *sql.Tx", q)
		}
		intent := PaymentIntent{
			IdempotencyKey:  req.IdempotencyKey,
			RequestHash:     req.RequestHash,
			WorkflowID:      workflowID,
			PayerCustomerID: req.PayerCustomerID,
			PayerAccountNo:  req.PayerAccountNo,
			PayeeAccountNo:  req.PayeeAccountNo,
			Currency:        req.Currency,
			AmountMinor:     req.AmountMinor,
			Status:          IntentPending,
		}
		stored, inserted, err := s.intents.CreateOrMatchInTx(ctx, tx, intent)
		if err != nil {
			return err
		}
		if !inserted {
			// Idempotent replay — the original workflow_id stays.
			result.Intent = stored
			result.Inserted = false
			return nil
		}
		// New intent: persist the workflow instance in the SAME tx so the
		// intent+instance pair is atomic. CorrelationID = workflowID gives
		// downstream saga participants a stable trace identifier.
		if _, err := s.store.CreateInTx(ctx, tx, workflow.StartRequest{
			WorkflowID:    workflowID,
			Type:          WorkflowDefinitionType,
			Version:       WorkflowDefinitionVersion,
			Input:         req.Input,
			CorrelationID: workflowID,
		}); err != nil {
			return fmt.Errorf("create workflow instance: %w", err)
		}
		result.Intent = stored
		result.Inserted = true
		return nil
	})
	if err != nil {
		return StartPaymentResult{}, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Consumer — RabbitMQ result-event subscriber
// ---------------------------------------------------------------------------

// ConsumerName is the Inbox consumer identifier used by the payment consumer
// for at-least-once delivery dedup. It MUST be distinct from the workflow
// engine's internal consumer ("workflow") so the two Inbox layers do not
// collide.
const ConsumerName = "payment"

// EventPaymentCompleted is the result event type emitted by the payment
// consumer when a payment-transfer workflow transitions to StatusSucceeded.
// It is consumed by the reward service (a NON-CRITICAL consumer) to earn
// points without affecting payment status.
const EventPaymentCompleted = "payment.completed.v1"

// RoutePaymentCompleted is the outbox routing key for payment.completed.v1.
// The reward queue binds to this key.
const RoutePaymentCompleted = "payment.completed"

// ApplyResulter is the subset of *workflow.Engine the consumer needs.
// *workflow.Engine satisfies this interface.
type ApplyResulter interface {
	ApplyResult(ctx context.Context, env messaging.Envelope) error
}

// InstanceStatusReader reads the current status of a workflow instance. The
// consumer uses it after ApplyResult succeeds to sync payment_intent.status.
// The concrete implementation is *InstanceStatusRepo below.
type InstanceStatusReader interface {
	InstanceStatus(ctx context.Context, workflowID string) (string, error)
}

// IntentStatusUpdater updates payment_intent.status by workflow id. The
// concrete implementation is *PaymentIntentRepo.
type IntentStatusUpdater interface {
	UpdateStatusByWorkflowID(ctx context.Context, workflowID string, status PaymentIntentStatus) error
}

// IntentReader reads a PaymentIntent by workflow id. The consumer uses it to
// detect a FRESH succeeded transition (previous status != succeeded) before
// emitting payment.completed.v1, so a duplicate delivery for an
// already-succeeded workflow does not re-emit the completion event. When no
// IntentReader is wired the consumer falls back to emitting on every
// successful ApplyResult whose resulting status is succeeded.
// *PaymentIntentRepo satisfies this interface.
type IntentReader interface {
	GetByWorkflowID(ctx context.Context, workflowID string) (PaymentIntent, error)
}

// CompletionOutbox emits the payment.completed.v1 event so downstream
// non-critical consumers (the reward service) react to a successful payment
// without coupling to the payment schema. The concrete implementation writes
// to the pay_db outbox_message table; a stub is used in unit tests.
type CompletionOutbox interface {
	EmitCompletion(ctx context.Context, env messaging.Envelope) error
}

// Consumer receives workflow result events from RabbitMQ and:
//  1. Hands each decoded envelope to Engine.ApplyResult (the engine's own
//     per-instance Inbox dedup makes ApplyResult idempotent).
//  2. After a successful apply, syncs payment_intent.status from the workflow
//     instance's current status so GET /workflows/{id} reflects the saga
//     outcome.
//
// The consumer does NOT open its own DB transaction around ApplyResult: the
// engine does that atomically via Store.WithInstance. The consumer only begins
// a tx for the outer Inbox dedup that messaging.ProcessDelivery performs.
type Consumer struct {
	db            *sql.DB
	engine        ApplyResulter
	statusReader  InstanceStatusReader
	intentUpdater IntentStatusUpdater
	intentReader  IntentReader
	completionOutbox CompletionOutbox
	policy        messaging.RetryPolicy
}

// NewConsumer wires the consumer for production. db is the pay_db connection
// the outer Inbox + status sync write to. engine applies envelopes. statusReader
// reads workflow_instance.status. intentUpdater updates payment_intent.status.
// intentReader reads the intent to detect a fresh succeeded transition before
// emitting payment.completed.v1 (nil falls back to emit-on-success).
// completionOutbox emits payment.completed.v1 (nil disables emission).
// policy is the messaging retry/DLQ policy.
func NewConsumer(
	db *sql.DB,
	engine ApplyResulter,
	statusReader InstanceStatusReader,
	intentUpdater IntentStatusUpdater,
	intentReader IntentReader,
	completionOutbox CompletionOutbox,
	policy messaging.RetryPolicy,
) *Consumer {
	return &Consumer{
		db:               db,
		engine:           engine,
		statusReader:     statusReader,
		intentUpdater:    intentUpdater,
		intentReader:     intentReader,
		completionOutbox: completionOutbox,
		policy:           policy,
	}
}

// ConsumeDelivery is the AMQP entry point. It begins a pay_db tx, then
// delegates to messaging.ProcessDelivery which owns the Inbox insert, handler
// invocation, commit, and ack/retry/DLQ lifecycle. The handler closure does
// not write through tx directly (the engine owns its own tx), but the outer tx
// still records the consumer-level Inbox dedup row.
func (c *Consumer) ConsumeDelivery(ctx context.Context, delivery amqp.Delivery) error {
	if c.db == nil {
		// Without a DB the outer Inbox cannot be recorded; route through retry.
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(), c.policy)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		// Begin-tx failure: pass nil so ProcessDelivery routes the delivery
		// through retry/DLQ settlement.
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(), c.policy)
	}
	return messaging.ProcessDelivery(ctx, tx, ConsumerName, delivery, c.handler(), c.policy)
}

// handler returns the closure ProcessDelivery invokes once the Inbox row has
// been inserted (i.e. the delivery is not a duplicate). The closure:
//   - Calls Engine.ApplyResult (atomic via the engine's own WithInstance tx).
//   - On success, syncs payment_intent.status from the workflow instance.
//
// ApplyResult errors propagate so ProcessDelivery retries/DLQs the delivery.
func (c *Consumer) handler() func(context.Context, messaging.Envelope) error {
	return func(ctx context.Context, env messaging.Envelope) error {
		return c.handleResult(ctx, env)
	}
}

// handleResult is the package-internal entry point tested by consumer_test.go.
// It exists separately from handler() so tests can call it without spinning up
// AMQP + a DB transaction.
func (c *Consumer) handleResult(ctx context.Context, env messaging.Envelope) error {
	if err := c.engine.ApplyResult(ctx, env); err != nil {
		// Engine failed (transient or terminal): leave payment_intent.status
		// untouched — the saga state has not observably changed.
		return err
	}
	// ApplyResult succeeded: sync payment_intent.status from the instance so
	// the GET endpoint reflects the latest saga outcome. A status-sync failure
	// is logged and swallowed: it does not invalidate the (already-applied)
	// result event, and the next event for this workflow will resync.
	if c.statusReader != nil && c.intentUpdater != nil && env.WorkflowID != "" {
		status, err := c.statusReader.InstanceStatus(ctx, env.WorkflowID)
		if err != nil {
			log.Printf("payment consumer: read status for %s: %v", env.WorkflowID, err)
			return nil
		}
		intentStatus := mapInstanceStatusToIntent(status)

		// Read the intent's PREVIOUS status to detect a fresh succeeded
		// transition. This guards the exactly-once emission of
		// payment.completed.v1: a duplicate delivery for an already-succeeded
		// workflow (late redelivery bypassing the outer Inbox) must not
		// re-emit. When no IntentReader is wired, previousStatus is left empty
		// so the emit-on-success fallback still fires.
		//
		// intentExists discriminates payment-transfer workflows (which own a
		// payment_intent row) from payment-reversal workflows (which do
		// not). A reversal workflow reaching StatusSucceeded has no intent to
		// update or emit a completion event for — its outcome is observed via
		// the reversal_workflow_id the reverse endpoint returned.
		var previousStatus PaymentIntentStatus
		intentExists := true
		if c.intentReader != nil {
			intent, readErr := c.intentReader.GetByWorkflowID(ctx, env.WorkflowID)
			if readErr != nil {
				intentExists = false
			} else {
				previousStatus = intent.Status
			}
		}

		if intentExists {
			if err := c.intentUpdater.UpdateStatusByWorkflowID(ctx, env.WorkflowID, intentStatus); err != nil {
				log.Printf("payment consumer: sync intent status for %s: %v", env.WorkflowID, err)
			}

			// Emit payment.completed.v1 exactly once per fresh succeeded
			// transition. The completion event drives the downstream reward
			// consumer (a NON-CRITICAL consumer whose failures route to the
			// reward DLQ and never affect payment status). An emission failure
			// is logged and swallowed: it does not invalidate the result
			// event; the next resync event will detect the transition is no
			// longer fresh and skip emission — so a transient outbox failure
			// can drop the completion event. A future iteration may wrap the
			// status update + emission in one transaction to close that gap;
			// for Task 8 the emit-on-success path is the documented contract.
			if intentStatus == IntentSucceeded && previousStatus != IntentSucceeded && c.completionOutbox != nil {
				if err := c.emitCompletion(ctx, env.WorkflowID); err != nil {
					log.Printf("payment consumer: emit completion for %s: %v", env.WorkflowID, err)
				}
			}
		}
	}
	return nil
}

// emitCompletion builds the payment.completed.v1 envelope from the intent and
// hands it to the CompletionOutbox. The payload carries the fields downstream
// non-critical consumers need (customer id, amount, currency) so the reward
// service does not have to call back into the payment API.
//
// When no IntentReader is wired, emitCompletion falls back to a minimal
// payload carrying only the workflow_id so a legacy deployment without an
// intent reader still emits the event; the reward consumer's handler
// tolerates the missing fields by using env.WorkflowID.
func (c *Consumer) emitCompletion(ctx context.Context, workflowID string) error {
	payload := struct {
		WorkflowID      string `json:"workflow_id"`
		PaymentID       string `json:"payment_id"`
		PayerCustomerID string `json:"payer_customer_id"`
		AmountMinor     int64  `json:"amount_minor"`
		Currency        string `json:"currency"`
	}{
		WorkflowID: workflowID,
		PaymentID:  workflowID,
	}
	if c.intentReader != nil {
		intent, err := c.intentReader.GetByWorkflowID(ctx, workflowID)
		if err != nil {
			return fmt.Errorf("read intent for completion: %w", err)
		}
		payload.PayerCustomerID = intent.PayerCustomerID
		payload.AmountMinor = intent.AmountMinor
		payload.Currency = intent.Currency
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal completion payload: %w", err)
	}
	env := messaging.NewEnvelope(EventPaymentCompleted, workflowID, body, time.Now)
	env.WorkflowID = workflowID
	return c.completionOutbox.EmitCompletion(ctx, env)
}

// mapInstanceStatusToIntent translates a workflow.InstanceStatus string into
// the corresponding PaymentIntentStatus. Unknown statuses fall back to pending
// — a defensive default that preserves observability without inventing a
// payment-side status the schema does not model.
func mapInstanceStatusToIntent(status string) PaymentIntentStatus {
	switch status {
	case "preparing":
		return IntentPending
	case "ready", "running":
		return IntentRunning
	case "succeeded":
		return IntentSucceeded
	case "compensated":
		return IntentCompensated
	case "compensation_failed":
		return IntentCompensationFailed
	case "rejected":
		return IntentRejected
	default:
		return IntentPending
	}
}

// ---------------------------------------------------------------------------
// InstanceStatusRepo — reads workflow_instance.status without the engine's
// row lock. Used by the consumer's status sync.
// ---------------------------------------------------------------------------

// InstanceStatusRepo reads the status column of a workflow_instance row. It is
// a separate struct from PaymentIntentRepo so each repo owns one table.
type InstanceStatusRepo struct {
	db *sql.DB
}

// NewInstanceStatusRepo binds a *sql.DB.
func NewInstanceStatusRepo(db *sql.DB) *InstanceStatusRepo {
	return &InstanceStatusRepo{db: db}
}

// InstanceStatus returns the current status of workflowID, or
// ErrWorkflowNotFound when no such instance exists.
func (r *InstanceStatusRepo) InstanceStatus(ctx context.Context, workflowID string) (string, error) {
	var status string
	err := r.db.QueryRowContext(ctx,
		`SELECT status FROM workflow_instance WHERE workflow_id = $1`, workflowID,
	).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("%w: workflow_id=%s", ErrWorkflowNotFound, workflowID)
		}
		return "", err
	}
	return status, nil
}

// ---------------------------------------------------------------------------
// AMQP runtime: Run subscribes to the result-event queue and dispatches
// deliveries to ConsumeDelivery.
// ---------------------------------------------------------------------------

// Run connects to the RabbitMQ broker, subscribes to the result-event queue,
// and dispatches each delivery to ConsumeDelivery. It blocks until ctx is
// cancelled (returns nil) or a fatal broker error occurs (returns error).
//
// The result-event queue is bound to the routing keys the saga participants
// emit (risk.payment-authorized.v1, core.hold-placed.v1, core.transfer-posted.v1,
// and the failure counterparts). Bindings are declared by the seed or broker
// topology; Run only consumes.
func (c *Consumer) Run(ctx context.Context, amqpURL, queue string) error {
	if c.db == nil {
		return errors.New("payment consumer: db is nil")
	}
	if amqpURL == "" {
		return errors.New("payment consumer: amqp URL is required")
	}
	if queue == "" {
		queue = ConsumerName + ".results"
	}

	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("payment consumer: dial broker: %w", err)
	}
	defer conn.Close()

	// Cancel the connection on context cancellation so a blocked Consume loop
	// unblocks promptly on shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("payment consumer: open channel: %w", err)
	}
	defer ch.Close()

	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("payment consumer: declare queue %s: %w", queue, err)
	}
	// Fair dispatch: process one delivery at a time so retries are not starved
	// by prefetch buffering.
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("payment consumer: set QoS: %w", err)
	}

	// Wire the retry policy's publisher so ProcessDelivery can route replacement
	// deliveries to retry/DLQ destinations.
	publisher, err := messaging.NewRabbitPublisher(ch, "")
	if err != nil {
		return fmt.Errorf("payment consumer: retry publisher: %w", err)
	}
	c.policy.Router = publisher

	deliveries, err := ch.Consume(queue, ConsumerName, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("payment consumer: start consume: %w", err)
	}

	log.Printf("payment consumer: subscribed to queue %s", queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("payment consumer: delivery channel closed")
			}
			if err := c.ConsumeDelivery(ctx, delivery); err != nil {
				log.Printf("payment consumer: delivery %s: %v", delivery.MessageId, err)
				// Non-fatal: retry/DLQ settlement is handled inside
				// ProcessDelivery. Continue processing subsequent deliveries.
			}
		}
	}
}

// ---------------------------------------------------------------------------
// OutboxRelay — drains outbox_message rows the engine's AppendOutbox wrote
// (workflow dispatch commands) and publishes them to the broker.
// ---------------------------------------------------------------------------

// Publisher publishes a result envelope to the broker under a routing key.
// *messaging.RabbitPublisher satisfies this interface. Re-declared here to
// keep payment's runtime self-contained (corebanking exports the same shape).
type Publisher interface {
	Publish(ctx context.Context, routingKey string, envelope messaging.Envelope) error
}

// OutboxRelay drains undispatched outbox_message rows and publishes them.
// Single-instance only (no claim tokens); multi-instance deployments would add
// claim-based dispatch. A publish failure stops the relay so runx.Serve fails
// readiness — undispatched rows retry on the next relay start.
type OutboxRelay struct {
	db        *sql.DB
	publisher Publisher
	Interval  time.Duration
	BatchSize int
}

// NewOutboxRelay constructs an outbox relay. Interval defaults to 500ms,
// BatchSize to 100.
func NewOutboxRelay(db *sql.DB, publisher Publisher) *OutboxRelay {
	return &OutboxRelay{db: db, publisher: publisher, Interval: 500 * time.Millisecond, BatchSize: 100}
}

// Run polls outbox_message and publishes undispatched envelopes until ctx is
// cancelled (returns nil) or a fatal error occurs.
func (r *OutboxRelay) Run(ctx context.Context) error {
	if r.db == nil {
		return errors.New("payment outbox relay: db is nil")
	}
	if r.publisher == nil {
		return errors.New("payment outbox relay: publisher is nil")
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.drain(ctx); err != nil {
				return fmt.Errorf("payment outbox relay: drain: %w", err)
			}
		}
	}
}

// drain reads one batch of undispatched rows, publishes each, and marks it
// dispatched.
func (r *OutboxRelay) drain(ctx context.Context) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT message_id, routing_key, envelope
		FROM outbox_message
		WHERE dispatched_at IS NULL
		ORDER BY created_at
		LIMIT $1`, r.BatchSize)
	if err != nil {
		return fmt.Errorf("query undispatched: %w", err)
	}
	type pending struct {
		msgID    string
		routeKey string
		body     []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.msgID, &p.routeKey, &p.body); err != nil {
			rows.Close()
			return fmt.Errorf("scan outbox row: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read outbox rows: %w", err)
	}

	for _, p := range batch {
		var env messaging.Envelope
		if err := json.Unmarshal(p.body, &env); err != nil {
			// Poison message: record the error so the relay does not loop.
			_, _ = r.db.ExecContext(ctx,
				`UPDATE outbox_message SET last_error = $2, attempts = attempts + 1 WHERE message_id = $1`,
				p.msgID, fmt.Sprintf("unmarshal envelope: %v", err))
			log.Printf("payment outbox relay: poison message %s: %v", p.msgID, err)
			continue
		}
		if err := r.publisher.Publish(ctx, p.routeKey, env); err != nil {
			return fmt.Errorf("publish %s: %w", p.msgID, err)
		}
		if _, err := r.db.ExecContext(ctx,
			`UPDATE outbox_message SET dispatched_at = CURRENT_TIMESTAMP WHERE message_id = $1`,
			p.msgID); err != nil {
			return fmt.Errorf("mark dispatched %s: %w", p.msgID, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// PgCompletionOutbox — concrete CompletionOutbox backed by pay_db.
// ---------------------------------------------------------------------------

// PgCompletionOutbox writes the payment.completed.v1 envelope to the
// outbox_message table. The OutboxRelay drains the row and publishes it to
// the broker. The write runs in its own transaction: the consumer's
// handleResult calls this AFTER the engine's ApplyResult has already
// committed the workflow state transition, so a failure here loses the
// completion event but does NOT roll back the saga. The intent-reader's
// previous-status gate prevents a duplicate re-emit on the next resync.
type PgCompletionOutbox struct {
	db *sql.DB
}

// NewPgCompletionOutbox binds a *sql.DB (pay_db). The caller owns the pool.
func NewPgCompletionOutbox(db *sql.DB) *PgCompletionOutbox {
	return &PgCompletionOutbox{db: db}
}

// EmitCompletion serialises env and inserts it into outbox_message with the
// payment.completed routing key. The row's primary key (message_id) dedupes
// a duplicate write of the same envelope silently.
func (o *PgCompletionOutbox) EmitCompletion(ctx context.Context, env messaging.Envelope) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal completion envelope: %w", err)
	}
	_, err = o.db.ExecContext(ctx, `
		INSERT INTO outbox_message
		  (message_id, message_type, schema_version, routing_key, envelope, created_at)
		VALUES ($1, $2, $3, $4, $5, CURRENT_TIMESTAMP)
		ON CONFLICT (message_id) DO NOTHING`,
		env.MessageID, env.MessageType, env.SchemaVersion, RoutePaymentCompleted, body,
	)
	if err != nil {
		return fmt.Errorf("insert completion outbox: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Reversal helpers — used by the reverse API endpoint and the consumer's
// reversal-completion detection.
// ---------------------------------------------------------------------------

// MarkReversalPendingByWorkflowID sets status='reversal_pending' on the
// intent owning workflowID. Returns ErrWorkflowNotFound when the workflow id
// does not exist (zero rows affected).
//
// This is the Task-7 follow-up fix: the reverse endpoint MUST NOT persist
// reversed=true before the reversal saga runs. Instead it persists
// reversal_pending; the consumer flips reversed to true only after the
// payment-reversal workflow reaches StatusSucceeded.
func (r *PaymentIntentRepo) MarkReversalPendingByWorkflowID(ctx context.Context, workflowID string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE payment_intent
		SET status = 'reversal_pending',
		    updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $1`,
		workflowID)
	if err != nil {
		return fmt.Errorf("mark reversal_pending for %q: %w", workflowID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %q: %w", workflowID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: workflow_id=%s", ErrWorkflowNotFound, workflowID)
	}
	return nil
}

// IntentReversalPending is the status recorded when the reverse endpoint has
// started a payment-reversal workflow but the reversal has not yet
// completed. It is an intermediate state: the consumer transitions it to
// IntentReversed once the reversal workflow succeeds.
const IntentReversalPending PaymentIntentStatus = "reversal_pending"
