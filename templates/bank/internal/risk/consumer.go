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
	"errors"
	"fmt"
	"log"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/testfail"
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

	// Test-only smoke gate: when BANK_TEST_FAILURES_ENABLED is set and the
	// workflow_id carries the smoke-reject prefix, emit a
	// risk.payment-rejected.v1 result event without persisting an
	// authorization row. The saga treats the rejection as terminal and
	// compensates (no prior succeeded action → the instance ends
	// compensated). Inert in production; see testfail docs.
	if testfail.IsReject(env.WorkflowID) {
		rejectedEnv, err := buildSmokeRejectEnvelope(env, payload, c.now())
		if err != nil {
			return err
		}
		return c.outbox.AppendOutbox(ctx, q, rejectedEnv, RoutePaymentRejected)
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

// buildSmokeRejectEnvelope constructs a risk.payment-rejected.v1 result
// envelope for the smoke-reject gate. It mirrors the wire shape produced by
// buildAuthorizeResultEnvelope so the saga engine's AuthorizeRisk.ApplyResult
// accepts it via the resultRiskRejected branch. The matched_rules marker
// "SMOKE-INJECTED" makes the rejection auditable as test-injected.
func buildSmokeRejectEnvelope(cmdEnv messaging.Envelope, payload authorizePaymentPayload, now time.Time) (messaging.Envelope, error) {
	body, err := json.Marshal(authorizeResultPayload{
		AuthorizationID: payload.AuthorizationID,
		WorkflowID:      cmdEnv.WorkflowID,
		CustomerID:      payload.CustomerID,
		AmountCents:     payload.AmountCents,
		Currency:        payload.Currency,
		MatchedRules:    []string{"SMOKE-INJECTED"},
	})
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("marshal smoke-reject payload: %w", err)
	}
	return makeResultEnvelope(cmdEnv, service.AuthorizeEventRejected, body, now), nil
}

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
// the command envelope. CommandID MUST be echoed so the saga engine's
// ApplyResult validation (env.CommandID == action.CommandID) accepts the
// result — mirrors corebanking.makeResultEnvelope.
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

// ---------------------------------------------------------------------------
// AMQP runtime: Run subscribes to the command queue and dispatches deliveries
// ---------------------------------------------------------------------------

// Run connects to the RabbitMQ broker, subscribes to the risk command queue,
// and dispatches each delivery to ConsumeDelivery. It blocks until ctx is
// cancelled (returns nil) or a fatal broker error occurs (returns error). The
// retry policy's Router is wired from the broker channel so
// messaging.ProcessDelivery can route retries and DLQ entries. Mirrors
// corebanking.Consumer.Run.
func (c *Consumer) Run(ctx context.Context, amqpURL, queue string) error {
	if c.db == nil {
		return errors.New("risk consumer: db is nil")
	}
	if amqpURL == "" {
		return errors.New("risk consumer: amqp URL is required")
	}
	if queue == "" {
		queue = ConsumerName + ".commands"
	}

	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("risk consumer: dial broker: %w", err)
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
		return fmt.Errorf("risk consumer: open channel: %w", err)
	}
	defer ch.Close()

	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("risk consumer: declare queue %s: %w", queue, err)
	}
	// Fair dispatch: process one delivery at a time so retries are not starved
	// by prefetch buffering.
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("risk consumer: set QoS: %w", err)
	}

	// Wire the retry policy's publisher so ProcessDelivery can route
	// replacement deliveries to retry/DLQ destinations. The publisher's
	// exchange is unused for retry routing (Route is called with the
	// RetryPolicy's explicit RetryExchange/DeadLetterExchange); bank.commands
	// is set as the semantically meaningful default for a command consumer.
	publisher, err := messaging.NewRabbitPublisher(ch, messaging.ExchangeCommands)
	if err != nil {
		return fmt.Errorf("risk consumer: retry publisher: %w", err)
	}
	c.policy.Router = publisher

	deliveries, err := ch.Consume(queue, ConsumerName, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("risk consumer: start consume: %w", err)
	}

	log.Printf("risk consumer: subscribed to queue %s", queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("risk consumer: delivery channel closed")
			}
			if err := c.ConsumeDelivery(ctx, delivery); err != nil {
				log.Printf("risk consumer: delivery %s: %v", delivery.MessageId, err)
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
// Re-declared here to keep risk's runtime self-contained (corebanking exports
// the same shape).
type Publisher interface {
	PublishTo(ctx context.Context, exchange, routingKey string, envelope messaging.Envelope) error
}

// OutboxRelay drains undispatched outbox_message rows and publishes them to the
// broker via a Publisher. Single-instance only (no claim tokens); for
// multi-instance deployments, add claim-based dispatch. Mirrors
// corebanking.OutboxRelay.
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

// NewOutboxRelay constructs an outbox relay. Interval defaults to 500ms,
// BatchSize to 100.
func NewOutboxRelay(db *sql.DB, publisher Publisher) *OutboxRelay {
	return &OutboxRelay{db: db, publisher: publisher, Interval: 500 * time.Millisecond, BatchSize: 100}
}

// Run polls outbox_message and publishes undispatched envelopes until ctx is
// cancelled (returns nil) or a fatal error occurs.
func (r *OutboxRelay) Run(ctx context.Context) error {
	if r.db == nil {
		return errors.New("risk outbox relay: db is nil")
	}
	if r.publisher == nil {
		return errors.New("risk outbox relay: publisher is nil")
	}
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.drain(ctx); err != nil {
				return fmt.Errorf("risk outbox relay: drain: %w", err)
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
			log.Printf("risk outbox relay: poison message %s: %v", p.msgID, err)
			continue
		}
		// Derive the topic exchange from the routing key. The risk outbox
		// holds result events (risk.payment.*, risk.command.rejected), which
		// route through bank.events; deriving per row keeps the relay robust
		// if a versioned command is ever appended here.
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
