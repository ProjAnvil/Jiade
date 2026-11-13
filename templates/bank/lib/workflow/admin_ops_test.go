package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// These tests exercise the operator-driven compensation recovery operations
// (RetryCompensation, ResolveCompensation) added for the protected admin gRPC
// surface. They reuse the in-memory Store + scriptedAction fixtures from
// engine_test.go. The scriptedAction's Compensate is configurable via compDisp
// and its ApplyCompensationResult consumes compOutcomes left-to-right.

// runToCompensationFailed drives a 2-action workflow (step-1 succeeds, step-2
// fails terminally) and exhausts step-1's automatic compensation retries so the
// instance lands in StatusCompensationFailed with the current action
// (step-1) in ActionCompensationFailed. Returns the engine, store, and the
// step-1 scripted action so tests can assert on compensation dispatches.
func runToCompensationFailed(t *testing.T, workflowID string) (*Engine, *memoryStore, *scriptedAction) {
	t.Helper()
	step1 := &scriptedAction{
		name:           "step-1",
		forwardDisp:    fwdDispatch("step-1"),
		forwardOutcome: okOutcome(),
		compDisp:       compDispatch("step-1"),
		// Exhaust the default 5 automatic compensation retries.
		compOutcomes: []Outcome{
			transientOutcome("f1"), transientOutcome("f2"),
			transientOutcome("f3"), transientOutcome("f4"),
			transientOutcome("f5"),
		},
	}
	step2 := &scriptedAction{
		name:           "step-2",
		forwardDisp:    fwdDispatch("step-2"),
		forwardOutcome: rejectedOutcome("nope"),
	}
	def := linearDef{
		workflowType: "retry-flow",
		version:      1,
		actions:      []Action{step1, step2},
	}
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: workflowID, Type: "retry-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	deliverForward(t, engine, store, workflowID, "step-1", 0)
	deliverForwardFailure(t, engine, store, workflowID, "step-2", 1)
	const maxAttempts = 5
	for i := 1; i <= maxAttempts; i++ {
		deliverCompensation(t, engine, store, workflowID, "step-1", 0)
	}

	got := store.instance(workflowID)
	assertStatus(t, got, StatusCompensationFailed)
	assertActionStatus(t, got, 0, ActionCompensationFailed)
	return engine, store, step1
}

func TestRetryCompensation_RedispachesAfterFailure(t *testing.T) {
	engine, store, step1 := runToCompensationFailed(t, "wf-retry")
	before := store.outboxCount()
	beforeAttempt := store.instance("wf-retry").Actions[0].Attempt

	prev, curr, err := engine.RetryCompensation(context.Background(), "wf-retry")
	if err != nil {
		t.Fatalf("RetryCompensation: %v", err)
	}
	if prev != StatusCompensationFailed || curr != StatusCompensating {
		t.Fatalf("transition = %q→%q, want %q→%q", prev, curr, StatusCompensationFailed, StatusCompensating)
	}
	got := store.instance("wf-retry")
	assertStatus(t, got, StatusCompensating)
	assertActionStatus(t, got, 0, ActionCompensating)
	// Fresh compensation command appended to the outbox.
	if store.outboxCount() != before+1 {
		t.Fatalf("outbox = %d, want %d (one new compensation dispatch)", store.outboxCount(), before+1)
	}
	// Attempt must increment past the exhausted value.
	if got.Actions[0].Attempt != beforeAttempt+1 {
		t.Errorf("Attempt = %d, want %d", got.Actions[0].Attempt, beforeAttempt+1)
	}
	if step1.compCalls == 0 {
		t.Errorf("Compensate was never called on retry")
	}
}

