// Package risk wires the transactional command consumer for the risk service.
//
// The consumer decodes risk.authorize-payment.v1 and
// risk.void-payment-authorization.v1 command envelopes, inserts the Inbox
// record, calls the AuthorizationService, and writes the result event to the
// Outbox — all within a single risk-DB transaction. Duplicate commands are
// deduplicated by the Inbox so no second authorization or event is produced.
package risk

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/workflow"
	"bank/internal/risk/service"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Command message types consumed by this service.
const (
	CmdAuthorizePayment  = "risk.authorize-payment.v1"
	CmdVoidAuthorization = "risk.void-payment-authorization.v1"
)

// Result routing keys published to the outbox.
const (
	RoutePaymentAuthorized          = "risk.payment.authorized"
	RoutePaymentRejected            = "risk.payment.rejected"
	RoutePaymentAuthorizationVoided = "risk.payment.authorization.voided"
	RouteCommandRejected            = "risk.command.rejected"
)

// Result event types produced by the consumer.
const (
	// EventCommandRejected is emitted for unknown / undecodable command types
	// so the saga engine can cleanly terminate the instance with an
	// invalid_message classification. Mirrors corebanking's pattern.
	EventCommandRejected = "risk.command-rejected.v1"
)

// OutboxWriter appends a result envelope to the outbox within the consumer's
// transaction. The concrete risk-DB repository satisfies this interface; a
// stub is used in unit tests. Mirrors corebanking.OutboxWriter.
type OutboxWriter interface {
	AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error
}

// ConsumerName is the Inbox consumer identifier used for deduplication.
const ConsumerName = "risk"

// Consumer processes risk authorization commands transactionally.
type Consumer struct {
	db      *sql.DB
	service *service.AuthorizationService
	outbox  OutboxWriter
	policy  messaging.RetryPolicy
	now     func() time.Time
}

// NewConsumer constructs a Consumer. The db must point at the risk database
// (which contains both inbox_message/outbox_message and payment_authorization).
// outbox appends result events to the outbox within the consumer's tx.
func NewConsumer(db *sql.DB, svc *service.AuthorizationService, outbox OutboxWriter, policy messaging.RetryPolicy, now func() time.Time) *Consumer {
	if now == nil {
		now = time.Now
	}
	return &Consumer{db: db, service: svc, outbox: outbox, policy: policy, now: now}
}

// ConsumeDelivery is the AMQP entry point. It begins a risk-DB transaction,
// then delegates to messaging.ProcessDelivery which owns the Inbox insert,
// handler invocation, commit, and ack/retry/DLQ lifecycle. The handler closure
// captures the tx so domain and Outbox writes commit atomically with the Inbox.
func (c *Consumer) ConsumeDelivery(ctx context.Context, delivery amqp.Delivery) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		// When the transaction cannot begin, pass nil so ProcessDelivery
		// routes the delivery through retry/DLQ settlement.
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(nil), c.policy)
	}
	handler := c.handler(tx)
	return messaging.ProcessDelivery(ctx, tx, ConsumerName, delivery, handler, c.policy)
}

// handler returns a closure that captures the transaction (which may be nil on
// begin-tx failure) and processes a decoded envelope.
func (c *Consumer) handler(tx *sql.Tx) func(context.Context, messaging.Envelope) error {
	return func(ctx context.Context, env messaging.Envelope) error {
		return c.processEnvelope(ctx, tx, env)
	}
}

// processEnvelope dispatches the envelope to the authorize or void handler
// based on message type. All database writes (domain + Outbox) use the same tx.
// Unknown command types emit an invalid_message failure event (terminal) so the
// saga engine can cleanly terminate the instance, and return nil so
// ProcessDelivery acks the delivery. This mirrors corebanking's pattern.
func (c *Consumer) processEnvelope(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	switch env.MessageType {
	case CmdAuthorizePayment:
		return c.handleAuthorize(ctx, q, env)
	case CmdVoidAuthorization:
		return c.handleVoid(ctx, q, env)
	default:
		return c.emitFailure(ctx, q, env,
			fmt.Sprintf("unknown message type %q", env.MessageType))
	}
}

