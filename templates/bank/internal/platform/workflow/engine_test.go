package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"bank/internal/platform/messaging"
)

// ---------------------------------------------------------------------------
// In-memory Store + Tx for engine unit tests. Simulates transactional
// semantics: writes inside WithInstance are buffered in the Tx's working copy
// and committed to the store only when the callback returns nil; an error
// return discards the buffer (rollback).
// ---------------------------------------------------------------------------

type outboxEntry struct {
	env        messaging.Envelope
	routingKey string
}

type inboxEntry struct {
	consumer  string
	messageID string
}

type memoryStore struct {
	mu        sync.Mutex
	instances map[string]*Instance
	outbox    []outboxEntry
	inbox     map[string]map[string]struct{}

	// appendOutboxHook, if non-nil, is invoked before each AppendOutbox; a
	// non-nil error aborts the in-flight Tx (rollback).
	appendOutboxHook func() error
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		instances: make(map[string]*Instance),
		inbox:     make(map[string]map[string]struct{}),
	}
}

// Create persists a new Instance in StatusPreparing and returns a snapshot.
func (s *memoryStore) Create(_ context.Context, req StartRequest) (Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.instances[req.WorkflowID]; exists {
		return Instance{}, ErrInstanceExists
	}
	inst := Instance{
		ID:      req.WorkflowID,
		Type:    req.Type,
		Version: req.Version,
		Status:  StatusPreparing,
		Input:   append(json.RawMessage(nil), req.Input...),
	}
	s.instances[req.WorkflowID] = &inst
	return inst, nil
}

// WithInstance loads the instance, runs fn against a working-copy Tx, and
// commits the buffered writes on success (rollback on error/panic).
func (s *memoryStore) WithInstance(_ context.Context, id string, fn func(Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return ErrInstanceNotFound
	}
	tx := &memoryTx{store: s, working: *inst}
	if err := fn(tx); err != nil {
		return err
	}
	tx.commit(inst)
	return nil
}

// instance returns a snapshot copy for test assertions. Not part of Store.
func (s *memoryStore) instance(id string) Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst, ok := s.instances[id]; ok {
		return *inst
	}
	return Instance{}
}

func (s *memoryStore) outboxCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.outbox)
}

// memoryTx implements Tx over a private working copy. Instance/Action writes
// mutate the working copy directly; outbox/inbox writes buffer into sidecar
// slices and flush on commit. An error return from the WithInstance callback
// discards the working copy (rollback).
type memoryTx struct {
	store          *memoryStore
	working        Instance
	bufferedOutbox []outboxEntry
	bufferedInbox  []inboxEntry
}

func (t *memoryTx) Instance() *Instance { return &t.working }

func (t *memoryTx) InsertInbox(consumer string, env messaging.Envelope) (bool, error) {
	if set, ok := t.store.inbox[consumer]; ok {
		if _, dup := set[env.MessageID]; dup {
			return false, nil
		}
	}
	// Also dedup against inserts buffered earlier in this same Tx.
	for _, ie := range t.bufferedInbox {
		if ie.consumer == consumer && ie.messageID == env.MessageID {
			return false, nil
		}
	}
	t.bufferedInbox = append(t.bufferedInbox, inboxEntry{consumer, env.MessageID})
	return true, nil
}

func (t *memoryTx) SaveInstance(inst Instance) error {
	t.working = inst
	return nil
}

func (t *memoryTx) SaveAction(rec ActionRecord) error {
	actions := t.working.Actions
	for len(actions) <= rec.Index {
		actions = append(actions, ActionRecord{Index: len(actions)})
	}
	actions[rec.Index] = rec
	t.working.Actions = actions
	return nil
}

func (t *memoryTx) AppendOutbox(env messaging.Envelope, routingKey string) error {
	if t.store.appendOutboxHook != nil {
		if err := t.store.appendOutboxHook(); err != nil {
			return err
		}
	}
	t.bufferedOutbox = append(t.bufferedOutbox, outboxEntry{env: env, routingKey: routingKey})
	return nil
}

// commit copies the working Instance back over the store's pointer and flushes
// buffered outbox/inbox writes. Called by WithInstance only on success.
func (t *memoryTx) commit(storeInst *Instance) {
	*storeInst = t.working
	t.store.outbox = append(t.store.outbox, t.bufferedOutbox...)
	for _, ie := range t.bufferedInbox {
		set, ok := t.store.inbox[ie.consumer]
		if !ok {
			set = make(map[string]struct{})
			t.store.inbox[ie.consumer] = set
		}
		set[ie.messageID] = struct{}{}
	}
}

// ---------------------------------------------------------------------------
// Fake definitions / actions driving the tests.
// ---------------------------------------------------------------------------