func TestRetryCompensation_RejectsNonFailedInstance(t *testing.T) {
	// A freshly-prepared instance is StatusRunning, not compensation_failed.
	store := newMemoryStore()
	def := linearDef{
		workflowType: "x", version: 1,
		actions: []Action{
			&scriptedAction{name: "a", forwardDisp: fwdDispatch("a"), forwardOutcome: okOutcome(), compDisp: compDispatch("a")},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	inst, err := engine.Start(context.Background(), StartRequest{WorkflowID: "wf", Type: "x", Version: 1, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}

	_, _, err = engine.RetryCompensation(context.Background(), "wf")
	if err == nil || !strings.Contains(err.Error(), string(StatusCompensationFailed)) {
		t.Fatalf("err = %v, want a not-compensation_failed error", err)
	}
}

func TestResolveCompensation_MarksActionCompensatedAndFinishes(t *testing.T) {
	// A 2-action flow: "only" succeeds forward, "next" fails terminally, so the
	// engine begins compensating "only". "only"'s compensation exhausts retries
	// → compensation_failed. Resolving "only" (no prior succeeded action to
	// walk to) finishes the instance.
	step1 := &scriptedAction{
		name:           "only",
		forwardDisp:    fwdDispatch("only"),
		forwardOutcome: okOutcome(),
		compDisp:       compDispatch("only"),
		compOutcomes: []Outcome{
			transientOutcome("f1"), transientOutcome("f2"),
			transientOutcome("f3"), transientOutcome("f4"),
			transientOutcome("f5"),
		},
	}
	step2 := &scriptedAction{name: "next", forwardDisp: fwdDispatch("next"), forwardOutcome: rejectedOutcome("no")}
	def := linearDef{workflowType: "one", version: 1, actions: []Action{step1, step2}}
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	inst, err := engine.Start(context.Background(), StartRequest{WorkflowID: "wf-r", Type: "one", Version: 1, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}
	deliverForward(t, engine, store, "wf-r", "only", 0)
	deliverForwardFailure(t, engine, store, "wf-r", "next", 1)
	for i := 0; i < 5; i++ {
		deliverCompensation(t, engine, store, "wf-r", "only", 0)
	}
	got := store.instance("wf-r")
	assertStatus(t, got, StatusCompensationFailed)
	assertActionStatus(t, got, 0, ActionCompensationFailed)

	before := store.outboxCount()
	prev, curr, err := engine.ResolveCompensation(context.Background(), "wf-r", "only")
	if err != nil {
		t.Fatalf("ResolveCompensation: %v", err)
	}
	if prev != StatusCompensationFailed || curr != StatusCompensated {
		t.Fatalf("transition = %q→%q, want %q→%q", prev, curr, StatusCompensationFailed, StatusCompensated)
	}
	got = store.instance("wf-r")
	assertStatus(t, got, StatusCompensated)
	assertActionStatus(t, got, 0, ActionCompensated)
	// No prior succeeded action → no further compensation dispatch.
	if store.outboxCount() != before {
		t.Fatalf("outbox = %d, want %d (no further dispatch)", store.outboxCount(), before)
	}
}

func TestResolveCompensation_WalksToPreviousSucceededAction(t *testing.T) {
	// 3-action flow: step-1 + step-2 succeed forward, step-3 fails terminally.
	// step-2 compensation exhausts → compensation_failed at step-2. Resolving
	// step-2 must dispatch step-1's compensation (the previous succeeded).
	step1 := &scriptedAction{name: "step-1", forwardDisp: fwdDispatch("step-1"), forwardOutcome: okOutcome(), compDisp: compDispatch("step-1")}
	step2 := &scriptedAction{
		name: "step-2", forwardDisp: fwdDispatch("step-2"), forwardOutcome: okOutcome(), compDisp: compDispatch("step-2"),
		compOutcomes: []Outcome{transientOutcome("f1"), transientOutcome("f2"), transientOutcome("f3"), transientOutcome("f4"), transientOutcome("f5")},
	}
	step3 := &scriptedAction{name: "step-3", forwardDisp: fwdDispatch("step-3"), forwardOutcome: rejectedOutcome("no")}
	def := linearDef{workflowType: "three", version: 1, actions: []Action{step1, step2, step3}}
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	inst, err := engine.Start(context.Background(), StartRequest{WorkflowID: "wf-3", Type: "three", Version: 1, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}
	deliverForward(t, engine, store, "wf-3", "step-1", 0)
	deliverForward(t, engine, store, "wf-3", "step-2", 1)
	deliverForwardFailure(t, engine, store, "wf-3", "step-3", 2)
	for i := 0; i < 5; i++ {
		deliverCompensation(t, engine, store, "wf-3", "step-2", 1)
	}
	got := store.instance("wf-3")
	assertStatus(t, got, StatusCompensationFailed)
	if got.CurrentAction != 1 {
		t.Fatalf("CurrentAction = %d, want 1", got.CurrentAction)
	}

	before := store.outboxCount()
	_, curr, err := engine.ResolveCompensation(context.Background(), "wf-3", "step-2")
	if err != nil {
		t.Fatalf("ResolveCompensation: %v", err)
	}
	if curr != StatusCompensating {
		t.Fatalf("new status = %q, want %q (walk continues)", curr, StatusCompensating)
	}
	got = store.instance("wf-3")
	assertStatus(t, got, StatusCompensating)
	assertActionStatus(t, got, 1, ActionCompensated)
	if got.CurrentAction != 0 {
		t.Fatalf("CurrentAction = %d, want 0 (walked to step-1)", got.CurrentAction)
	}
	assertActionStatus(t, got, 0, ActionCompensating)
	// step-1 compensation dispatched.
	if store.outboxCount() != before+1 {
		t.Fatalf("outbox = %d, want %d (step-1 compensation dispatched)", store.outboxCount(), before+1)
	}
	last := store.outbox[store.outboxCount()-1]
	if last.env.ActionName != "step-1" {
		t.Fatalf("dispatched action = %q, want step-1", last.env.ActionName)
	}
}

func TestResolveCompensation_RejectsUnknownAction(t *testing.T) {
	engine, _, _ := runToCompensationFailed(t, "wf-unk")
	_, _, err := engine.ResolveCompensation(context.Background(), "wf-unk", "no-such-action")
	if err == nil || !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("err = %v, want ErrInvalidMessage", err)
	}
}

func TestResolveCompensation_RejectsHealthyInstance(t *testing.T) {
	store := newMemoryStore()
	def := linearDef{workflowType: "h", version: 1, actions: []Action{
		&scriptedAction{name: "a", forwardDisp: fwdDispatch("a"), forwardOutcome: okOutcome(), compDisp: compDispatch("a")},
	}}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	inst, err := engine.Start(context.Background(), StartRequest{WorkflowID: "wf-h", Type: "h", Version: 1, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}
	_, _, err = engine.ResolveCompensation(context.Background(), "wf-h", "a")
	if err == nil || !strings.Contains(err.Error(), "is not") {
		t.Fatalf("err = %v, want a status-precondition error", err)
	}
}

func TestResolveCompensation_RunsWithinCallerTx(t *testing.T) {
	// The Tx-form runs against an instance already locked by WithInstance.
	engine, store, _ := runToCompensationFailed(t, "wf-tx")
	var prev, curr InstanceStatus
	err := store.WithInstance(context.Background(), "wf-tx", func(tx Tx) error {
		var e error
		prev, curr, e = engine.ResolveCompensationTx(tx, context.Background(), "step-1")
		return e
	})
	if err != nil {
		t.Fatalf("ResolveCompensationTx: %v", err)
	}
	if prev != StatusCompensationFailed || curr != StatusCompensated {
		t.Fatalf("transition = %q→%q, want compensation_failed→compensated", prev, curr)
	}
}
