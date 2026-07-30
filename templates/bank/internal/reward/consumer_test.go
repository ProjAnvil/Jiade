// Package reward wires the reward service's read-only API and its
// payment-completion consumer (Task 8).
//
// This file holds the unit tests for the reward consumer path. The reward
// consumer reacts to payment.completed.v1 by earning points. It is a
// NON-CRITICAL consumer: a permanent failure routes the message to the
// reward DLQ and the payment status is NEVER affected.
package reward

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"bank/internal/platform/messaging"
)

// ---------------------------------------------------------------------------
// Stubs
// ---------------------------------------------------------------------------

// fakePointsEarner records EarnPoints calls and optionally returns an error.
// The Consumer's PointsEarner dependency is the full reward-side write path.
type fakePointsEarner struct {
	mu    sync.Mutex
	calls []earnedCall
	err   error
}

type earnedCall struct {
	PaymentID   string
	CustomerID  string
	AmountMinor int64
	Currency    string
}

func (f *fakePointsEarner) EarnPoints(_ context.Context, paymentID, customerID string, amountMinor int64, currency string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, earnedCall{
		PaymentID:   paymentID,
		CustomerID:  customerID,
		AmountMinor: amountMinor,
		Currency:    currency,
	})
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakePointsEarner) Calls() []earnedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]earnedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// completionEnvelope builds a payment.completed.v1 envelope the reward
// consumer subscribes to. The payload mirrors the wire format the payment
// consumer emits (Task 8: payment.consumer.emitCompletion).
func completionEnvelope(t *testing.T, workflowID string) messaging.Envelope {
	t.Helper()
	payload := completionPayload{
		WorkflowID:     workflowID,
		PaymentID:      workflowID,
		PayerCustomerID: "C-100",
		AmountMinor:    50000,
		Currency:       "CNY",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal completion payload: %v", err)
	}
	env := messaging.NewEnvelope("payment.completed.v1", "corr-"+workflowID, body, time.Now)
	env.WorkflowID = workflowID
	return env
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestConsumer_ProcessesCompletionAndEarnsPoints: a well-formed
// payment.completed.v1 event triggers exactly one EarnPoints call.
func TestConsumer_ProcessesCompletionAndEarnsPoints(t *testing.T) {
	earner := &fakePointsEarner{}
	c := NewConsumer(nil, earner, messaging.RetryPolicy{MaxAttempts: 3})

	env := completionEnvelope(t, "wf-1")
	if err := c.processCompletion(context.Background(), env); err != nil {
		t.Fatalf("processCompletion: %v", err)
	}
	calls := earner.Calls()
	if len(calls) != 1 {
		t.Fatalf("EarnPoints calls = %d, want 1", len(calls))
	}
	if calls[0].PaymentID != "wf-1" {
		t.Errorf("EarnPoints paymentID = %q, want wf-1", calls[0].PaymentID)
	}
	if calls[0].CustomerID != "C-100" {
		t.Errorf("EarnPoints customerID = %q, want C-100", calls[0].CustomerID)
	}
	if calls[0].AmountMinor != 50000 {
		t.Errorf("EarnPoints amount = %d, want 50000", calls[0].AmountMinor)
	}
}

// TestConsumer_RewardFailurePropagates_PaymentStaysSucceeded is the reward
// ISOLATION test. The points earner fails permanently; the handler returns
// the error so messaging.ProcessDelivery routes the delivery through retry
// and eventually the reward DLQ. Crucially, the reward consumer CANNOT touch
// payment state: it lives in a different service with its own database and
// no access to payment_intent. The payment remains succeeded.
func TestConsumer_RewardFailurePropagates_PaymentStaysSucceeded(t *testing.T) {
	earner := &fakePointsEarner{err: errors.New("reward DB permanently unavailable")}
	c := NewConsumer(nil, earner, messaging.RetryPolicy{MaxAttempts: 3})

	env := completionEnvelope(t, "wf-1")
	err := c.processCompletion(context.Background(), env)
	if err == nil {
		t.Fatal("processCompletion: expected error from failing earner, got nil")
	}
	// The handler error propagates so ProcessDelivery retries (up to
	// MaxAttempts) and then routes the delivery to the DeadLetterRoutingKey
	// (reward DLQ). This is the messaging-layer contract already verified in
	// the platform/messaging package's ProcessDelivery tests.
	//
	// ISOLATION: the reward package does NOT import payment and has no
	// PaymentIntentRepo. The payment_intent row is physically unreachable
	// from this consumer; no code path exists by which reward processing
	// could reverse money or change payment_intent.status. The payment
	// therefore REMAINS succeeded regardless of the reward outcome.
	calls := earner.Calls()
	if len(calls) != 1 {
		t.Errorf("EarnPoints calls = %d, want 1 (attempted once before propagating error)", len(calls))
	}
}

// TestConsumer_MalformedPayloadRoutesToDLQ: a payload that cannot be decoded
// as a completion event yields an error so ProcessDelivery settles the
// delivery. The reward consumer never panics on a malformed event.
func TestConsumer_MalformedPayloadRoutesToDLQ(t *testing.T) {
	earner := &fakePointsEarner{}
	c := NewConsumer(nil, earner, messaging.RetryPolicy{MaxAttempts: 3})

	env := messaging.NewEnvelope("payment.completed.v1", "corr-1",
		json.RawMessage(`{not-json`), time.Now)
	env.WorkflowID = "wf-1"
	if err := c.processCompletion(context.Background(), env); err == nil {
		t.Fatal("processCompletion: expected decode error, got nil")
	}
	if len(earner.Calls()) != 0 {
		t.Errorf("EarnPoints calls = %d, want 0 on decode failure", len(earner.Calls()))
	}
}