// linearDefinition returns a 2-action workflow whose Prepare echoes the input
// and whose actions emit deterministic dispatches. Used for the happy-path and
// retry tests.
func linearDefinition() Definition {
	return linearDef{
		workflowType: "payment-transfer",
		version:      1,
		actions: []Action{
			linearAction{name: "book-transfer", routingKey: "bookings.cmd"},
			linearAction{name: "settle-transfer", routingKey: "settlements.cmd"},
		},
	}
}

type linearDef struct {
	workflowType string
	version      int
	actions      []Action
}

func (d linearDef) Type() string { return d.workflowType }
func (d linearDef) Version() int { return d.version }
func (d linearDef) Prepare(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	if len(input) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return append(json.RawMessage(nil), input...), nil
}
func (d linearDef) Actions() []Action { return d.actions }

type linearAction struct {
	name       string
	routingKey string
}

func (a linearAction) Name() string { return a.name }
func (a linearAction) Execute(_ context.Context, _ View) (Dispatch, error) {
	return Dispatch{
		RoutingKey:          a.routingKey,
		Payload:             json.RawMessage(`{}`),
		IdempotencyKey:      a.name + "-idem-key",
		AcceptedResultTypes: []string{"result." + a.name},
		Deadline:            30 * time.Second,
	}, nil
}
func (a linearAction) ApplyResult(_ context.Context, _ View, _ messaging.Envelope) (Outcome, error) {
	return Outcome{Succeeded: true, Output: json.RawMessage(`{"ok":true}`)}, nil
}
func (a linearAction) Compensate(context.Context, View) (Dispatch, error) {
	return Dispatch{}, nil
}
func (a linearAction) ApplyCompensationResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	return Outcome{}, nil
}

// rejectingDefinition returns a Definition whose Prepare always fails, used to
// exercise the StatusRejected transition.
func rejectingDefinition() Definition {
	return rejectingDef{}
}

type rejectingDef struct{}

func (rejectingDef) Type() string { return "rejected-flow" }
func (rejectingDef) Version() int { return 1 }
func (rejectingDef) Prepare(context.Context, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("amount must be positive")
}
func (rejectingDef) Actions() []Action { return nil }

func registryWith(def Definition) *Registry {
	r := NewRegistry()
	if err := r.Register(def); err != nil {
		panic(err)
	}
	return r
}

// ---------------------------------------------------------------------------
// Test assertion helpers.
// ---------------------------------------------------------------------------

func assertStatus(t *testing.T, inst Instance, want InstanceStatus) {
	t.Helper()
	if inst.Status != want {
		t.Errorf("instance status = %q, want %q", inst.Status, want)
	}
}

func assertActionStatus(t *testing.T, inst Instance, idx int, want ActionStatus) {
	t.Helper()
	if idx >= len(inst.Actions) {
		t.Errorf("action[%d] missing; have %d actions", idx, len(inst.Actions))
		return
	}
	if inst.Actions[idx].Status != want {
		t.Errorf("action[%d].Status = %q, want %q", idx, inst.Actions[idx].Status, want)
	}
}

func assertOutboxCount(t *testing.T, s *memoryStore, want int) {
	t.Helper()
	if got := s.outboxCount(); got != want {
		t.Errorf("outbox count = %d, want %d", got, want)
	}
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestStartPersistsPreparingInstance verifies Engine.Start looks up the
// Definition (rejecting unknown types) and creates a StatusPreparing Instance
// with the input preserved verbatim.
func TestStartPersistsPreparingInstance(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})

	input := json.RawMessage(`{"amount":100}`)
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-start-1",
		Type:       "payment-transfer",
		Version:    1,
		Input:      input,
	})
	if err != nil {
		t.Fatalf("Start: error=%v", err)
	}
	if instance.ID != "wf-start-1" {
		t.Errorf("instance.ID = %q, want wf-start-1", instance.ID)
	}
	if instance.Type != "payment-transfer" || instance.Version != 1 {
		t.Errorf("instance type/version = %q/%d, want payment-transfer/1", instance.Type, instance.Version)
	}

	got := store.instance("wf-start-1")
	assertStatus(t, got, StatusPreparing)
	if string(got.Input) != `{"amount":100}` {
		t.Errorf("instance.Input = %s, want {\"amount\":100}", string(got.Input))
	}
	if got.CurrentAction != 0 || got.Revision != 0 {
		t.Errorf("instance CurrentAction=%d Revision=%d, want 0/0", got.CurrentAction, got.Revision)
	}
}

// TestStartRejectsUnregisteredType verifies Start fails when no Definition is
// registered for the requested (Type, Version) pair.
func TestStartRejectsUnregisteredType(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})

	_, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-unknown",
		Type:       "no-such-flow",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrDefinitionNotFound) {
		t.Fatalf("Start with unknown type: error=%v, want ErrDefinitionNotFound", err)
	}
	if got := store.instance("wf-unknown"); got.ID != "" {
		t.Errorf("instance was persisted despite Start failure: %+v", got)
	}
}

