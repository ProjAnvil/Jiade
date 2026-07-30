package workflow

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
		RoutingKey:     a.routingKey,
		Payload:        json.RawMessage(`{}`),
		IdempotencyKey: a.name + "-idem-key",
		Deadline:       30 * time.Second,
	}, nil
}
func (a linearAction) ApplyResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	return Outcome{}, nil
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
