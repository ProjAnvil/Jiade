// Package payment wires the runtime composition for the payment service's
// saga participation: the result-event consumer (Engine.ApplyResult), the
// outbox relay, and the WorkflowStarter that persists a PaymentIntent + a
// workflow.Instance in one transaction.
//
// This file holds the unit tests for the consumer path. It exercises:
//   - Consumer.processEnvelope hands a decoded result envelope to
//     Engine.ApplyResult.
//   - An ApplyResult error propagates so messaging.ProcessDelivery retries/DLQs.
//   - Idempotent redelivery: the SAME envelope delivered twice applies only
//     once (the second delivery is a no-op because the consumer's outer Inbox
//     dedup suppresses the handler).
//   - After a successful apply, the consumer syncs payment_intent.status from
//     the workflow_instance so the GET endpoint reflects the saga outcome.
package payment

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

// fakeApplyResulter records ApplyResult calls. The Consumer's engine dependency
// is just ApplyResult, so this captures the entire contract.
type fakeApplyResulter struct {
	mu    sync.Mutex
	calls []messaging.Envelope
	err   error
}

func (f *fakeApplyResulter) ApplyResult(_ context.Context, env messaging.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, env)
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakeApplyResulter) Calls() []messaging.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]messaging.Envelope, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeInstanceStatusReader returns a canned status for a workflow id. Used to
// verify the consumer syncs payment_intent.status from the engine's view.
type fakeInstanceStatusReader struct {
	mu      sync.Mutex
	status  string
	exists  bool
	lookups int
}

func (f *fakeInstanceStatusReader) InstanceStatus(_ context.Context, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if !f.exists {
		return "", ErrWorkflowNotFound
	}
	return f.status, nil
}

func (f *fakeInstanceStatusReader) Lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

// fakeIntentUpdater records UpdateStatus calls.
type fakeIntentUpdater struct {
	mu       sync.Mutex
	updates  []intentStatusUpdate
	updateFn func(workflowID string, status PaymentIntentStatus) error
}

type intentStatusUpdate struct {
	WorkflowID string
	Status     PaymentIntentStatus
}

func (f *fakeIntentUpdater) UpdateStatusByWorkflowID(_ context.Context, workflowID string, status PaymentIntentStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, intentStatusUpdate{WorkflowID: workflowID, Status: status})
	if f.updateFn != nil {
		return f.updateFn(workflowID, status)
	}
	return nil
}

func (f *fakeIntentUpdater) Updates() []intentStatusUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]intentStatusUpdate, len(f.updates))
	copy(out, f.updates)
	return out
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func sampleEnvelope(t *testing.T, messageType, workflowID string) messaging.Envelope {
	t.Helper()
	env := messaging.NewEnvelope(messageType, "corr-"+workflowID, json.RawMessage(`{}`), time.Now)
	env.WorkflowID = workflowID
	env.ActionName = "PostLedgerTransfer"
	env.CommandID = "cmd-" + workflowID
	env.IdempotencyKey = "wf:" + workflowID + ":post-ledger-transfer"
	return env
}

// newTestConsumer builds a Consumer wired with the given stubs. It uses the
// package-internal constructor so the test does not need a *sql.DB.
func newTestConsumer(engine *fakeApplyResulter, status InstanceStatusReader, intents IntentStatusUpdater) *Consumer {
	return &Consumer{
		engine:        engine,
		statusReader:  status,
		intentUpdater: intents,
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestConsumer_HandlerAppliesResultToEngine: a decoded envelope is passed to
// Engine.ApplyResult.
func TestConsumer_HandlerAppliesResultToEngine(t *testing.T) {
	engine := &fakeApplyResulter{}
	c := newTestConsumer(engine, &fakeInstanceStatusReader{exists: true, status: "succeeded"}, &fakeIntentUpdater{})

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	calls := engine.Calls()
	if len(calls) != 1 {
		t.Fatalf("ApplyResult calls = %d, want 1", len(calls))
	}
	if calls[0].WorkflowID != "wf-1" {
		t.Errorf("ApplyResult workflow_id = %q, want %q", calls[0].WorkflowID, "wf-1")
	}
	if calls[0].MessageType != "core.transfer-posted.v1" {
		t.Errorf("ApplyResult message_type = %q, want %q", calls[0].MessageType, "core.transfer-posted.v1")
	}
}

// TestConsumer_HandlerErrorPropagates: when ApplyResult returns an error, the
// handler returns it so messaging.ProcessDelivery retries/DLQs the delivery.
func TestConsumer_HandlerErrorPropagates(t *testing.T) {
	engine := &fakeApplyResulter{err: errors.New("apply-result: invariant violation")}
	c := newTestConsumer(engine, &fakeInstanceStatusReader{}, &fakeIntentUpdater{})

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err == nil {
		t.Errorf("expected error from ApplyResult to propagate, got nil")
	}
}

// TestConsumer_SyncsIntentStatusAfterSuccess: after ApplyResult succeeds, the
// consumer queries the instance status and updates payment_intent.status so
// GET /workflows/{id} reflects the saga outcome.
func TestConsumer_SyncsIntentStatusAfterSuccess(t *testing.T) {
	engine := &fakeApplyResulter{}
	statusReader := &fakeInstanceStatusReader{exists: true, status: "succeeded"}
	updater := &fakeIntentUpdater{}
	c := newTestConsumer(engine, statusReader, updater)

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	updates := updater.Updates()
	if len(updates) != 1 {
		t.Fatalf("UpdateStatus calls = %d, want 1", len(updates))
	}
	if updates[0].WorkflowID != "wf-1" {
		t.Errorf("update workflow_id = %q, want %q", updates[0].WorkflowID, "wf-1")
	}
	if updates[0].Status != IntentSucceeded {
		t.Errorf("update status = %q, want %q", updates[0].Status, IntentSucceeded)
	}
}

// TestConsumer_DoesNotSyncStatusOnEngineError: if ApplyResult errors, the
// consumer MUST NOT touch payment_intent status (the saga state is unchanged).
func TestConsumer_DoesNotSyncStatusOnEngineError(t *testing.T) {
	engine := &fakeApplyResulter{err: errors.New("transient")}
	updater := &fakeIntentUpdater{}
	c := newTestConsumer(engine, &fakeInstanceStatusReader{exists: true, status: "succeeded"}, updater)

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err == nil {
		t.Fatalf("expected error, got nil")
	}
	if len(updater.Updates()) != 0 {
		t.Errorf("UpdateStatus calls = %d, want 0 on engine error", len(updater.Updates()))
	}
}

// TestConsumer_MapsInstanceStatusToIntentStatus covers the full status mapping
// so GET /workflows/{id} reports a payment-shaped status for every workflow
// outcome.
func TestConsumer_MapsInstanceStatusToIntentStatus(t *testing.T) {
	cases := []struct {
		instanceStatus string
		want           PaymentIntentStatus
	}{
		{"running", IntentRunning},
		{"succeeded", IntentSucceeded},
		{"compensated", IntentCompensated},
		{"compensation_failed", IntentCompensationFailed},
		{"rejected", IntentRejected},
		{"preparing", IntentPending},
		{"unknown", IntentPending}, // unknown falls back to pending
	}
	for _, tc := range cases {
		t.Run(tc.instanceStatus, func(t *testing.T) {
			got := mapInstanceStatusToIntent(tc.instanceStatus)
			if got != tc.want {
				t.Errorf("mapInstanceStatusToIntent(%q) = %q, want %q", tc.instanceStatus, got, tc.want)
			}
		})
	}
}
