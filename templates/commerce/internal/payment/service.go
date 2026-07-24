// Package payment owns payment-intent lifecycle rules, provider simulation,
// and the event workflow that order (Task 6) consumes.
package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"commerce/internal/platform/messaging"
)

// Event type constants this package produces. They match order's bindings
// exactly (see cmd/order/main.go:orderEventBindings).
const (
	EventPaymentCaptured = "payment.captured.v1"
	EventPaymentFailed   = "payment.failed.v1"
	EventRefundSucceeded = "refund.succeeded.v1"
)

// MethodType enumerates the payment_method_snapshot.method_type values.
type MethodType string

const (
	MethodCard           MethodType = "card"
	MethodWallet         MethodType = "wallet"
	MethodBankTransfer   MethodType = "bank_transfer"
)

const (
	defaultMethodType MethodType = MethodCard
	defaultProvider   string     = "simulator"
)

var (
	// ErrInvalidCommand flags a malformed command (bad money, missing keys).
	ErrInvalidCommand = errors.New("invalid payment command")
	// ErrIntentNotFound flags a missing intent for refund/cancel paths.
	ErrIntentNotFound = errors.New("payment intent not found")
	// ErrRefundExceedsCaptured flags a refund request larger than the
	// remaining captured amount.
	ErrRefundExceedsCaptured = errors.New("refund exceeds captured amount")
)

// Intent is the immutable identity of a capture attempt. The schema stores it
// in payment_intent; provider_reference and idempotency_key are UNIQUE.
type Intent struct {
	PaymentIntentID   string `json:"payment_intent_id"`
	OrderID           string `json:"order_id"`
	AmountMinor       int64  `json:"amount_minor"`
	Currency          string `json:"currency"`
	Status            State  `json:"status"`
	Provider          string `json:"provider"`
	ProviderReference string `json:"provider_reference,omitempty"`
	IdempotencyKey    string `json:"-"`
	RefundedMinor     int64  `json:"refunded_minor,omitempty"`
	Replayed          bool   `json:"-"`
}

// Attempt is a single provider ChargeResult recorded in payment_attempt.
type Attempt struct {
	AttemptID         string      `json:"attempt_id"`
	PaymentIntentID   string      `json:"payment_intent_id"`
	Status            string      `json:"status"`
	FailureCode       FailureCode `json:"failure_code,omitempty"`
	AmountMinor       int64       `json:"amount_minor"`
	ProviderReference string      `json:"-"`
	ProviderEventID   string      `json:"-"`
}

// Refund is a single refund record stored in the refund table.
type Refund struct {
	RefundID       string `json:"refund_id"`
	PaymentIntentID string `json:"payment_intent_id"`
	AmountMinor    int64  `json:"amount_minor"`
	Status         string `json:"status"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"-"`
}

// CaptureCommand is the input produced by consuming order.placed.v1.
type CaptureCommand struct {
	OrderID        string
	Currency       string
	AmountMinor    int64
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
	MethodType     MethodType
	MethodToken    string
	OccurredAt     time.Time
}

// RefundCommand is the input produced by consuming payment.refund-requested.v1.
type RefundCommand struct {
	OrderID        string
	AmountMinor    int64
	Currency       string
	Reason         string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
	OccurredAt     time.Time
}

// CancelCommand is the input produced by consuming order.cancelled.v1.
type CancelCommand struct {
	OrderID        string
	Reason         string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
	OccurredAt     time.Time
}

// CaptureOutcome is the in-memory result handed to the store for atomic write.
type CaptureOutcome struct {
	Intent   Intent
	Attempts []Attempt
	Events   []messaging.Event
}

// CaptureResult is what the service returns to the caller (HTTP or consumer).
type CaptureResult struct {
	Intent   Intent
	Attempts []Attempt
	Events   []messaging.Event
	Replayed bool
}

// RefundOutcome is the in-memory result handed to the store for atomic write.
type RefundOutcome struct {
	Intent Intent
	Refund Refund
	Events []messaging.Event
}

// RefundResult is what the service returns to the caller.
type RefundResult struct {
	Intent  Intent
	Refund  Refund
	Events  []messaging.Event
	Replayed bool
}

// CancelOutcome is the in-memory result handed to the store for atomic write.
type CancelOutcome struct {
	Intent Intent
	Events []messaging.Event
}

// CancelResult is what the service returns to the caller.
type CancelResult struct {
	Intent  Intent
	Events  []messaging.Event
	Replayed bool
}