// handleAuthorize decodes an authorize-payment command, calls the service, and
// writes the result event (authorized or rejected) to the Outbox. Duplicate
// commands (Inbox dedup) never reach this method; duplicate idempotency keys
// return Duplicate=true and skip the Outbox write.
func (c *Consumer) handleAuthorize(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload authorizePaymentPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", env.MessageType, err)
	}
	cmd := service.AuthorizeCommand{
		AuthorizationID: payload.AuthorizationID,
		WorkflowID:      env.WorkflowID,
		IdempotencyKey:  payloadIdempotencyKey(env, payload.IdempotencyKey),
		CustomerID:      payload.CustomerID,
		AmountCents:     payload.AmountCents,
		Currency:        payload.Currency,
	}
	result, err := c.service.AuthorizePayment(ctx, q, cmd)
	if err != nil {
		return fmt.Errorf("authorize payment: %w", err)
	}
	if result.Duplicate {
		return nil
	}
	resultEnv, routingKey, err := buildAuthorizeResultEnvelope(env, result, c.now())
	if err != nil {
		return err
	}
	return c.outbox.AppendOutbox(ctx, q, resultEnv, routingKey)
}

// handleVoid decodes a void-payment-authorization command, calls the service,
// and writes the voided result event to the Outbox. If the authorization is
// already voided the service returns Duplicate=true and no event is emitted.
func (c *Consumer) handleVoid(ctx context.Context, q pg.DBTX, env messaging.Envelope) error {
	var payload voidAuthorizationPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", env.MessageType, err)
	}
	cmd := service.VoidCommand{
		AuthorizationID: payload.AuthorizationID,
		WorkflowID:      env.WorkflowID,
		IdempotencyKey:  payloadIdempotencyKey(env, payload.IdempotencyKey),
	}
	result, err := c.service.VoidAuthorization(ctx, q, cmd)
	if err != nil {
		return fmt.Errorf("void authorization: %w", err)
	}
	if result.Duplicate {
		return nil
	}
	resultEnv := buildVoidResultEnvelope(env, result, c.now())
	return c.outbox.AppendOutbox(ctx, q, resultEnv, RoutePaymentAuthorizationVoided)
}

// ---------------------------------------------------------------------------
// Command payloads (wire format).
// ---------------------------------------------------------------------------

type authorizePaymentPayload struct {
	AuthorizationID string `json:"authorization_id"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	CustomerID      string `json:"customer_id"`
	AmountCents     int64  `json:"amount_cents"`
	Currency        string `json:"currency"`
}

type voidAuthorizationPayload struct {
	AuthorizationID string `json:"authorization_id"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
}

// payloadIdempotencyKey falls back to the envelope's IdempotencyKey when the
// payload omits one, so the saga engine can set it at the envelope level.
func payloadIdempotencyKey(env messaging.Envelope, fromPayload string) string {
	if fromPayload != "" {
		return fromPayload
	}
	return env.IdempotencyKey
}

// ---------------------------------------------------------------------------
// Result event builders.
// ---------------------------------------------------------------------------

// buildAuthorizeResultEnvelope constructs the result envelope and routing key
// for an authorize command outcome.
func buildAuthorizeResultEnvelope(cmdEnv messaging.Envelope, result service.AuthorizeResult, now time.Time) (messaging.Envelope, string, error) {
	auth := result.Authorization
	var routingKey string
	switch result.EventType {
	case service.AuthorizeEventRejected:
		routingKey = RoutePaymentRejected
	default:
		routingKey = RoutePaymentAuthorized
	}
	payload := authorizeResultPayload{
		AuthorizationID: auth.AuthorizationID,
		WorkflowID:      auth.WorkflowID,
		CustomerID:      auth.CustomerID,
		AmountCents:     auth.AmountCents,
		Currency:        auth.Currency,
		MatchedRules:    auth.MatchedRuleIDs,
		ContextDigest:   auth.ContextDigest,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return messaging.Envelope{}, "", fmt.Errorf("marshal authorize result payload: %w", err)
	}
	return makeResultEnvelope(cmdEnv, result.EventType, body, now), routingKey, nil
}