// TestPreparePersistsImmutableContextAndFirstCommand is the brief's prescribed
// happy-path test: after Prepare, the Instance is StatusRunning, the first
// ActionRecord is ActionWaitingResult, and exactly one command sits in the
// outbox.
func TestPreparePersistsImmutableContextAndFirstCommand(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-1", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{"amount":100}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	got := store.instance("wf-1")
	assertStatus(t, got, StatusRunning)
	assertActionStatus(t, got, 0, ActionWaitingResult)
	assertOutboxCount(t, store, 1)

	// Immutable context equals the Prepare output (linearDef echoes input).
	if string(got.PreparedContext) != `{"amount":100}` {
		t.Errorf("PreparedContext = %s, want {\"amount\":100}", string(got.PreparedContext))
	}
	if got.CurrentAction != 0 {
		t.Errorf("CurrentAction = %d, want 0", got.CurrentAction)
	}
	if got.Revision != 1 {
		t.Errorf("Revision = %d, want 1 (incremented once by Prepare)", got.Revision)
	}
	// First action record should carry the dispatch's idempotency key.
	if len(got.Actions) == 0 || got.Actions[0].IdempotencyKey != "book-transfer-idem-key" {
		t.Errorf("action[0].IdempotencyKey = %q, want book-transfer-idem-key",
			safeKey(got.Actions))
	}
}

// TestPrepareRetriesAfterTransientFailure verifies that a transient Store
// failure (here: AppendOutbox) rolls back all writes, leaves the instance in
// StatusPreparing, and a subsequent Prepare succeeds cleanly.
func TestPrepareRetriesAfterTransientFailure(t *testing.T) {
	store := newMemoryStore()
	var appendCalls int
	store.appendOutboxHook = func() error {
		appendCalls++
		if appendCalls == 1 {
			return errors.New("simulated broker connection lost")
		}
		return nil
	}
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-retry", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{"amount":100}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// First Prepare: AppendOutbox fails, Tx rolls back.
	firstErr := engine.Prepare(context.Background(), instance.ID)
	if firstErr == nil {
		t.Fatal("first Prepare: want error from AppendOutbox, got nil")
	}

	// State must be unchanged: still preparing, no actions, empty outbox.
	got := store.instance("wf-retry")
	assertStatus(t, got, StatusPreparing)
	if len(got.Actions) != 0 {
		t.Errorf("after failed Prepare: actions = %d, want 0 (rollback)", len(got.Actions))
	}
	if got.Revision != 0 {
		t.Errorf("after failed Prepare: Revision = %d, want 0 (rollback)", got.Revision)
	}
	assertOutboxCount(t, store, 0)

	// Retry: AppendOutbox succeeds, instance advances.
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatalf("retry Prepare: error=%v", err)
	}
	got = store.instance("wf-retry")
	assertStatus(t, got, StatusRunning)
	assertActionStatus(t, got, 0, ActionWaitingResult)
	assertOutboxCount(t, store, 1)
}

// TestPrepareRejectsAfterBusinessValidation verifies that when the
// Definition's Prepare returns a business error, the Instance transitions to
// StatusRejected, no command is dispatched, and Engine.Prepare returns nil
// (rejection is a workflow outcome, not a system error).
func TestPrepareRejectsAfterBusinessValidation(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(rejectingDefinition()), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-rej", Type: "rejected-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatalf("Prepare returned error for business rejection: %v", err)
	}

	got := store.instance("wf-rej")
	assertStatus(t, got, StatusRejected)
	assertOutboxCount(t, store, 0)
	if got.LastError == "" {
		t.Errorf("LastError is empty, want the business validation message")
	}
	if got.LastErrorClass != BusinessRejected {
		t.Errorf("LastErrorClass = %q, want %q", got.LastErrorClass, BusinessRejected)
	}
	if got.Revision != 1 {
		t.Errorf("Revision = %d, want 1 (rejection increments)", got.Revision)
	}
}

// TestPrepareIsIdempotentOnReinvoke verifies a second Prepare call on an
// already-running instance returns nil without side effects.
func TestPrepareIsIdempotentOnReinvoke(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-idem", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	gotBefore := store.instance("wf-idem")

	// Second Prepare: no-op.
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatalf("second Prepare: error=%v", err)
	}
	gotAfter := store.instance("wf-idem")

	if gotAfter.Revision != gotBefore.Revision {
		t.Errorf("Revision changed on idempotent Prepare: %d → %d", gotBefore.Revision, gotAfter.Revision)
	}
	assertOutboxCount(t, store, 1) // no second command emitted
}

func safeKey(actions []ActionRecord) string {
	if len(actions) == 0 {
		return "<no actions>"
	}
	return actions[0].IdempotencyKey
}

// ---------------------------------------------------------------------------
// Result-advancement test helpers (Task 4).
// ---------------------------------------------------------------------------

