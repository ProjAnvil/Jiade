package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
)

func fixedPaymentClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

func TestTransientPaymentGetsTwoAttemptsThenSucceeds(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	result, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-1", Currency: "CNY", AmountMinor: 3593,
		IdempotencyKey: "place-1", CorrelationID: "request-1",
	})
	if err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if result.Intent.Status != StateSucceeded {
		t.Fatalf("status=%q, want succeeded", result.Intent.Status)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts=%d, want 2 (transient then success)", len(result.Attempts))
	}
	if result.Attempts[0].Status != "failed" || result.Attempts[0].FailureCode != "provider_timeout" {
		t.Fatalf("first attempt=%+v, want failed/provider_timeout", result.Attempts[0])
	}
	if result.Attempts[1].Status != "succeeded" {
		t.Fatalf("second attempt=%+v, want succeeded", result.Attempts[1])
	}
	if len(result.Events) != 1 || result.Events[0].Type != "payment.captured.v1" {
		t.Fatalf("events=%+v, want single payment.captured.v1", eventTypes(result.Events))
	}
	if result.Replayed {
		t.Fatalf("first capture must not be a replay")
	}
}

func TestHardDeclineEmitsPaymentFailedWithCode(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	result, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-2", Currency: "USD", AmountMinor: 1500,
		IdempotencyKey: "place-2", CorrelationID: "request-2",
	})
	if err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if result.Intent.Status != StateFailed {
		t.Fatalf("status=%q, want failed", result.Intent.Status)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("attempts=%d, want 1 (hard decline does not retry)", len(result.Attempts))
	}
	if len(result.Events) != 1 || result.Events[0].Type != "payment.failed.v1" {
		t.Fatalf("events=%+v, want single payment.failed.v1", eventTypes(result.Events))
	}
	var payload paymentFailurePayload
	if err := decodeStrict(result.Events[0].Data, &payload); err != nil {
		t.Fatalf("decode failure payload: %v", err)
	}
	if payload.OrderID != "ORD-2" || payload.Code != "card_declined" {
		t.Fatalf("payload=%+v, want order_id=ORD-2 code=card_declined", payload)
	}
	if err := assertMoneyPayload(result.Events[0]); err != nil {
		t.Fatalf("failed event must not carry money payload: %v", err)
	}
}

func TestInsufficientFundsScenarioRecordsFailureCode(t *testing.T) {
	fixture := newPaymentFixture(ScenarioInsufficientFunds)
	result, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-3", Currency: "EUR", AmountMinor: 9900,
		IdempotencyKey: "place-3", CorrelationID: "request-3",
	})
	if err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if result.Intent.Status != StateFailed {
		t.Fatalf("status=%q, want failed", result.Intent.Status)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].FailureCode != "insufficient_funds" {
		t.Fatalf("attempts=%+v, want single insufficient_funds failure", result.Attempts)
	}
}

func TestRiskRejectionScenarioRecordsFailureCode(t *testing.T) {
	fixture := newPaymentFixture(ScenarioRiskRejection)
	result, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-4", Currency: "GBP", AmountMinor: 4200,
		IdempotencyKey: "place-4", CorrelationID: "request-4",
	})
	if err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if len(result.Attempts) != 1 || result.Attempts[0].FailureCode != "risk_rejection" {
		t.Fatalf("attempts=%+v, want single risk_rejection failure", result.Attempts)
	}
}

func TestCapturedEventPayloadMatchesOrderContractExactly(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	result, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-5", Currency: "CNY", AmountMinor: 3593,
		IdempotencyKey: "place-5", CorrelationID: "request-5",
	})
	if err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	captured := result.Events[0]
	var payload moneyResultPayload
	if err := decodeStrict(captured.Data, &payload); err != nil {
		t.Fatalf("decode money payload: %v", err)
	}
	if payload.OrderID != "ORD-5" || payload.Currency != "CNY" || payload.AmountMinor != 3593 {
		t.Fatalf("payload=%+v, want exact order money", payload)
	}
	if captured.Subject != "ORD-5" {
		t.Fatalf("subject=%q, want ORD-5", captured.Subject)
	}
	if captured.SchemaVersion != messaging.CurrentSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", captured.SchemaVersion, messaging.CurrentSchemaVersion)
	}
	if captured.OccurredAt.IsZero() {
		t.Fatalf("occurred_at must be non-zero")
	}
	if captured.CorrelationID != "request-5" {
		t.Fatalf("correlation_id=%q, want request-5", captured.CorrelationID)
	}
}

