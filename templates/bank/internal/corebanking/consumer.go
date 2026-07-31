// Package corebanking wires the transactional command consumer for the
// core-banking service.
//
// The consumer decodes place-hold, release-hold, post-held-transfer, and
// reverse-transfer command envelopes, inserts the Inbox record, calls the
// appropriate domain service, and — for place-hold and release-hold — writes
// the result event to the Outbox, all within a single core-DB transaction.
// PostHeldTransfer and ReverseTransfer emit their own success events from
// within the service's transaction; the consumer only emits failure events
// for those command types. Duplicate commands are deduplicated by the Inbox
// so no second domain mutation or event is produced.
//
// ErrorClass mapping:
//   - Unknown schema/command type or payload decode failure → invalid_message.
//   - Domain invariant failure (hold state, mismatch, not-found) →
//     invariant_violation.
//   - Insufficient funds → business_rejected.
//   - Database/broker failure → transient_failure (returned for retry, no
//     event emitted).
//
// Terminal classes (invalid_message, invariant_violation, business_rejected)
// emit a failure result event and ack the delivery so the saga engine can
// react; the transient class returns the raw error so messaging.ProcessDelivery
// retries and eventually DLQs the message.
package corebanking

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/testfail"
	"bank/internal/platform/workflow"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Command message types consumed by this service.
const (
	CmdPlaceHold        = "core.place-hold.v1"
	CmdReleaseHold      = "core.release-hold.v1"
	CmdPostHeldTransfer = "core.post-held-transfer.v1"
	CmdReverseTransfer  = "core.reverse-transfer.v1"
)

// Result event types produced by the consumer.
//
// For place-hold and release-hold the consumer emits the success/failure
// event directly. For post-held-transfer and reverse-transfer the service
// emits the success event (service.EventTransferPosted / EventTransferReversed)
// from within its own transaction; the consumer only emits the matching
// failure event when the service call fails.
const (
	EventHoldPlaced            = "core.hold-placed.v1"
	EventHoldFailed            = "core.hold-failed.v1"
	EventHoldReleased          = "core.hold-released.v1"
	EventHoldReleaseFailed     = "core.hold-release-failed.v1"
	EventTransferFailed        = "core.transfer-failed.v1"
	EventTransferReverseFailed = "core.transfer-reverse-failed.v1"
	EventCommandRejected       = "core.command-rejected.v1"
)

// Result routing keys published to the outbox.
const (
	RouteHoldPlaced            = "core.hold.placed"
	RouteHoldFailed            = "core.hold.failed"
	RouteHoldReleased          = "core.hold.released"
	RouteHoldReleaseFailed     = "core.hold.release_failed"
	RouteTransferFailed        = "core.transfer.failed"
	RouteTransferReverseFailed = "core.transfer.reverse_failed"
	RouteCommandRejected       = "core.command.rejected"
)

// ConsumerName is the Inbox consumer identifier used for deduplication.
const ConsumerName = "core-banking"

// HoldCommander is the subset of the hold service used by the consumer.
// *service.HoldService satisfies this interface.
type HoldCommander interface {
	PlaceHold(ctx context.Context, in domain.PlaceHoldInput) (domain.Hold, error)
	ReleaseHold(ctx context.Context, holdID, idempotencyKey string) (domain.Hold, error)
}

// TransferCommander is the subset of the held-transfer service used by the
// consumer. *service.HeldTransferService satisfies this interface.
type TransferCommander interface {
	PostHeldTransfer(ctx context.Context, in service.PostHeldTransfer) (domain.Booking, error)
	ReverseTransfer(ctx context.Context, in service.ReverseTransfer) (domain.Booking, error)
}

// OutboxWriter appends a result envelope to the outbox within the consumer's
// transaction. *repo.LedgerRepo satisfies this interface.
type OutboxWriter interface {
	AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error
}

// Consumer processes core-banking commands transactionally.
type Consumer struct {
	db        *sql.DB
	holds     HoldCommander
	transfers TransferCommander
	outbox    OutboxWriter
	policy    messaging.RetryPolicy
	now       func() time.Time
}

