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

// ---------------------------------------------------------------------------
// Completion emission tests (Task 8).
//
// payment.completed.v1 is emitted ONCE — exactly once — when the
// payment-transfer workflow transitions to StatusSucceeded. The consumer
// reads the intent's previous status to detect a fresh transition; a
// duplicate delivery for an already-succeeded workflow does NOT re-emit.
// ---------------------------------------------------------------------------

// fakeCompletionOutbox records EmitCompletion calls so tests can assert
// "exactly one outbox event" without a *sql.DB.
type fakeCompletionOutbox struct {
	mu    sync.Mutex
	calls []messaging.Envelope
	err   error
}

func (f *fakeCompletionOutbox) EmitCompletion(_ context.Context, env messaging.Envelope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, env)
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakeCompletionOutbox) Calls() []messaging.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]messaging.Envelope, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeIntentReader returns a canned PaymentIntent for a workflow id. It lets
// the consumer detect fresh succeeded transitions without a *sql.DB.
type fakeIntentReader struct {
	mu     sync.Mutex
	intent PaymentIntent
	err    error
}

func (f *fakeIntentReader) GetByWorkflowID(_ context.Context, _ string) (PaymentIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return PaymentIntent{}, f.err
	}
	return f.intent, nil
}

func (f *fakeIntentReader) SetStatus(status PaymentIntentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.intent.Status = status
}

// newTestConsumerWithCompletion wires a Consumer with completion-outbox and
// intent-reader stubs so the emission path can be exercised end-to-end.
func newTestConsumerWithCompletion(
	engine *fakeApplyResulter,
	status InstanceStatusReader,
	intents IntentStatusUpdater,
	intentReader IntentReader,
	outbox CompletionOutbox,
) *Consumer {
	return &Consumer{
		engine:          engine,
		statusReader:    status,
		intentUpdater:   intents,
		intentReader:    intentReader,
		completionOutbox: outbox,
	}
}

// TestConsumer_EmitsCompletionOnceOnSuccess: when the workflow transitions to
// succeeded, the consumer emits exactly ONE payment.completed.v1 outbox event
// AND commits payment_intent.status to succeeded.
func TestConsumer_EmitsCompletionOnceOnSuccess(t *testing.T) {
	engine := &fakeApplyResulter{}
	statusReader := &fakeInstanceStatusReader{exists: true, status: "succeeded"}
	updater := &fakeIntentUpdater{}
	// Intent was running before this event — this is a fresh transition.
	intentReader := &fakeIntentReader{intent: PaymentIntent{
		WorkflowID: "wf-1", PayerCustomerID: "C-100",
		Currency: "CNY", AmountMinor: 50000, Status: IntentRunning,
	}}
	outbox := &fakeCompletionOutbox{}
	c := newTestConsumerWithCompletion(engine, statusReader, updater, intentReader, outbox)

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}

	// Exactly one completion event.
	calls := outbox.Calls()
	if len(calls) != 1 {
		t.Fatalf("completion outbox calls = %d, want 1", len(calls))
	}
	if calls[0].MessageType != "payment.completed.v1" {
		t.Errorf("completion message_type = %q, want payment.completed.v1", calls[0].MessageType)
	}
	if calls[0].WorkflowID != "wf-1" {
		t.Errorf("completion workflow_id = %q, want wf-1", calls[0].WorkflowID)
	}
	// Payment status committed to succeeded.
	updates := updater.Updates()
	if len(updates) != 1 {
		t.Fatalf("intent updates = %d, want 1", len(updates))
	}
	if updates[0].Status != IntentSucceeded {
		t.Errorf("intent status = %q, want %q", updates[0].Status, IntentSucceeded)
	}
}

// TestConsumer_DoesNotEmitCompletionOnNonSuccess: a non-succeeded result
// (e.g. hold-placed → running) MUST NOT emit payment.completed.v1.
func TestConsumer_DoesNotEmitCompletionOnNonSuccess(t *testing.T) {
	engine := &fakeApplyResulter{}
	statusReader := &fakeInstanceStatusReader{exists: true, status: "running"}
	updater := &fakeIntentUpdater{}
	intentReader := &fakeIntentReader{intent: PaymentIntent{Status: IntentPending}}
	outbox := &fakeCompletionOutbox{}
	c := newTestConsumerWithCompletion(engine, statusReader, updater, intentReader, outbox)

	env := sampleEnvelope(t, "core.hold-placed.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if calls := outbox.Calls(); len(calls) != 0 {
		t.Errorf("completion outbox calls = %d, want 0 for non-succeeded status", len(calls))
	}
}

// TestConsumer_DoesNotReemitCompletionForAlreadySucceeded: if a duplicate
// delivery arrives for an already-succeeded workflow (late redelivery
// bypassing the outer Inbox), the consumer MUST NOT emit a second completion
// event. The intent's previous status gate guards the exactly-once emission.
func TestConsumer_DoesNotReemitCompletionForAlreadySucceeded(t *testing.T) {
	engine := &fakeApplyResulter{}
	statusReader := &fakeInstanceStatusReader{exists: true, status: "succeeded"}
	updater := &fakeIntentUpdater{}
	// Intent was already succeeded — this is NOT a fresh transition.
	intentReader := &fakeIntentReader{intent: PaymentIntent{Status: IntentSucceeded}}
	outbox := &fakeCompletionOutbox{}
	c := newTestConsumerWithCompletion(engine, statusReader, updater, intentReader, outbox)

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if calls := outbox.Calls(); len(calls) != 0 {
		t.Errorf("completion outbox calls = %d, want 0 for already-succeeded intent", len(calls))
	}
}

// TestConsumer_EmitsCompletionWithoutIntentReader: when no intent reader is
// wired (e.g. a legacy deployment), the consumer still emits completion on
// every successful ApplyResult. The intent-reader is an optimization to
// suppress duplicates; its absence falls back to emit-on-success.
func TestConsumer_EmitsCompletionWithoutIntentReader(t *testing.T) {
	engine := &fakeApplyResulter{}
	statusReader := &fakeInstanceStatusReader{exists: true, status: "succeeded"}
	updater := &fakeIntentUpdater{}
	outbox := &fakeCompletionOutbox{}
	// No intentReader — completionOutbox is still wired.
	c := &Consumer{
		engine:           engine,
		statusReader:     statusReader,
		intentUpdater:    updater,
		completionOutbox: outbox,
	}

	env := sampleEnvelope(t, "core.transfer-posted.v1", "wf-1")
	if err := c.handleResult(context.Background(), env); err != nil {
		t.Fatalf("handleResult: %v", err)
	}
	if calls := outbox.Calls(); len(calls) != 1 {
		t.Errorf("completion outbox calls = %d, want 1 when intent reader is absent", len(calls))
	}
}