// buildVoidResultEnvelope constructs the result envelope for a void outcome.
func buildVoidResultEnvelope(cmdEnv messaging.Envelope, result service.VoidResult, now time.Time) messaging.Envelope {
	auth := result.Authorization
	payload := voidResultPayload{
		AuthorizationID: auth.AuthorizationID,
		WorkflowID:      auth.WorkflowID,
	}
	body, _ := json.Marshal(payload)
	return makeResultEnvelope(cmdEnv, result.EventType, body, now)
}

// makeResultEnvelope stamps the correlation/causation fields that link the
// result back to the originating command and saga. MessageID and SchemaVersion
// are generated by NewEnvelope; workflow-specific fields are propagated from
// the command envelope.
func makeResultEnvelope(cmdEnv messaging.Envelope, messageType string, payload json.RawMessage, now time.Time) messaging.Envelope {
	env := messaging.NewEnvelope(messageType, cmdEnv.CorrelationID, payload, func() time.Time {
		return now.UTC()
	})
	env.WorkflowID = cmdEnv.WorkflowID
	env.ActionName = cmdEnv.ActionName
	env.IdempotencyKey = cmdEnv.IdempotencyKey
	env.CausationID = cmdEnv.MessageID
	return env
}

type authorizeResultPayload struct {
	AuthorizationID string   `json:"authorization_id"`
	WorkflowID      string   `json:"workflow_id,omitempty"`
	CustomerID      string   `json:"customer_id,omitempty"`
	AmountCents     int64    `json:"amount_cents,omitempty"`
	Currency        string   `json:"currency,omitempty"`
	MatchedRules    []string `json:"matched_rules"`
	ContextDigest   string   `json:"context_digest,omitempty"`
}

type voidResultPayload struct {
	AuthorizationID string `json:"authorization_id"`
	WorkflowID      string `json:"workflow_id,omitempty"`
}

// failurePayload mirrors corebanking.failurePayload so the saga engine can
// classify terminal risk-command failures uniformly.
type failurePayload struct {
	ErrorClass   string `json:"error_class"`
	ErrorMessage string `json:"error_message,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
}

// emitFailure builds and appends an invalid_message failure result event for
// an unknown / undecodable risk command. Returns nil on success so
// messaging.ProcessDelivery acks the delivery; the saga engine reacts to the
// terminal invalid_message classification by compensating or rejecting.
func (c *Consumer) emitFailure(ctx context.Context, q pg.DBTX, cmdEnv messaging.Envelope, message string) error {
	payload := failurePayload{
		ErrorClass:   string(workflow.InvalidMessage),
		ErrorMessage: message,
		WorkflowID:   cmdEnv.WorkflowID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", EventCommandRejected, err)
	}
	env := makeResultEnvelope(cmdEnv, EventCommandRejected, body, c.now())
	return c.outbox.AppendOutbox(ctx, q, env, RouteCommandRejected)
}

// ---------------------------------------------------------------------------
// Concrete OutboxWriter — backed by the risk DB.
// ---------------------------------------------------------------------------

// RiskOutbox writes result envelopes to the risk DB's outbox_message table.
// It implements OutboxWriter so the consumer can commit the outbox row inside
// the same transaction as the Inbox insert + domain mutation.
type RiskOutbox struct {
	db *sql.DB
}

// NewRiskOutbox binds a *sql.DB (risk_db). The caller owns the pool.
func NewRiskOutbox(db *sql.DB) *RiskOutbox { return &RiskOutbox{db: db} }

// AppendOutbox enqueues an envelope for at-least-once publishing once the
// surrounding transaction commits. Mirrors the workflow engine's AppendOutbox
// SQL so the shared outbox_message table is populated identically.
func (o *RiskOutbox) AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal outbox envelope %s: %w", env.MessageID, err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO outbox_message
		  (message_id, message_type, schema_version, routing_key, envelope,
		   attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, 0, CURRENT_TIMESTAMP)`,
		env.MessageID, env.MessageType, env.SchemaVersion, routingKey, []byte(body),
	)
	if err != nil {
		return fmt.Errorf("insert outbox_message %s: %w", env.MessageID, err)
	}
	return nil
}