// NewConsumer constructs a core-banking command consumer. db is the
// transaction boundary for the Inbox insert and consumer-level Outbox writes;
// it may be nil in unit tests that exercise processEnvelope directly. holds
// and transfers are the domain services. outbox writes result events for
// place-hold/release-hold (and failure events for transfers). policy is the
// retry/DLQ settlement policy used by messaging.ProcessDelivery.
func NewConsumer(
	db *sql.DB,
	holds HoldCommander,
	transfers TransferCommander,
	outbox OutboxWriter,
	policy messaging.RetryPolicy,
	now func() time.Time,
) *Consumer {
	if now == nil {
		now = time.Now
	}
	return &Consumer{db: db, holds: holds, transfers: transfers, outbox: outbox, policy: policy, now: now}
}

// ConsumeDelivery is the AMQP entry point. It begins a core-DB transaction,
// then delegates to messaging.ProcessDelivery which owns the Inbox insert,
// handler invocation, commit, and ack/retry/DLQ lifecycle. The handler
// closure captures the tx so consumer-level Outbox writes commit atomically
// with the Inbox.
func (c *Consumer) ConsumeDelivery(ctx context.Context, delivery amqp.Delivery) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		// When the transaction cannot begin, pass nil so ProcessDelivery
		// routes the delivery through retry/DLQ settlement.
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(nil), c.policy)
	}
	return messaging.ProcessDelivery(ctx, tx, ConsumerName, delivery, c.handler(tx), c.policy)
}

// handler returns a closure that captures the transaction (which may be nil on
// begin-tx failure) and processes a decoded envelope.
func (c *Consumer) handler(tx *sql.Tx) func(context.Context, messaging.Envelope) error {
	return func(ctx context.Context, env messaging.Envelope) error {
		return c.processEnvelope(ctx, tx, env)
	}
}

// processEnvelope dispatches the envelope to the appropriate handler based on
// message type. All consumer-level outbox writes (if any) use the same tx.
// Unknown command types emit an invalid_message failure event (terminal) so the
// saga engine can react, and return nil so ProcessDelivery acks the delivery.
func (c *Consumer) processEnvelope(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	switch env.MessageType {
	case CmdPlaceHold:
		return c.handlePlaceHold(ctx, q, env)
	case CmdReleaseHold:
		return c.handleReleaseHold(ctx, q, env)
	case CmdPostHeldTransfer:
		return c.handlePostHeldTransfer(ctx, q, env)
	case CmdReverseTransfer:
		return c.handleReverseTransfer(ctx, q, env)
	default:
		return c.emitFailure(ctx, q, env, EventCommandRejected, RouteCommandRejected,
			workflow.InvalidMessage, fmt.Sprintf("unknown command type %q", env.MessageType))
	}
}

// ---------------------------------------------------------------------------
// Command handlers
// ---------------------------------------------------------------------------

// handlePlaceHold decodes a place-hold command, calls HoldService.PlaceHold,
// and writes the result event (hold-placed or hold-failed) to the Outbox.
func (c *Consumer) handlePlaceHold(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload placeHoldPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return c.emitFailure(ctx, q, env, EventHoldFailed, RouteHoldFailed,
			workflow.InvalidMessage, fmt.Sprintf("decode %s payload: %v", env.MessageType, err))
	}

	// Test-only smoke gate: smoke-insuff workflows report an
	// insufficient-available-balance failure without touching the ledger,
	// so the saga classifies it as business_rejected (terminal) and
	// compensates by voiding the authorization. Inert in production.
	if testfail.IsInsuff(env.WorkflowID) {
		return c.emitFailure(ctx, q, env, EventHoldFailed, RouteHoldFailed,
			workflow.BusinessRejected, "smoke: insufficient available balance")
	}

	hold, err := c.holds.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: payloadIdempotencyKey(env, payload.IdempotencyKey),
		AccountNo:      payload.AccountNo,
		Amount:         domain.NewMoneyFromCents(payload.AmountCents),
		Ccy:            payload.Currency,
		WorkflowID:     env.WorkflowID,
		ExpiresAt:      payload.expiresAt(),
	})
	if err != nil {
		return c.handleServiceError(ctx, q, env, err, EventHoldFailed, RouteHoldFailed)
	}
	return c.appendHoldPlaced(ctx, q, env, hold)
}

