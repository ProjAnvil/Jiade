package payment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
)

// TestConsumerApplyOrderPlacedAcceptsOrdersActualPayload is the regression for
// the order.placed.v1 contract mismatch. Order publishes
// `{order_id, customer_id, currency, total_minor, lines}` (see
// internal/order/service.go:orderPlacedPayload) and the consumer previously
// decoded it as `{order_id, currency, amount_minor}` under
// DisallowUnknownFields, which rejected the payload and read amount 0.
func TestConsumerApplyOrderPlacedAcceptsOrdersActualPayload(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	// applyEvent only touches consumer.service; a zero-value PostgresStore is
	// sufficient for the unit path (ProcessDelivery is not exercised here).
	consumer := &Consumer{store: &PostgresStore{}, service: fixture.service,
		policy: messaging.RetryPolicy{}}
	body, err := json.Marshal(map[string]any{
		"order_id":    "ORD-PLACED-1",
		"customer_id": "CUS-1",
		"currency":    "CNY",
		"total_minor": 3593,
		"lines": []map[string]any{{
			"order_item_id": "ITEM-1", "sku": "SKU-1", "quantity": 2,
			"unit_price_minor": 1236, "total_minor": 2472,
		}},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	event := messaging.NewEvent("order.placed.v1", "ORD-PLACED-1", "corr-1", "src-1",
		body, func() time.Time { return fixedPaymentClock() })

	if err := consumer.applyEvent(event); err != nil {
		t.Fatalf("applyOrderPlaced error: %v", err)
	}
	result, found, err := fixture.store.GetIntentByOrder(context.Background(), "ORD-PLACED-1")
	if err != nil {
		t.Fatalf("GetIntentByOrder error: %v", err)
	}
	if !found {
		t.Fatalf("intent not persisted for ORD-PLACED-1")
	}
	if result.AmountMinor != 3593 {
		t.Fatalf("AmountMinor=%d, want 3593 (total_minor from order.placed.v1)",
			result.AmountMinor)
	}
	if result.Currency != "CNY" {
		t.Fatalf("Currency=%q, want CNY", result.Currency)
	}
	if result.Status != StateSucceeded {
		t.Fatalf("Status=%q, want succeeded", result.Status)
	}
	if len(fixture.store.capturedEvents) != 1 ||
		fixture.store.capturedEvents[0].Type != EventPaymentCaptured {
		t.Fatalf("captured events=%+v, want single payment.captured.v1",
			eventTypes(fixture.store.capturedEvents))
	}
	if fixture.store.captureCalls != 1 {
		t.Fatalf("captureCalls=%d, want 1", fixture.store.captureCalls)
	}
}

// TestConsumerApplyOrderPlacedRejectsZeroTotal asserts the strict money
// validation is preserved when the payload carries a non-positive total.
func TestConsumerApplyOrderPlacedRejectsZeroTotal(t *testing.T) {
	fixture := newPaymentFixture(ScenarioCardDeclined)
	consumer := &Consumer{store: &PostgresStore{}, service: fixture.service,
		policy: messaging.RetryPolicy{}}
	body, _ := json.Marshal(map[string]any{
		"order_id": "ORD-BAD", "customer_id": "CUS-1",
		"currency": "CNY", "total_minor": 0, "lines": []map[string]any{},
	})
	event := messaging.NewEvent("order.placed.v1", "ORD-BAD", "corr", "", body,
		func() time.Time { return fixedPaymentClock() })
	err := consumer.applyEvent(event)
	if err == nil || !strings.Contains(err.Error(), "invalid order.placed money") {
		t.Fatalf("applyEvent error=%v, want invalid order.placed money", err)
	}
}

// TestWebhookAfterConsumerConvergesOnSameIntent covers Critical 2: an
// order.placed.v1 event that the consumer has already turned into a captured
// intent must not let the webhook handler insert a duplicate payment_intent.
// Both paths now share the "place:" idempotency-key namespace.
func TestWebhookAfterConsumerConvergesOnSameIntent(t *testing.T) {
	fixture := newPaymentFixture(ScenarioProviderTimeoutThenSuccess)
	service := NewService(fixture.store, fixture.provider,
		ServiceOptions{Clock: fixedPaymentClock})
	handler := NewHandler(service, nil) // store nil: webhook path does not need it

	// 1. Consumer processes order.placed.v1 first.
	consumer := &Consumer{store: &PostgresStore{}, service: service,
		policy: messaging.RetryPolicy{}}
	body, _ := json.Marshal(map[string]any{
		"order_id": "ORD-WEB-1", "customer_id": "CUS-1",
		"currency": "CNY", "total_minor": 4242,
		"lines": []map[string]any{{
			"order_item_id": "ITEM-1", "sku": "SKU-1", "quantity": 1,
			"unit_price_minor": 4242, "total_minor": 4242,
		}},
	})
	placedEvent := messaging.NewEvent("order.placed.v1", "ORD-WEB-1", "corr-w", "",
		body, func() time.Time { return fixedPaymentClock() })
	if err := consumer.applyEvent(placedEvent); err != nil {
		t.Fatalf("consumer.applyEvent error: %v", err)
	}
	if fixture.store.captureCalls != 1 {
		t.Fatalf("captureCalls=%d after consumer, want 1", fixture.store.captureCalls)
	}
	capturedBefore := len(fixture.store.capturedEvents)

	// 2. Webhook arrives for the same order.
	webhookReq := httptest.NewRequest(http.MethodPost, "/api/v1/payments/webhooks",
		strings.NewReader(`{"order_id":"ORD-WEB-1","currency":"CNY","amount_minor":4242}`))
	webhookReq.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, webhookReq)

	if recorder.Code != http.StatusOK {
		t.Fatalf("webhook status=%d, want 200 (replay)", recorder.Code)
	}
	var view IntentView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode webhook response: %v", err)
	}
	if view.OrderID != "ORD-WEB-1" {
		t.Fatalf("view.OrderID=%q, want ORD-WEB-1", view.OrderID)
	}
	if view.Status != StateSucceeded {
		t.Fatalf("view.Status=%q, want succeeded", view.Status)
	}
	if view.PaymentIntentID != deterministicIntentID(placeIntentKey("ORD-WEB-1")) {
		t.Fatalf("webhook intent_id=%q, want %q (same as consumer)",
			view.PaymentIntentID, deterministicIntentID(placeIntentKey("ORD-WEB-1")))
	}
	// The webhook MUST be treated as a replay: no new provider run, no new
	// capture event in the outbox.
	if fixture.store.captureCalls != 1 {
		t.Fatalf("captureCalls=%d after webhook, want 1 (webhook must replay)",
			fixture.store.captureCalls)
	}
	if len(fixture.store.capturedEvents) != capturedBefore {
		t.Fatalf("captured events=%d after webhook, want %d (no duplicate capture)",
			len(fixture.store.capturedEvents), capturedBefore)
	}
}