func TestDuplicateOrderPlacedReplayIsIdempotent(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	command := CaptureCommand{
		OrderID: "ORD-6", Currency: "CNY", AmountMinor: 1000,
		IdempotencyKey: "place-6", CorrelationID: "request-6",
	}
	first, err := fixture.service.CaptureOrder(context.Background(), command)
	if err != nil {
		t.Fatalf("first CaptureOrder error: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first capture must not be a replay")
	}
	second, err := fixture.service.CaptureOrder(context.Background(), command)
	if err != nil {
		t.Fatalf("second CaptureOrder error: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("second capture must be a replay")
	}
	if second.Intent.PaymentIntentID != first.Intent.PaymentIntentID {
		t.Fatalf("replay changed intent id: first=%s second=%s",
			first.Intent.PaymentIntentID, second.Intent.PaymentIntentID)
	}
	if fixture.store.captureCalls != 1 {
		t.Fatalf("capture calls=%d, want 1 (idempotent replay must not re-run provider)",
			fixture.store.captureCalls)
	}
	if len(fixture.store.capturedEvents) != 1 {
		t.Fatalf("outbox events=%d, want 1 (no duplicates on replay)",
			len(fixture.store.capturedEvents))
	}
}

func TestCaptureOrderRejectsInvalidCommand(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	for _, command := range []CaptureCommand{
		{OrderID: "", Currency: "CNY", AmountMinor: 100, IdempotencyKey: "k"},
		{OrderID: "ORD", Currency: "CN", AmountMinor: 100, IdempotencyKey: "k"},
		{OrderID: "ORD", Currency: "CNY", AmountMinor: 0, IdempotencyKey: "k"},
		{OrderID: "ORD", Currency: "CNY", AmountMinor: 100, IdempotencyKey: ""},
	} {
		if _, err := fixture.service.CaptureOrder(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("CaptureOrder(%+v) error=%v, want ErrInvalidCommand", command, err)
		}
	}
}

func TestFullRefundTransitionsToRefunded(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-7", Currency: "CNY", AmountMinor: 2000,
		IdempotencyKey: "place-7", CorrelationID: "request-7",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	fixture.store.resetEvents()
	result, err := fixture.service.Refund(context.Background(), RefundCommand{
		OrderID: "ORD-7", AmountMinor: 2000, Reason: "customer_request",
		IdempotencyKey: "refund-7", CorrelationID: "request-7",
	})
	if err != nil {
		t.Fatalf("Refund error: %v", err)
	}
	if result.Intent.Status != StateRefunded {
		t.Fatalf("status=%q, want refunded", result.Intent.Status)
	}
	if len(result.Events) != 1 || result.Events[0].Type != "refund.succeeded.v1" {
		t.Fatalf("events=%+v, want single refund.succeeded.v1", eventTypes(result.Events))
	}
	var payload moneyResultPayload
	if err := decodeStrict(result.Events[0].Data, &payload); err != nil {
		t.Fatalf("decode refund payload: %v", err)
	}
	if payload.OrderID != "ORD-7" || payload.Currency != "CNY" || payload.AmountMinor != 2000 {
		t.Fatalf("payload=%+v, want exact refund money", payload)
	}
}

func TestPartialRefundTransitionsToPartiallyRefunded(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-8", Currency: "CNY", AmountMinor: 3000,
		IdempotencyKey: "place-8", CorrelationID: "request-8",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	fixture.store.resetEvents()
	result, err := fixture.service.Refund(context.Background(), RefundCommand{
		OrderID: "ORD-8", AmountMinor: 1000, Reason: "partial",
		IdempotencyKey: "refund-8a", CorrelationID: "request-8",
	})
	if err != nil {
		t.Fatalf("first Refund error: %v", err)
	}
	if result.Intent.Status != StatePartiallyRefunded {
		t.Fatalf("status=%q, want partially_refunded", result.Intent.Status)
	}
	fixture.store.resetEvents()
	result, err = fixture.service.Refund(context.Background(), RefundCommand{
		OrderID: "ORD-8", AmountMinor: 2000, Reason: "remaining",
		IdempotencyKey: "refund-8b", CorrelationID: "request-8",
	})
	if err != nil {
		t.Fatalf("second Refund error: %v", err)
	}
	if result.Intent.Status != StateRefunded {
		t.Fatalf("status=%q, want refunded after exhausting captured amount", result.Intent.Status)
	}
}

func TestRefundRejectsAmountExceedingCaptured(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-9", Currency: "CNY", AmountMinor: 1000,
		IdempotencyKey: "place-9", CorrelationID: "request-9",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if _, err := fixture.service.Refund(context.Background(), RefundCommand{
		OrderID: "ORD-9", AmountMinor: 2000, Reason: "over",
		IdempotencyKey: "refund-9", CorrelationID: "request-9",
	}); !errors.Is(err, ErrRefundExceedsCaptured) {
		t.Fatalf("Refund error=%v, want ErrRefundExceedsCaptured", err)
	}
}

func TestRefundFailsUnlessIntentCaptured(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-10", Currency: "CNY", AmountMinor: 1000,
		IdempotencyKey: "place-10", CorrelationID: "request-10",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	if _, err := fixture.service.Refund(context.Background(), RefundCommand{
		OrderID: "ORD-10", AmountMinor: 500, Reason: "x",
		IdempotencyKey: "refund-10", CorrelationID: "request-10",
	}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("Refund error=%v, want ErrInvalidCommand", err)
	}
}

func TestCancelIntentForProcessingIntentTransitionsToCancelled(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	// Seed a processing intent directly so we can exercise the cancel path
	// without completing capture first.
	intent := Intent{
		PaymentIntentID: deterministicIntentID("place-11"),
		OrderID:         "ORD-11", AmountMinor: 1000, Currency: "CNY",
		Status: StateProcessing, Provider: defaultProvider, IdempotencyKey: "place-11",
	}
	fixture.store.seedIntent(intent)
	cancelled, err := fixture.service.CancelIntent(context.Background(), CancelCommand{
		OrderID: "ORD-11", Reason: "buyer_cancelled",
		IdempotencyKey: "cancel-11", CorrelationID: "request-11",
	})
	if err != nil {
		t.Fatalf("CancelIntent error: %v", err)
	}
	if cancelled.Intent.Status != StateCancelled {
		t.Fatalf("status=%q, want cancelled", cancelled.Intent.Status)
	}
	if cancelled.Replayed {
		t.Fatalf("processing cancel must not be a replay")
	}
}

func TestCancelIntentForTerminalIntentIsIdempotentNoOp(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-11", Currency: "CNY", AmountMinor: 1000,
		IdempotencyKey: "place-11", CorrelationID: "request-11",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	// A failed intent must accept the order.cancelled.v1 that order emits as
	// compensation without blocking the inbox delivery.
	cancelled, err := fixture.service.CancelIntent(context.Background(), CancelCommand{
		OrderID: "ORD-11", Reason: "buyer_cancelled",
		IdempotencyKey: "cancel-11", CorrelationID: "request-11",
	})
	if err != nil {
		t.Fatalf("CancelIntent error: %v", err)
	}
	if cancelled.Intent.Status != StateFailed {
		t.Fatalf("status=%q, want failed (terminal no-op)", cancelled.Intent.Status)
	}
	if !cancelled.Replayed {
		t.Fatalf("cancel of terminal intent must be a replay no-op")
	}
}

func TestCancelCapturedIntentIsIdempotentNoOp(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-12", Currency: "CNY", AmountMinor: 1000,
		IdempotencyKey: "place-12", CorrelationID: "request-12",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	// A captured intent must accept the cancel event that order emits as
	// compensation without regressing state; refunds handle the money side.
	cancelled, err := fixture.service.CancelIntent(context.Background(), CancelCommand{
		OrderID: "ORD-12", Reason: "late",
		IdempotencyKey: "cancel-12", CorrelationID: "request-12",
	})
	if err != nil {
		t.Fatalf("CancelIntent error=%v, want nil (idempotent no-op)", err)
	}
	if cancelled.Intent.Status != StateSucceeded {
		t.Fatalf("status=%q, want succeeded (cancel must not regress capture)", cancelled.Intent.Status)
	}
	if !cancelled.Replayed {
		t.Fatalf("cancel of captured intent must be a replay no-op")
	}
}

func TestDuplicateRefundReplayIsIdempotent(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	if _, err := fixture.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID: "ORD-13", Currency: "CNY", AmountMinor: 1500,
		IdempotencyKey: "place-13", CorrelationID: "request-13",
	}); err != nil {
		t.Fatalf("CaptureOrder error: %v", err)
	}
	command := RefundCommand{
		OrderID: "ORD-13", AmountMinor: 1500, Reason: "full",
		IdempotencyKey: "refund-13", CorrelationID: "request-13",
	}
	first, err := fixture.service.Refund(context.Background(), command)
	if err != nil {
		t.Fatalf("first Refund error: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first refund must not be a replay")
	}
	second, err := fixture.service.Refund(context.Background(), command)
	if err != nil {
		t.Fatalf("second Refund error: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("second refund must be a replay")
	}
	refundEvents := 0
	for _, event := range fixture.store.capturedEvents {
		if event.Type == "refund.succeeded.v1" && event.Subject == "ORD-13" {
			refundEvents++
		}
	}
	if refundEvents != 1 {
		t.Fatalf("refund outbox events=%d, want 1 (no duplicates on replay)",
			refundEvents)
	}
}