// handleReleaseHold decodes a release-hold command, calls
// HoldService.ReleaseHold, and writes the result event (hold-released or
// hold-release-failed) to the Outbox.
func (c *Consumer) handleReleaseHold(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload releaseHoldPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return c.emitFailure(ctx, q, env, EventHoldReleaseFailed, RouteHoldReleaseFailed,
			workflow.InvalidMessage, fmt.Sprintf("decode %s payload: %v", env.MessageType, err))
	}

	// Test-only smoke gate: smoke-compfail workflows report a transient
	// release-hold failure on every compensation attempt. The saga retries
	// the compensation up to CompensationMaxAttempts (default 5) and then
	// transitions the instance to compensation_failed. Inert in production.
	if testfail.IsCompFail(env.WorkflowID) {
		return c.emitFailure(ctx, q, env, EventHoldReleaseFailed, RouteHoldReleaseFailed,
			workflow.TransientFailure, "smoke: release-hold compensation failed")
	}

	hold, err := c.holds.ReleaseHold(ctx, payload.HoldID, payloadIdempotencyKey(env, payload.IdempotencyKey))
	if err != nil {
		return c.handleServiceError(ctx, q, env, err, EventHoldReleaseFailed, RouteHoldReleaseFailed)
	}
	return c.appendHoldReleased(ctx, q, env, hold)
}

// handlePostHeldTransfer decodes a post-held-transfer command and calls
// HeldTransferService.PostHeldTransfer. On success the service has already
// emitted core.transfer-posted.v1 from within its own transaction, so the
// consumer emits nothing. On terminal failure the consumer emits
// core.transfer-failed.v1 so the saga engine can react.
func (c *Consumer) handlePostHeldTransfer(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload postHeldTransferPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return c.emitFailure(ctx, q, env, EventTransferFailed, RouteTransferFailed,
			workflow.InvalidMessage, fmt.Sprintf("decode %s payload: %v", env.MessageType, err))
	}

	// Test-only smoke gate: smoke-transient workflows report a terminal
	// business_rejected transfer failure (representing a transient ledger
	// fault that the saga cannot retry past) so the engine triggers
	// compensation and releases the previously placed hold. The saga ends
	// in compensated with the hold released. Inert in production.
	if testfail.IsTransient(env.WorkflowID) {
		return c.emitFailure(ctx, q, env, EventTransferFailed, RouteTransferFailed,
			workflow.BusinessRejected, "smoke: transfer posting failed (transient)")
	}

	// Test-only smoke gate: smoke-compfail workflows fail the forward
	// post-held-transfer terminally (business_rejected) so the saga enters
	// compensation. The release-hold compensation then fails transiently
	// on every attempt (see handleReleaseHold), exhausting
	// CompensationMaxAttempts and transitioning the instance to
	// compensation_failed. Inert in production.
	if testfail.IsCompFail(env.WorkflowID) {
		return c.emitFailure(ctx, q, env, EventTransferFailed, RouteTransferFailed,
			workflow.BusinessRejected, "smoke: transfer posting failed (compfail)")
	}

	_, err := c.transfers.PostHeldTransfer(ctx, service.PostHeldTransfer{
		IdempotencyKey: payloadIdempotencyKey(env, payload.IdempotencyKey),
		HoldID:         payload.HoldID,
		FromAccount:    payload.FromAccount,
		ToAccount:      payload.ToAccount,
		Amount:         domain.NewMoneyFromCents(payload.AmountCents),
		Ccy:            payload.Currency,
		Summary:        payload.Summary,
		SagaRouting: service.SagaRouting{
			WorkflowID:       env.WorkflowID,
			ActionName:       env.ActionName,
			CommandID:        env.CommandID,
			CorrelationID:    env.CorrelationID,
			CommandMessageID: env.MessageID,
		},
	})
	if err != nil {
		return c.handleServiceError(ctx, q, env, err, EventTransferFailed, RouteTransferFailed)
	}
	// Success: the service emitted core.transfer-posted.v1 in its own tx.
	return nil
}

// handleReverseTransfer decodes a reverse-transfer command and calls
// HeldTransferService.ReverseTransfer. On success the service has already
// emitted core.transfer-reversed.v1 from within its own transaction, so the
// consumer emits nothing. On terminal failure the consumer emits
// core.transfer-reverse-failed.v1.
func (c *Consumer) handleReverseTransfer(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload reverseTransferPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return c.emitFailure(ctx, q, env, EventTransferReverseFailed, RouteTransferReverseFailed,
			workflow.InvalidMessage, fmt.Sprintf("decode %s payload: %v", env.MessageType, err))
	}
	_, err := c.transfers.ReverseTransfer(ctx, service.ReverseTransfer{
		IdempotencyKey:    payloadIdempotencyKey(env, payload.IdempotencyKey),
		OriginalVoucherNo: payload.OriginalVoucherNo,
		Summary:           payload.Summary,
		SagaRouting: service.SagaRouting{
			WorkflowID:       env.WorkflowID,
			ActionName:       env.ActionName,
			CommandID:        env.CommandID,
			CorrelationID:    env.CorrelationID,
			CommandMessageID: env.MessageID,
		},
	})
	if err != nil {
		return c.handleServiceError(ctx, q, env, err, EventTransferReverseFailed, RouteTransferReverseFailed)
	}
	// Success: the service emitted core.transfer-reversed.v1 in its own tx.
	return nil
}