// runningTwoStepWorkflow creates a 2-action workflow, starts it, and prepares
// it, leaving it in StatusRunning with action[0] in ActionWaitingResult. The
// returned store already holds exactly one command in the outbox (the first
// action's dispatch).
func runningTwoStepWorkflow(t *testing.T) (*Engine, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-1", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{"amount":100}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	return engine, store
}

// successEvent builds a valid result Envelope addressed to the given action,
// carrying the given command ID so the engine's command-ID validation passes.
func successEvent(workflowID, actionName, commandID string) messaging.Envelope {
	env := messaging.NewEnvelope(
		"result."+actionName,
		workflowID,
		json.RawMessage(`{"ok":true}`),
		time.Now,
	)
	env.WorkflowID = workflowID
	env.ActionName = actionName
	env.CommandID = commandID
	return env
}

// runConcurrently launches n goroutines that all call fn at (approximately)
// the same instant, waits for them to finish, and fails the test if any
// returned a non-nil error.
func runConcurrently(t *testing.T, n int, fn func() error) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = fn()
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: error=%v", i, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Task 4 result-advancement tests.
// ---------------------------------------------------------------------------

// TestApplyResultAdvancesFirstAction verifies that applying a valid success
// event for the first action marks it succeeded, dispatches the second action,
// and leaves the instance in StatusRunning.
func TestApplyResultAdvancesFirstAction(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")
	cmdID := inst.Actions[0].CommandID

	event := successEvent("wf-1", "book-transfer", cmdID)
	if err := engine.ApplyResult(context.Background(), event); err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}

	got := store.instance("wf-1")
	assertActionStatus(t, got, 0, ActionSucceeded)
	assertActionStatus(t, got, 1, ActionWaitingResult)
	assertOutboxCount(t, store, 2) // first command + second command
	assertStatus(t, got, StatusRunning)
	if got.CurrentAction != 1 {
		t.Errorf("CurrentAction = %d, want 1", got.CurrentAction)
	}
	if got.Revision != 2 {
		t.Errorf("Revision = %d, want 2 (Prepare + one advance)", got.Revision)
	}
	// The first action record should carry the result event id and output.
	if len(got.Actions) > 0 && got.Actions[0].ResultEventID != event.MessageID {
		t.Errorf("action[0].ResultEventID = %q, want %q", got.Actions[0].ResultEventID, event.MessageID)
	}
}

// TestDuplicateResultAdvancesOnlyOnce is the brief's prescribed concurrency
// test: two goroutines deliver the SAME result event concurrently; the
// Inbox dedup + serialized WithInstance guarantee exactly one advance.
func TestDuplicateResultAdvancesOnlyOnce(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")
	event := successEvent("wf-1", "book-transfer", inst.Actions[0].CommandID)

	runConcurrently(t, 2, func() error {
		return engine.ApplyResult(context.Background(), event)
	})

	got := store.instance("wf-1")
	assertActionStatus(t, got, 0, ActionSucceeded)
	assertOutboxCount(t, store, 2) // first command plus exactly one second command
	if got.CurrentAction != 1 {
		t.Errorf("CurrentAction = %d, want 1 (advanced exactly once)", got.CurrentAction)
	}
	if got.Revision != 2 {
		t.Errorf("Revision = %d, want 2 (advanced exactly once)", got.Revision)
	}
}

// TestDuplicateResultSequentialIsIdempotent verifies that a second (sequential)
// delivery of the same event is a silent no-op.
func TestDuplicateResultSequentialIsIdempotent(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")
	event := successEvent("wf-1", "book-transfer", inst.Actions[0].CommandID)

	if err := engine.ApplyResult(context.Background(), event); err != nil {
		t.Fatalf("first ApplyResult: %v", err)
	}
	afterFirst := store.instance("wf-1")

	// Second delivery of the same event: idempotent no-op.
	if err := engine.ApplyResult(context.Background(), event); err != nil {
		t.Fatalf("second (duplicate) ApplyResult: %v", err)
	}
	afterSecond := store.instance("wf-1")

	if afterSecond.Revision != afterFirst.Revision {
		t.Errorf("Revision changed on duplicate: %d -> %d", afterFirst.Revision, afterSecond.Revision)
	}
	assertOutboxCount(t, store, 2) // no extra second command
}

// TestApplyResultWrongCommandID verifies that a result event whose CommandID
// does not match the current action's dispatched command is rejected with
// ErrInvalidMessage and leaves the workflow state unchanged.
func TestApplyResultWrongCommandID(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)

	event := successEvent("wf-1", "book-transfer", "bogus-command-id")
	err := engine.ApplyResult(context.Background(), event)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("error = %v, want ErrInvalidMessage", err)
	}

	got := store.instance("wf-1")
	assertStatus(t, got, StatusRunning)
	assertActionStatus(t, got, 0, ActionWaitingResult) // unchanged
	assertOutboxCount(t, store, 1)                     // no second command dispatched
}