// --- helpers / fakes ---

func decodeStrict(data json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func assertMoneyPayload(event messaging.Event) error {
	var payload moneyResultPayload
	if err := decodeStrict(event.Data, &payload); err == nil {
		return errors.New("failure event decoded as money payload; contract allows only {order_id,code}")
	}
	return nil
}

func eventTypes(events []messaging.Event) []string {
	types := make([]string, len(events))
	for index, event := range events {
		types[index] = event.Type
	}
	return types
}

type paymentFixture struct {
	service *Service
	store   *fakeStore
	provider *Simulator
}

func newPaymentFixture(scenario Scenario) *paymentFixture {
	store := newFakeStore()
	provider := NewSimulator(scenario)
	service := NewService(store, provider, ServiceOptions{Clock: fixedPaymentClock})
	return &paymentFixture{service: service, store: store, provider: provider}
}

type fakeStore struct {
	mu             sync.Mutex
	intents        map[string]Intent
	byID           map[string]string // order_id -> payment_intent_id
	attempts       map[string][]Attempt
	refunds        map[string][]Refund // intent_id -> refunds
	refundsByKey   map[string]Refund   // idempotency_key -> refund
	capturedEvents []messaging.Event
	captureCalls   int
	refundCalls    int
	cancelCalls    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		intents:      make(map[string]Intent),
		byID:         make(map[string]string),
		attempts:     make(map[string][]Attempt),
		refunds:      make(map[string][]Refund),
		refundsByKey: make(map[string]Refund),
	}
}