// Store persists payment state and the derived Outbox events in one
// transaction. Implementations MUST make replay idempotent on
// idempotency_key and MUST NOT insert duplicate Outbox rows.
type Store interface {
	FindIntent(ctx context.Context, idempotencyKey string) (Intent, bool, error)
	GetIntentByOrder(ctx context.Context, orderID string) (Intent, bool, error)
	FindRefund(ctx context.Context, idempotencyKey string) (Refund, bool, error)
	SaveCapture(ctx context.Context, outcome CaptureOutcome) (CaptureResult, error)
	SaveRefund(ctx context.Context, outcome RefundOutcome) (RefundResult, error)
	SaveCancel(ctx context.Context, outcome CancelOutcome) (CancelResult, error)
}

// ServiceOptions carries optional collaborators (clock, scenario resolver).
type ServiceOptions struct {
	Clock func() time.Time
}

// Service applies payment commands through the state machine and the provider.
type Service struct {
	store    Store
	provider Provider
	clock    func() time.Time
}

// NewService constructs a Service. provider must be deterministic.
func NewService(store Store, provider Provider, options ServiceOptions) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, provider: provider, clock: clock}
}

// CaptureOrder runs the deterministic provider against the order's money and
// persists a captured or failed intent. Replays of the same idempotency key
// return the original outcome without re-invoking the provider.
func (service *Service) CaptureOrder(ctx context.Context, command CaptureCommand) (CaptureResult, error) {
	if service == nil || service.store == nil || service.provider == nil {
		return CaptureResult{}, errors.New("payment service is unavailable")
	}
	command = canonicalCaptureCommand(command)
	if err := validateCaptureCommand(command); err != nil {
		return CaptureResult{}, err
	}
	if existing, found, err := service.store.FindIntent(ctx, command.IdempotencyKey); err != nil {
		return CaptureResult{}, fmt.Errorf("find payment intent: %w", err)
	} else if found {
		return service.replayCapture(ctx, existing)
	}
	intentID := deterministicIntentID(command.IdempotencyKey)
	now := service.clock().UTC()
	if command.OccurredAt.IsZero() {
		command.OccurredAt = now
	}
	intent := Intent{
		PaymentIntentID: intentID,
		OrderID:         command.OrderID,
		AmountMinor:     command.AmountMinor,
		Currency:        command.Currency,
		Status:          StateRequiresMethod,
		Provider:        defaultProvider,
		IdempotencyKey:  command.IdempotencyKey,
	}
	intent.Status, _ = Transition(intent.Status, EventMethodAttached)
	maxAttempts := service.provider.MaxAttempts()
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	var lastFailure FailureCode
	attempts := make([]Attempt, 0, maxAttempts)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		charge, err := service.provider.Charge(ChargeRequest{
			IntentID:        intentID,
			IdempotencyKey:  command.IdempotencyKey,
			OrderID:         command.OrderID,
			Currency:        command.Currency,
			AmountMinor:     command.AmountMinor,
			Attempt:         attempt,
			MethodToken:     command.MethodToken,
			ProviderEventID: deterministicProviderEventID(intentID, attempt),
		})
		if err != nil {
			return CaptureResult{}, fmt.Errorf("charge payment: %w", err)
		}
		intent.ProviderReference = charge.ProviderReference
		record := Attempt{
			AttemptID:         deterministicAttemptID(intentID, attempt),
			PaymentIntentID:   intentID,
			Status:            charge.Status,
			FailureCode:       charge.FailureCode,
			AmountMinor:       command.AmountMinor,
			ProviderReference: charge.ProviderReference,
			ProviderEventID:   charge.ProviderEventID,
		}
		attempts = append(attempts, record)
		if charge.Status == "succeeded" {
			intent.Status, _ = Transition(intent.Status, EventAuthorize)
			intent.Status, _ = Transition(intent.Status, EventCapture)
			event := service.capturedEvent(intent, command, now)
			outcome := CaptureOutcome{Intent: intent, Attempts: attempts, Events: []messaging.Event{event}}
			result, err := service.store.SaveCapture(ctx, outcome)
			if err != nil {
				return CaptureResult{}, err
			}
			result.Attempts = collectAttempts(result.Attempts, attempts)
			return result, nil
		}
		lastFailure = charge.FailureCode
		if attempt < maxAttempts {
			// Transient failure; the next attempt will be persisted alongside
			// the terminal outcome so the store writes intent + attempts +
			// outbox atomically.
			continue
		}
		intent.Status, _ = Transition(intent.Status, EventFail)
		event := service.failedEvent(intent, command, lastFailure, now)
		outcome := CaptureOutcome{Intent: intent, Attempts: attempts, Events: []messaging.Event{event}}
		result, err := service.store.SaveCapture(ctx, outcome)
		if err != nil {
			return CaptureResult{}, err
		}
		result.Attempts = collectAttempts(result.Attempts, attempts)
		return result, nil
	}
	// Unreachable: the loop always returns on success or on the final attempt.
	return CaptureResult{}, fmt.Errorf("payment capture did not terminate")
}