// TestApplyResultUnexpectedEventType verifies that a result event whose
// MessageType is not in the current action's AcceptedResultTypes is rejected
// with ErrInvalidMessage.
func TestApplyResultUnexpectedEventType(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")

	env := messaging.NewEnvelope(
		"unexpected.event.type",
		"wf-1",
		json.RawMessage(`{}`),
		time.Now,
	)
	env.WorkflowID = "wf-1"
	env.ActionName = "book-transfer"
	env.CommandID = inst.Actions[0].CommandID

	err := engine.ApplyResult(context.Background(), env)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("error = %v, want ErrInvalidMessage", err)
	}

	got := store.instance("wf-1")
	assertActionStatus(t, got, 0, ActionWaitingResult) // unchanged
	assertOutboxCount(t, store, 1)                     // no second command
}

// TestApplyResultWrongActionName verifies that a result event whose ActionName
// does not match the current action is rejected.
func TestApplyResultWrongActionName(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")

	event := successEvent("wf-1", "settle-transfer", inst.Actions[0].CommandID)
	err := engine.ApplyResult(context.Background(), event)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("error = %v, want ErrInvalidMessage", err)
	}
}

// TestApplyResultWrongWorkflowID verifies that a result event for a different
// workflow is rejected.
func TestApplyResultWrongWorkflowID(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)
	inst := store.instance("wf-1")

	// The envelope's WorkflowID routes to a non-existent instance.
	event := successEvent("wf-missing", "book-transfer", inst.Actions[0].CommandID)
	err := engine.ApplyResult(context.Background(), event)
	if err == nil {
		t.Fatal("ApplyResult for missing workflow: want error, got nil")
	}
}

// TestApplyResultFinalSuccess verifies that applying a success event to the
// LAST action transitions the instance to StatusSucceeded and emits no
// further commands.
func TestApplyResultFinalSuccess(t *testing.T) {
	engine, store := runningTwoStepWorkflow(t)

	// Advance first action.
	inst := store.instance("wf-1")
	event1 := successEvent("wf-1", "book-transfer", inst.Actions[0].CommandID)
	if err := engine.ApplyResult(context.Background(), event1); err != nil {
		t.Fatalf("ApplyResult first: %v", err)
	}

	// Advance second (last) action.
	inst = store.instance("wf-1")
	if len(inst.Actions) < 2 {
		t.Fatalf("expected 2 actions, got %d", len(inst.Actions))
	}
	event2 := successEvent("wf-1", "settle-transfer", inst.Actions[1].CommandID)
	if err := engine.ApplyResult(context.Background(), event2); err != nil {
		t.Fatalf("ApplyResult second: %v", err)
	}

	got := store.instance("wf-1")
	assertStatus(t, got, StatusSucceeded)
	assertActionStatus(t, got, 0, ActionSucceeded)
	assertActionStatus(t, got, 1, ActionSucceeded)
	assertOutboxCount(t, store, 2) // two commands total, no third
	if got.Revision != 3 {
		t.Errorf("Revision = %d, want 3 (Prepare + 2 advances)", got.Revision)
	}
}

// ---------------------------------------------------------------------------
// Task 5 compensation test fixtures.
// ---------------------------------------------------------------------------

// scriptedAction is a fully configurable Action for compensation tests. Each
// hook returns a preconfigured value; compOutcomes is consumed left-to-right on
// each ApplyCompensationResult call (falling back to success once exhausted),
// letting tests script "fail N times then succeed" compensation sequences
// without mutating other fields.
type scriptedAction struct {
	name           string
	forwardDisp    Dispatch
	forwardOutcome Outcome
	compDisp       Dispatch
	compOutcomes   []Outcome
	compCalls      int
}

func (a *scriptedAction) Name() string { return a.name }
func (a *scriptedAction) Execute(context.Context, View) (Dispatch, error) {
	return a.forwardDisp, nil
}
func (a *scriptedAction) ApplyResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	return a.forwardOutcome, nil
}
func (a *scriptedAction) Compensate(context.Context, View) (Dispatch, error) {
	return a.compDisp, nil
}
func (a *scriptedAction) ApplyCompensationResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	if a.compCalls < len(a.compOutcomes) {
		o := a.compOutcomes[a.compCalls]
		a.compCalls++
		return o, nil
	}
	return Outcome{Succeeded: true, Output: json.RawMessage(`{"undone":true}`)}, nil
}

// fwdDispatch builds a forward Dispatch for an action named `name`. The
// result-event type convention is "result.<name>".
func fwdDispatch(name string) Dispatch {
	return Dispatch{
		RoutingKey:          name + ".cmd",
		Payload:             json.RawMessage(`{}`),
		IdempotencyKey:      name + "-fwd-idem",
		AcceptedResultTypes: []string{"result." + name},
		Deadline:            30 * time.Second,
	}
}