// handleServiceError classifies a service error and either emits a terminal
// failure event (invalid_message, invariant_violation, business_rejected) or
// returns the raw error for transient failures so ProcessDelivery retries.
func (c *Consumer) handleServiceError(
	ctx context.Context, q pg.DBTX, env messaging.Envelope,
	err error, eventType, routingKey string,
) error {
	class := classifyError(err)
	if class == workflow.TransientFailure {
		// Transient: do not emit an event — let ProcessDelivery retry/DLQ.
		return err
	}
	return c.emitFailure(ctx, q, env, eventType, routingKey, class, err.Error())
}

// ---------------------------------------------------------------------------
// ErrorClass classification
// ---------------------------------------------------------------------------

// classifyError maps a domain/service error to the workflow engine's
// ErrorClass so the saga engine's ApplyResult can decide whether to retry,
// compensate, or reject the instance.
func classifyError(err error) workflow.ErrorClass {
	if err == nil {
		return workflow.TransientFailure
	}
	switch {
	// Insufficient funds → business_rejected (terminal, triggers compensation).
	case errors.Is(err, service.ErrInsufficientAvailableBalance),
		errors.Is(err, service.ErrInsufficientBalance):
		return workflow.BusinessRejected

	// Domain invariant failures → invariant_violation (terminal).
	case errors.Is(err, domain.ErrHoldCaptured),
		errors.Is(err, domain.ErrHoldReleased),
		errors.Is(err, domain.ErrInvalidHoldTransition),
		errors.Is(err, service.ErrHoldNotActive),
		errors.Is(err, service.ErrHoldAmountMismatch),
		errors.Is(err, service.ErrHoldCcyMismatch),
		errors.Is(err, service.ErrHoldAccountMismatch),
		errors.Is(err, service.ErrHoldNotFound),
		errors.Is(err, service.ErrNonPositiveHoldAmount),
		errors.Is(err, service.ErrHeldTransferNotFound),
		errors.Is(err, service.ErrOriginalVoucherNotFound),
		errors.Is(err, service.ErrVoucherAlreadyReversed),
		errors.Is(err, service.ErrAccountNotFound),
		errors.Is(err, service.ErrAccountNotActive),
		errors.Is(err, service.ErrCcyMismatch):
		return workflow.InvariantViolation

	// Everything else (DB/broker) → transient_failure (retryable).
	default:
		return workflow.TransientFailure
	}
}

// ---------------------------------------------------------------------------
// Result event builders
// ---------------------------------------------------------------------------

// appendHoldPlaced builds and appends the core.hold-placed.v1 success event.
func (c *Consumer) appendHoldPlaced(ctx context.Context, q pg.DBTX, cmdEnv messaging.Envelope, hold domain.Hold) error {
	env, err := c.buildHoldPlacedEnvelope(ctx, q, cmdEnv, hold)
	if err != nil {
		return err
	}
	return c.outbox.AppendOutbox(ctx, q, env, RouteHoldPlaced)
}

// buildHoldPlacedEnvelope constructs the hold-placed result envelope.
func (c *Consumer) buildHoldPlacedEnvelope(_ context.Context, _ pg.DBTX, cmdEnv messaging.Envelope, hold domain.Hold) (messaging.Envelope, error) {
	payload := holdPlacedPayload{
		HoldID:      hold.HoldID,
		AccountNo:   hold.AccountNo,
		AmountCents: hold.Amount.Cents(),
		Currency:    hold.Ccy,
		WorkflowID:  hold.WorkflowID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("marshal hold-placed payload: %w", err)
	}
	return makeResultEnvelope(cmdEnv, EventHoldPlaced, body, c.now()), nil
}