func (service *Service) replayCapture(ctx context.Context, intent Intent) (CaptureResult, error) {
	events := make([]messaging.Event, 0)
	if intent.Status == StateSucceeded || intent.Status == StatePartiallyRefunded || intent.Status == StateRefunded {
		events = append(events, service.capturedEvent(intent, CaptureCommand{}, service.clock().UTC()))
	} else if intent.Status == StateFailed {
		events = append(events, service.failedEvent(intent, CaptureCommand{}, "", service.clock().UTC()))
	}
	return CaptureResult{Intent: intent, Attempts: nil, Events: events, Replayed: true}, nil
}

// Refund records a refund request and emits refund.succeeded.v1 with the
// requested amount. Partial refunds accumulate until the captured amount is
// exhausted; the store enforces the cumulative CHECK constraint.
func (service *Service) Refund(ctx context.Context, command RefundCommand) (RefundResult, error) {
	if service == nil || service.store == nil {
		return RefundResult{}, errors.New("payment service is unavailable")
	}
	command = canonicalRefundCommand(command)
	if err := validateRefundCommand(command); err != nil {
		return RefundResult{}, err
	}
	if existing, found, err := service.store.FindRefund(ctx, command.IdempotencyKey); err != nil {
		return RefundResult{}, fmt.Errorf("find refund: %w", err)
	} else if found {
		intent, _, err := service.store.GetIntentByOrder(ctx, command.OrderID)
		if err != nil {
			return RefundResult{}, fmt.Errorf("load payment intent: %w", err)
		}
		intent.Replayed = true
		return RefundResult{Intent: intent, Refund: existing, Replayed: true}, nil
	}
	intent, found, err := service.store.GetIntentByOrder(ctx, command.OrderID)
	if err != nil {
		return RefundResult{}, fmt.Errorf("load payment intent: %w", err)
	}
	if !found {
		return RefundResult{}, ErrIntentNotFound
	}
	if intent.Status != StateSucceeded && intent.Status != StatePartiallyRefunded {
		return RefundResult{}, fmt.Errorf("%w: intent status %q", ErrInvalidCommand, intent.Status)
	}
	remaining := intent.AmountMinor - intent.RefundedMinor
	if remaining <= 0 {
		return RefundResult{}, fmt.Errorf("%w: nothing remains to refund", ErrRefundExceedsCaptured)
	}
	if command.AmountMinor > remaining {
		return RefundResult{}, fmt.Errorf("%w: %d > %d", ErrRefundExceedsCaptured, command.AmountMinor, remaining)
	}
	now := service.clock().UTC()
	if command.OccurredAt.IsZero() {
		command.OccurredAt = now
	}
	next, err := Transition(intent.Status, chooseRefundEvent(command.AmountMinor, remaining))
	if err != nil {
		return RefundResult{}, fmt.Errorf("%w: refund transition: %v", ErrInvalidCommand, err)
	}
	intent.Status = next
	intent.RefundedMinor = intent.RefundedMinor + command.AmountMinor
	refund := Refund{
		RefundID:        deterministicRefundID(intent.PaymentIntentID, command.IdempotencyKey),
		PaymentIntentID: intent.PaymentIntentID,
		AmountMinor:     command.AmountMinor,
		Status:          "succeeded",
		Reason:          command.Reason,
		IdempotencyKey:  command.IdempotencyKey,
	}
	event := service.refundEvent(intent, command, now)
	outcome := RefundOutcome{Intent: intent, Refund: refund, Events: []messaging.Event{event}}
	return service.store.SaveRefund(ctx, outcome)
}

// CancelIntent applies an order.cancelled.v1 event. Only intents that have not
// been captured may transition; a cancel for an already-terminal intent
// (failed, cancelled, succeeded) is an idempotent no-op so the inbox delivery
// succeeds without producing duplicate compensation.
func (service *Service) CancelIntent(ctx context.Context, command CancelCommand) (CancelResult, error) {
	if service == nil || service.store == nil {
		return CancelResult{}, errors.New("payment service is unavailable")
	}
	command = canonicalCancelCommand(command)
	if err := validateCancelCommand(command); err != nil {
		return CancelResult{}, err
	}
	intent, found, err := service.store.GetIntentByOrder(ctx, command.OrderID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("load payment intent: %w", err)
	}
	if !found {
		return CancelResult{}, ErrIntentNotFound
	}
	if intent.Status == StateCancelled || intent.Status == StateFailed ||
		intent.Status == StateSucceeded || intent.Status == StatePartiallyRefunded ||
		intent.Status == StateRefunded {
		intent.Replayed = true
		return CancelResult{Intent: intent, Events: nil, Replayed: true}, nil
	}
	next, err := Transition(intent.Status, EventCancel)
	if err != nil {
		return CancelResult{}, fmt.Errorf("%w: cancel transition: %v", ErrInvalidCommand, err)
	}
	intent.Status = next
	outcome := CancelOutcome{Intent: intent, Events: nil}
	result, err := service.store.SaveCancel(ctx, outcome)
	if err != nil {
		return CancelResult{}, err
	}
	result.Replayed = false
	return result, nil
}