// compDispatch builds a compensation Dispatch for an action named `name`. The
// compensation result-event type convention is "compensation.result.<name>".
func compDispatch(name string) Dispatch {
	return Dispatch{
		RoutingKey:          name + ".compensate.cmd",
		Payload:             json.RawMessage(`{}`),
		IdempotencyKey:      name + "-comp-idem",
		AcceptedResultTypes: []string{"compensation.result." + name},
		Deadline:            30 * time.Second,
	}
}

func okOutcome() Outcome {
	return Outcome{Succeeded: true, Output: json.RawMessage(`{"ok":true}`)}
}
func rejectedOutcome(msg string) Outcome {
	return Outcome{Succeeded: false, Class: BusinessRejected, Message: msg}
}
func transientOutcome(msg string) Outcome {
	return Outcome{Succeeded: false, Class: TransientFailure, Message: msg}
}

// compensationEvent builds a valid compensation-result Envelope addressed to
// the given action, carrying the given command ID so the engine's command-ID
// validation passes on the compensation path.
func compensationEvent(workflowID, actionName, commandID string) messaging.Envelope {
	env := messaging.NewEnvelope(
		"compensation.result."+actionName,
		workflowID,
		json.RawMessage(`{"undone":true}`),
		time.Now,
	)
	env.WorkflowID = workflowID
	env.ActionName = actionName
	env.CommandID = commandID
	return env
}

// deliverForward sends a forward success result for the given action. It
// reads the current CommandID from the store so callers need not plumb it.
func deliverForward(t *testing.T, engine *Engine, store *memoryStore, workflowID, actionName string, idx int) {
	t.Helper()
	inst := store.instance(workflowID)
	if idx >= len(inst.Actions) {
		t.Fatalf("deliverForward: action[%d] missing (have %d)", idx, len(inst.Actions))
	}
	env := successEvent(workflowID, actionName, inst.Actions[idx].CommandID)
	if err := engine.ApplyResult(context.Background(), env); err != nil {
		t.Fatalf("ApplyResult forward %q: %v", actionName, err)
	}
}

// deliverForwardFailure sends a forward result envelope for the given action;
// the scripted action's ApplyResult decides the actual Outcome (terminal or
// transient failure). The envelope carries the action's current CommandID.
func deliverForwardFailure(t *testing.T, engine *Engine, store *memoryStore, workflowID, actionName string, idx int) {
	t.Helper()
	inst := store.instance(workflowID)
	if idx >= len(inst.Actions) {
		t.Fatalf("deliverForwardFailure: action[%d] missing (have %d)", idx, len(inst.Actions))
	}
	env := successEvent(workflowID, actionName, inst.Actions[idx].CommandID)
	if err := engine.ApplyResult(context.Background(), env); err != nil {
		t.Fatalf("ApplyResult forward-failure %q: %v", actionName, err)
	}
}

// deliverCompensation sends a compensation-result envelope for the given
// action, reading the current (compensation) CommandID from the store.
func deliverCompensation(t *testing.T, engine *Engine, store *memoryStore, workflowID, actionName string, idx int) {
	t.Helper()
	inst := store.instance(workflowID)
	if idx >= len(inst.Actions) {
		t.Fatalf("deliverCompensation: action[%d] missing (have %d)", idx, len(inst.Actions))
	}
	env := compensationEvent(workflowID, actionName, inst.Actions[idx].CommandID)
	if err := engine.ApplyResult(context.Background(), env); err != nil {
		t.Fatalf("ApplyResult compensation %q: %v", actionName, err)
	}
}

// compensationDispatchOrder scans the outbox and returns the ActionNames of
// compensation command envelopes, in dispatch (append) order. Non-compensation
// envelopes are skipped.
func compensationDispatchOrder(t *testing.T, store *memoryStore) []string {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	var order []string
	for _, e := range store.outbox {
		if strings.HasPrefix(e.env.MessageType, "compensation.") {
			order = append(order, e.env.ActionName)
		}
	}
	return order
}

// ---------------------------------------------------------------------------
// Task 5 compensation tests.
// ---------------------------------------------------------------------------