// appendHoldReleased builds and appends the core.hold-released.v1 success event.
func (c *Consumer) appendHoldReleased(ctx context.Context, q pg.DBTX, cmdEnv messaging.Envelope, hold domain.Hold) error {
	payload := holdReleasedPayload{
		HoldID:     hold.HoldID,
		AccountNo:  hold.AccountNo,
		WorkflowID: hold.WorkflowID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal hold-released payload: %w", err)
	}
	env := makeResultEnvelope(cmdEnv, EventHoldReleased, body, c.now())
	return c.outbox.AppendOutbox(ctx, q, env, RouteHoldReleased)
}

// emitFailure builds and appends a failure result event carrying the
// ErrorClass so the saga engine can react (compensate or reject).
func (c *Consumer) emitFailure(
	ctx context.Context, q pg.DBTX, cmdEnv messaging.Envelope,
	eventType, routingKey string, class workflow.ErrorClass, message string,
) error {
	payload := failurePayload{
		ErrorClass:   string(class),
		ErrorMessage: message,
		WorkflowID:   cmdEnv.WorkflowID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	env := makeResultEnvelope(cmdEnv, eventType, body, c.now())
	return c.outbox.AppendOutbox(ctx, q, env, routingKey)
}

// makeResultEnvelope stamps the correlation/causation fields that link the
// result back to the originating command and saga. MessageID and
// SchemaVersion are generated by NewEnvelope; workflow-specific fields are
// propagated from the command envelope.
func makeResultEnvelope(cmdEnv messaging.Envelope, messageType string, payload json.RawMessage, now time.Time) messaging.Envelope {
	env := messaging.NewEnvelope(messageType, cmdEnv.CorrelationID, payload, func() time.Time {
		return now.UTC()
	})
	env.WorkflowID = cmdEnv.WorkflowID
	env.ActionName = cmdEnv.ActionName
	env.CommandID = cmdEnv.CommandID
	env.IdempotencyKey = cmdEnv.IdempotencyKey
	env.CausationID = cmdEnv.MessageID
	return env
}

// ---------------------------------------------------------------------------
// Wire payload structs
// ---------------------------------------------------------------------------

type placeHoldPayload struct {
	AccountNo     string `json:"account_no"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	AmountCents   int64  `json:"amount_cents"`
	Currency      string `json:"currency"`
	ExpiresAt     string `json:"expires_at,omitempty"` // RFC3339; empty = no expiry
}

// expiresAt parses the RFC3339 timestamp; a zero value is returned for empty.
func (p placeHoldPayload) expiresAt() time.Time {
	if p.ExpiresAt == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return time.Time{}
	}
	return t
}

type releaseHoldPayload struct {
	HoldID       string `json:"hold_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type postHeldTransferPayload struct {
	HoldID         string `json:"hold_id"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	FromAccount    string `json:"from_account"`
	ToAccount      string `json:"to_account"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	Summary        string `json:"summary,omitempty"`
}

type reverseTransferPayload struct {
	IdempotencyKey    string `json:"idempotency_key,omitempty"`
	OriginalVoucherNo string `json:"original_voucher_no"`
	Summary           string `json:"summary,omitempty"`
}

// payloadIdempotencyKey falls back to the envelope's IdempotencyKey when the
// payload omits one, so the saga engine can set it at the envelope level.
func payloadIdempotencyKey(env messaging.Envelope, fromPayload string) string {
	if fromPayload != "" {
		return fromPayload
	}
	return env.IdempotencyKey
}

// Result event payloads (wire format).

type holdPlacedPayload struct {
	HoldID      string `json:"hold_id"`
	AccountNo   string `json:"account_no,omitempty"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency,omitempty"`
	WorkflowID  string `json:"workflow_id,omitempty"`
}

type holdReleasedPayload struct {
	HoldID     string `json:"hold_id"`
	AccountNo  string `json:"account_no,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

type failurePayload struct {
	ErrorClass   string `json:"error_class"`
	ErrorMessage string `json:"error_message,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
}

// ---------------------------------------------------------------------------
// AMQP runtime: Run subscribes to the command queue and dispatches deliveries
// ---------------------------------------------------------------------------

// Run connects to the RabbitMQ broker, subscribes to the core-banking command
// queue, and dispatches each delivery to ConsumeDelivery. It blocks until ctx
// is cancelled (returns nil) or a fatal broker error occurs (returns error).
// The retry policy's Router is wired from the broker channel so
// messaging.ProcessDelivery can route retries and DLQ entries.
func (c *Consumer) Run(ctx context.Context, amqpURL, queue string) error {
	if c.db == nil {
		return errors.New("corebanking consumer: db is nil")
	}
	if amqpURL == "" {
		return errors.New("corebanking consumer: amqp URL is required")
	}
	if queue == "" {
		queue = ConsumerName + ".commands"
	}

	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("corebanking consumer: dial broker: %w", err)
	}
	defer conn.Close()

	// Close the connection when the context is cancelled so a blocked
	// Consume loop unblocks promptly on shutdown.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("corebanking consumer: open channel: %w", err)
	}
	defer ch.Close()

	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("corebanking consumer: declare queue %s: %w", queue, err)
	}
	// Fair dispatch: process one delivery at a time so retries are not
	// starved by prefetch buffering.
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("corebanking consumer: set QoS: %w", err)
	}

	// Wire the retry policy's publisher so ProcessDelivery can route
	// replacement deliveries to retry/DLQ destinations. The publisher's
	// exchange is unused for retry routing (Route is called with the
	// RetryPolicy's explicit RetryExchange/DeadLetterExchange); bank.commands
	// is set as the semantically meaningful default for a command consumer.
	publisher, err := messaging.NewRabbitPublisher(ch, messaging.ExchangeCommands)
	if err != nil {
		return fmt.Errorf("corebanking consumer: retry publisher: %w", err)
	}
	c.policy.Router = publisher

	deliveries, err := ch.Consume(queue, ConsumerName, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("corebanking consumer: start consume: %w", err)
	}

	log.Printf("corebanking consumer: subscribed to queue %s", queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("corebanking consumer: delivery channel closed")
			}
			if err := c.ConsumeDelivery(ctx, delivery); err != nil {
				log.Printf("corebanking consumer: delivery %s: %v", delivery.MessageId, err)
				// Non-fatal: retry/DLQ settlement is handled inside
				// ProcessDelivery. Continue processing subsequent deliveries.
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Outbox relay: drains outbox_message rows and publishes them to the broker
// ---------------------------------------------------------------------------

// Publisher publishes a result envelope to the broker under the given exchange
// and routing key. *messaging.RabbitPublisher satisfies this interface.
type Publisher interface {
	PublishTo(ctx context.Context, exchange, routingKey string, envelope messaging.Envelope) error
}

// OutboxRelay drains undispatched outbox_message rows and publishes them to the
// broker via a Publisher. Single-instance only (no claim tokens); for
// multi-instance deployments, add claim-based dispatch (UPDATE ... SET
// claim_token = ... WHERE claim_token IS NULL RETURNING ...).
//
// The relay polls on a fixed interval. Each batch reads up to BatchSize
// undispatched rows ordered by created_at, publishes each envelope, and marks
// it dispatched. A publish failure stops the relay (returns the error) so
// runx.Serve can fail readiness — the rows remain undispatched and will retry
// on the next relay start.
type OutboxRelay struct {
	db        *sql.DB
	publisher Publisher
	Interval  time.Duration
	BatchSize int
}

// NewOutboxRelay constructs an outbox relay. interval defaults to 500ms,
// batchSize to 100.
func NewOutboxRelay(db *sql.DB, publisher Publisher) *OutboxRelay {
	return &OutboxRelay{db: db, publisher: publisher, Interval: 500 * time.Millisecond, BatchSize: 100}
}

// Run polls outbox_message and publishes undispatched envelopes until ctx is
// cancelled (returns nil) or a fatal error occurs.
func (r *OutboxRelay) Run(ctx context.Context) error {
	if r.db == nil {
		return errors.New("outbox relay: db is nil")
	}
	if r.publisher == nil {
		return errors.New("outbox relay: publisher is nil")
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.drain(ctx); err != nil {
				return fmt.Errorf("outbox relay: drain: %w", err)
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
			// Poison message: mark with error so the relay doesn't loop on it.
			_, _ = r.db.ExecContext(ctx,
				`UPDATE outbox_message SET last_error = $2, attempts = attempts + 1 WHERE message_id = $1`,
				p.msgID, fmt.Sprintf("unmarshal envelope: %v", err))
			log.Printf("outbox relay: poison message %s: %v", p.msgID, err)
			continue
		}
		// Derive the topic exchange from the routing key so result events
		// (core.hold.*, core.transfer.*, *.command.rejected) route through
		// bank.events and any versioned command through bank.commands. The
		// core-banking outbox only holds result events today, but deriving
		// per row keeps the relay robust if a command is ever appended here.
		exchange := messaging.ExchangeForRoutingKey(p.routeKey)
		if err := r.publisher.PublishTo(ctx, exchange, p.routeKey, env); err != nil {
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