func (store *fakeStore) seedIntent(intent Intent) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.intents[intent.IdempotencyKey] = intent
	store.byID[intent.OrderID] = intent.PaymentIntentID
}

func (store *fakeStore) resetEvents() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.capturedEvents = nil
}

func (store *fakeStore) FindIntent(ctx context.Context, idempotencyKey string) (Intent, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	intent, found := store.intents[idempotencyKey]
	return intent, found, nil
}

func (store *fakeStore) GetIntentByOrder(ctx context.Context, orderID string) (Intent, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	intentID, found := store.byID[orderID]
	if !found {
		return Intent{}, false, nil
	}
	for _, intent := range store.intents {
		if intent.PaymentIntentID == intentID {
			intent.RefundedMinor = store.sumRefunded(intent.PaymentIntentID)
			return intent, true, nil
		}
	}
	return Intent{}, false, nil
}

func (store *fakeStore) sumRefunded(intentID string) int64 {
	var total int64
	for _, refund := range store.refunds[intentID] {
		if refund.Status == "succeeded" {
			total += refund.AmountMinor
		}
	}
	return total
}

func (store *fakeStore) FindRefund(ctx context.Context, idempotencyKey string) (Refund, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	refund, found := store.refundsByKey[idempotencyKey]
	return refund, found, nil
}