// TestCompensateReverseOrder is the brief's prescribed reverse-order test: a
// 3-step workflow where step 3 returns a terminal failure must compensate
// step 2 THEN step 1 (reverse dispatch order).
func TestCompensateReverseOrder(t *testing.T) {
	store := newMemoryStore()
	def := linearDef{
		workflowType: "compensatable-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "step-1", forwardDisp: fwdDispatch("step-1"), forwardOutcome: okOutcome(), compDisp: compDispatch("step-1")},
			&scriptedAction{name: "step-2", forwardDisp: fwdDispatch("step-2"), forwardOutcome: okOutcome(), compDisp: compDispatch("step-2")},
			&scriptedAction{name: "step-3", forwardDisp: fwdDispatch("step-3"), forwardOutcome: rejectedOutcome("insufficient funds")},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-comp", Type: "compensatable-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Advance steps 1 and 2 with forward successes.
	deliverForward(t, engine, store, "wf-comp", "step-1", 0)
	deliverForward(t, engine, store, "wf-comp", "step-2", 1)

	// Step 3 returns a terminal failure → compensation must begin.
	deliverForwardFailure(t, engine, store, "wf-comp", "step-3", 2)

	got := store.instance("wf-comp")
	assertStatus(t, got, StatusCompensating)
	assertActionStatus(t, got, 2, ActionFailed)       // step-3 is failed (terminal)
	assertActionStatus(t, got, 1, ActionCompensating) // step-2 compensation dispatched first

	// Deliver step-2 compensation success → step-1 compensation dispatched.
	deliverCompensation(t, engine, store, "wf-comp", "step-2", 1)

	got = store.instance("wf-comp")
	assertActionStatus(t, got, 1, ActionCompensated)
	assertActionStatus(t, got, 0, ActionCompensating)

	// Deliver step-1 compensation success → instance fully compensated.
	deliverCompensation(t, engine, store, "wf-comp", "step-1", 0)

	got = store.instance("wf-comp")
	assertStatus(t, got, StatusCompensated)
	assertActionStatus(t, got, 0, ActionCompensated)
	assertActionStatus(t, got, 1, ActionCompensated)

	// Compensation dispatch order MUST be step-2 then step-1 (reverse).
	order := compensationDispatchOrder(t, store)
	if want := []string{"step-2", "step-1"}; !reflect.DeepEqual(order, want) {
		t.Errorf("compensation dispatch order = %v, want %v", order, want)
	}
}

// TestRejectedInstanceNotCompensated verifies that an instance rejected by
// Prepare (business validation failure) is terminal and never enters
// compensation, even if a stray result event arrives.
func TestRejectedInstanceNotCompensated(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(rejectingDefinition()), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-rej", Type: "rejected-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatalf("Prepare returned error for business rejection: %v", err)
	}

	got := store.instance("wf-rej")
	assertStatus(t, got, StatusRejected)
	assertOutboxCount(t, store, 0)

	// A stray result event must be rejected, NOT trigger compensation.
	env := successEvent("wf-rej", "anything", "anything")
	err = engine.ApplyResult(context.Background(), env)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("ApplyResult on rejected instance: error=%v, want ErrInvalidMessage", err)
	}

	// Instance stays rejected; no compensation, no commands.
	got = store.instance("wf-rej")
	assertStatus(t, got, StatusRejected)
	assertOutboxCount(t, store, 0)
}