// --- event construction ---

type moneyResultPayload struct {
	OrderID     string `json:"order_id"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

type paymentFailurePayload struct {
	OrderID string `json:"order_id"`
	Code    string `json:"code"`
}

func (service *Service) capturedEvent(intent Intent, command CaptureCommand, now time.Time) messaging.Event {
	body, _ := json.Marshal(moneyResultPayload{
		OrderID: intent.OrderID, Currency: intent.Currency, AmountMinor: intent.AmountMinor,
	})
	return messaging.NewEvent(EventPaymentCaptured, intent.OrderID,
		command.CorrelationID, command.CausationID, body, func() time.Time { return now })
}

func (service *Service) failedEvent(intent Intent, command CaptureCommand, code FailureCode, now time.Time) messaging.Event {
	body, _ := json.Marshal(paymentFailurePayload{
		OrderID: intent.OrderID, Code: string(code),
	})
	return messaging.NewEvent(EventPaymentFailed, intent.OrderID,
		command.CorrelationID, command.CausationID, body, func() time.Time { return now })
}

func (service *Service) refundEvent(intent Intent, command RefundCommand, now time.Time) messaging.Event {
	body, _ := json.Marshal(moneyResultPayload{
		OrderID: intent.OrderID, Currency: intent.Currency, AmountMinor: command.AmountMinor,
	})
	return messaging.NewEvent(EventRefundSucceeded, intent.OrderID,
		command.CorrelationID, command.CausationID, body, func() time.Time { return now })
}

// --- validation / canonicalisation ---

func canonicalCaptureCommand(command CaptureCommand) CaptureCommand {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Currency = strings.ToUpper(strings.TrimSpace(command.Currency))
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.MethodToken = strings.TrimSpace(command.MethodToken)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CausationID = strings.TrimSpace(command.CausationID)
	if command.MethodType == "" {
		command.MethodType = defaultMethodType
	}
	return command
}

func validateCaptureCommand(command CaptureCommand) error {
	if command.OrderID == "" || len(command.Currency) != 3 ||
		command.AmountMinor <= 0 || command.IdempotencyKey == "" {
		return fmt.Errorf("%w: capture order/money/idempotency required", ErrInvalidCommand)
	}
	return nil
}

func canonicalRefundCommand(command RefundCommand) RefundCommand {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Currency = strings.ToUpper(strings.TrimSpace(command.Currency))
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CausationID = strings.TrimSpace(command.CausationID)
	return command
}

func validateRefundCommand(command RefundCommand) error {
	if command.OrderID == "" || command.AmountMinor <= 0 ||
		command.Reason == "" || command.IdempotencyKey == "" {
		return fmt.Errorf("%w: refund order/amount/reason/idempotency required", ErrInvalidCommand)
	}
	return nil
}

func canonicalCancelCommand(command CancelCommand) CancelCommand {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CausationID = strings.TrimSpace(command.CausationID)
	return command
}

func validateCancelCommand(command CancelCommand) error {
	if command.OrderID == "" || command.Reason == "" || command.IdempotencyKey == "" {
		return fmt.Errorf("%w: cancel order/reason/idempotency required", ErrInvalidCommand)
	}
	return nil
}

func chooseRefundEvent(amount, remaining int64) Event {
	if amount >= remaining {
		return EventRefund
	}
	return EventPartialRefund
}

func collectAttempts(stored, generated []Attempt) []Attempt {
	if len(stored) > 0 {
		return stored
	}
	return generated
}

// --- deterministic IDs ---

func deterministicIntentID(idempotencyKey string) string {
	sum := sha256.Sum256([]byte("payment_intent\x00" + idempotencyKey))
	return "pi_" + hex.EncodeToString(sum[:12])
}

func deterministicAttemptID(intentID string, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("payment_attempt\x00%s\x00%d", intentID, attempt)))
	return "att_" + hex.EncodeToString(sum[:12])
}

func deterministicRefundID(intentID, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("payment_refund\x00%s\x00%s", intentID, idempotencyKey)))
	return "rfd_" + hex.EncodeToString(sum[:12])
}