func (store *fakeStore) SaveCapture(
	ctx context.Context,
	outcome CaptureOutcome,
) (CaptureResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.captureCalls++
	if _, exists := store.intents[outcome.Intent.IdempotencyKey]; exists {
		existing := store.intents[outcome.Intent.IdempotencyKey]
		existing.Replayed = true
		store.intents[outcome.Intent.IdempotencyKey] = existing
		attempts := store.attempts[existing.PaymentIntentID]
		events := make([]messaging.Event, 0)
		for _, event := range store.capturedEvents {
			if event.Subject == existing.OrderID {
				events = append(events, event)
			}
		}
		return CaptureResult{Intent: existing, Attempts: attempts, Events: events, Replayed: true}, nil
	}
	intent := outcome.Intent
	store.intents[intent.IdempotencyKey] = intent
	store.byID[intent.OrderID] = intent.PaymentIntentID
	store.attempts[intent.PaymentIntentID] = append([]Attempt(nil), outcome.Attempts...)
	store.capturedEvents = append(store.capturedEvents, outcome.Events...)
	return CaptureResult{Intent: intent, Attempts: outcome.Attempts, Events: outcome.Events}, nil
}

func (store *fakeStore) SaveRefund(
	ctx context.Context,
	outcome RefundOutcome,
) (RefundResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.refundCalls++
	intent, found := store.intents[outcome.Intent.IdempotencyKey]
	if !found {
		return RefundResult{}, ErrIntentNotFound
	}
	for _, refund := range store.refunds[intent.PaymentIntentID] {
		if refund.IdempotencyKey == outcome.Refund.IdempotencyKey {
			intent.Replayed = true
			store.intents[outcome.Intent.IdempotencyKey] = intent
			events := make([]messaging.Event, 0)
			for _, event := range store.capturedEvents {
				if event.Subject == intent.OrderID && event.Type == "refund.succeeded.v1" {
					events = append(events, event)
				}
			}
			return RefundResult{Intent: intent, Refund: refund, Events: events, Replayed: true}, nil
		}
	}
	intent = outcome.Intent
	store.intents[outcome.Intent.IdempotencyKey] = intent
	store.refunds[intent.PaymentIntentID] = append(store.refunds[intent.PaymentIntentID], outcome.Refund)
	store.refundsByKey[outcome.Refund.IdempotencyKey] = outcome.Refund
	store.capturedEvents = append(store.capturedEvents, outcome.Events...)
	return RefundResult{Intent: intent, Refund: outcome.Refund, Events: outcome.Events}, nil
}

func (store *fakeStore) SaveCancel(
	ctx context.Context,
	outcome CancelOutcome,
) (CancelResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.cancelCalls++
	intent := outcome.Intent
	store.intents[intent.IdempotencyKey] = intent
	store.capturedEvents = append(store.capturedEvents, outcome.Events...)
	return CancelResult{Intent: intent, Events: outcome.Events}, nil
}

var _ Store = (*fakeStore)(nil)