// TestCompensationTransientRetriesSameKey verifies that a transient
// compensation failure retries with the SAME semantic idempotency key (only
// the transport CommandID and the attempt counter change).
func TestCompensationTransientRetriesSameKey(t *testing.T) {
	store := newMemoryStore()
	step1 := &scriptedAction{
		name:           "step-1",
		forwardDisp:    fwdDispatch("step-1"),
		forwardOutcome: okOutcome(),
		compDisp:       compDispatch("step-1"),
		// First compensation result: transient failure. Second: success.
		compOutcomes: []Outcome{transientOutcome("compensation broker timeout")},
	}
	step2 := &scriptedAction{
		name:           "step-2",
		forwardDisp:    fwdDispatch("step-2"),
		forwardOutcome: rejectedOutcome("bad request"),
	}
	def := linearDef{
		workflowType: "retry-comp-flow",
		version:      1,
		actions:      []Action{step1, step2},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-rc", Type: "retry-comp-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Advance step-1; step-2 fails terminally → step-1 compensation dispatched.
	deliverForward(t, engine, store, "wf-rc", "step-1", 0)
	deliverForwardFailure(t, engine, store, "wf-rc", "step-2", 1)

	got := store.instance("wf-rc")
	assertStatus(t, got, StatusCompensating)
	firstKey := got.Actions[0].IdempotencyKey
	firstCmd := got.Actions[0].CommandID
	if got.Actions[0].Attempt != 1 {
		t.Fatalf("initial compensation Attempt = %d, want 1", got.Actions[0].Attempt)
	}

	// Deliver compensation result → transient failure → re-dispatched.
	deliverCompensation(t, engine, store, "wf-rc", "step-1", 0)

	got = store.instance("wf-rc")
	assertActionStatus(t, got, 0, ActionCompensating) // still compensating (retried)
	if got.Actions[0].IdempotencyKey != firstKey {
		t.Errorf("idempotency key changed on retry: %q → %q", firstKey, got.Actions[0].IdempotencyKey)
	}
	if got.Actions[0].CommandID == firstCmd {
		t.Errorf("CommandID did not change on retry (must be fresh transport id)")
	}
	if got.Actions[0].Attempt != 2 {
		t.Errorf("compensation Attempt = %d, want 2 after one retry", got.Actions[0].Attempt)
	}

	// Second compensation result → success → compensated.
	deliverCompensation(t, engine, store, "wf-rc", "step-1", 0)

	got = store.instance("wf-rc")
	assertActionStatus(t, got, 0, ActionCompensated)
	assertStatus(t, got, StatusCompensated)
}

// TestCompensationExhaustsAttemptsToFailed verifies that after
// CompensationMaxAttempts (default 5) transient compensation failures, the
// action and instance transition to compensation_failed while preserving
// CurrentAction.
func TestCompensationExhaustsAttemptsToFailed(t *testing.T) {
	store := newMemoryStore()
	step1 := &scriptedAction{
		name:           "step-1",
		forwardDisp:    fwdDispatch("step-1"),
		forwardOutcome: okOutcome(),
		compDisp:       compDispatch("step-1"),
		// Every compensation result is a transient failure.
		compOutcomes: []Outcome{
			transientOutcome("fail-1"),
			transientOutcome("fail-2"),
			transientOutcome("fail-3"),
			transientOutcome("fail-4"),
			transientOutcome("fail-5"),
			transientOutcome("fail-6"),
		},
	}
	step2 := &scriptedAction{
		name:           "step-2",
		forwardDisp:    fwdDispatch("step-2"),
		forwardOutcome: rejectedOutcome("nope"),
	}
	def := linearDef{
		workflowType: "exhaust-flow",
		version:      1,
		actions:      []Action{step1, step2},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-ex", Type: "exhaust-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Advance step-1; step-2 fails terminally → step-1 compensation begins.
	deliverForward(t, engine, store, "wf-ex", "step-1", 0)
	deliverForwardFailure(t, engine, store, "wf-ex", "step-2", 1)

	// Drive CompensationMaxAttempts (5) compensation results, all transient.
	const maxAttempts = 5
	for i := 1; i <= maxAttempts; i++ {
		got := store.instance("wf-ex")
		if got.Actions[0].Attempt != i {
			t.Fatalf("before comp result %d: Attempt = %d, want %d", i, got.Actions[0].Attempt, i)
		}
		deliverCompensation(t, engine, store, "wf-ex", "step-1", 0)

		got = store.instance("wf-ex")
		if i < maxAttempts {
			assertActionStatus(t, got, 0, ActionCompensating) // retried
			if got.Actions[0].Attempt != i+1 {
				t.Errorf("after comp result %d: Attempt = %d, want %d", i, got.Actions[0].Attempt, i+1)
			}
		}
	}

	// After the 5th transient failure: compensation_failed.
	got := store.instance("wf-ex")
	assertActionStatus(t, got, 0, ActionCompensationFailed)
	assertStatus(t, got, StatusCompensationFailed)
	if got.CurrentAction != 0 {
		t.Errorf("CurrentAction = %d, want 0 (preserved on compensation_failed)", got.CurrentAction)
	}
}

// TestTransientExecutionFailureLeavesRunning locks the Task 4/5 boundary: a
// transient forward failure marks the action failed but does NOT trigger
// compensation (instance stays running for Task 6 recovery).
func TestTransientExecutionFailureLeavesRunning(t *testing.T) {
	store := newMemoryStore()
	def := linearDef{
		workflowType: "transient-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "step-1", forwardDisp: fwdDispatch("step-1"), forwardOutcome: transientOutcome("downstream unavailable")},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-t", Type: "transient-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Step-1 returns a transient failure.
	deliverForwardFailure(t, engine, store, "wf-t", "step-1", 0)

	got := store.instance("wf-t")
	assertStatus(t, got, StatusRunning)         // NOT compensating
	assertActionStatus(t, got, 0, ActionFailed) // failed, not compensating
	assertOutboxCount(t, store, 1)              // only the forward command, no compensation dispatched
}

// TestCompensateSingleActionFailureWithNothingToUndo verifies a 1-step
// workflow whose sole action fails terminally goes straight to compensated
// (there are no prior succeeded actions to undo).
func TestCompensateSingleActionFailureWithNothingToUndo(t *testing.T) {
	store := newMemoryStore()
	def := linearDef{
		workflowType: "solo-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "step-1", forwardDisp: fwdDispatch("step-1"), forwardOutcome: rejectedOutcome("validation error")},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-solo", Type: "solo-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Step-1 fails terminally → nothing to compensate → compensated directly.
	deliverForwardFailure(t, engine, store, "wf-solo", "step-1", 0)

	got := store.instance("wf-solo")
	assertStatus(t, got, StatusCompensated)
	assertActionStatus(t, got, 0, ActionFailed)
	assertOutboxCount(t, store, 1) // forward command only, no compensation dispatched
}
