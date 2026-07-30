package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// ---------------------------------------------------------------------------
// Shared test helpers for the payment-transfer workflow action tests.
// ---------------------------------------------------------------------------

// jsonMust marshals v or fails the test. Used for building View payloads.
func jsonMust(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("jsonMust: marshal %T: %v", v, err)
	}
	return b
}

// sameStrings reports whether two string slices hold the same set of values
// (order-independent). Used for AcceptedResultTypes assertions.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

// buildView constructs a workflow.View for testing an action. The Instance
// carries a valid PreparedContext; priorActions are placed on the Instance so
// downstream actions can read earlier outputs (e.g. PostLedgerTransfer reads
// the hold_id from the PlaceFundsHold action record).
func buildView(wid string, actionIdx int, actionName string, priorActions []workflow.ActionRecord) workflow.View {
	tc := TransferContext{
		PaymentID:       "PAY-1",
		PayerCustomerID: "C-100",
		PayerAccountNo:  "ACC-PAYER",
		PayeeAccountNo:  "ACC-PAYEE",
		Currency:        "CNY",
		AmountMinor:     50000,
	}
	ctxBytes, _ := json.Marshal(tc)
	return workflow.View{
		Instance: workflow.Instance{
			ID:              wid,
			Type:            "payment-transfer",
			Version:         1,
			PreparedContext: ctxBytes,
			Actions:         priorActions,
		},
		Action: workflow.ActionRecord{Index: actionIdx, Name: actionName},
	}
}

// compensateView builds a View whose Action record carries the forward Output
// produced by a successful forward pass — the input Compensate reads to build
// the undo command.
func compensateView(wid string, idx int, name string, output json.RawMessage) workflow.View {
	v := buildView(wid, idx, name, nil)
	v.Action.Output = output
	v.Action.Status = workflow.ActionSucceeded
	return v
}

// ---------------------------------------------------------------------------
// In-memory Store for engine-through testing. Mirrors the engine package's
// internal memoryStore but lives here so the workflows package can drive the
// REAL engine without depending on unexported test helpers.
// ---------------------------------------------------------------------------

type memOutboxEntry struct {
	env        messaging.Envelope
	routingKey string
}

type memStore struct {
	mu        sync.Mutex
	instances map[string]*workflow.Instance
	outbox    []memOutboxEntry
	inbox     map[string]map[string]struct{}
}

func newMemStore() *memStore {
	return &memStore{
		instances: make(map[string]*workflow.Instance),
		inbox:     make(map[string]map[string]struct{}),
	}
}

func (s *memStore) Create(_ context.Context, req workflow.StartRequest) (workflow.Instance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.instances[req.WorkflowID]; exists {
		return workflow.Instance{}, workflow.ErrInstanceExists
	}
	inst := workflow.Instance{
		ID:            req.WorkflowID,
		Type:          req.Type,
		Version:       req.Version,
		Status:        workflow.StatusPreparing,
		Input:         append(json.RawMessage(nil), req.Input...),
		CorrelationID: req.CorrelationID,
	}
	s.instances[req.WorkflowID] = &inst
	return inst, nil
}

func (s *memStore) WithInstance(_ context.Context, id string, fn func(workflow.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return workflow.ErrInstanceNotFound
	}
	tx := &memTx{store: s, working: *inst}
	if err := fn(tx); err != nil {
		return err
	}
	*inst = tx.working
	s.outbox = append(s.outbox, tx.bufferedOutbox...)
	for _, ie := range tx.bufferedInbox {
		set, ok := s.inbox[ie.consumer]
		if !ok {
			set = make(map[string]struct{})
			s.inbox[ie.consumer] = set
		}
		set[ie.messageID] = struct{}{}
	}
	return nil
}

// lastOutbox returns the most recent outbox entry (the last dispatched command).
func (s *memStore) lastOutbox() memOutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outbox) == 0 {
		return memOutboxEntry{}
	}
	return s.outbox[len(s.outbox)-1]
}

func (s *memStore) outboxAt(i int) memOutboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.outbox) {
		return memOutboxEntry{}
	}
	return s.outbox[i]
}

// Unused Store methods — the forward+compensation test flow does not exercise
// the recovery/lease paths, so they return zero values.
func (s *memStore) ClaimRunnable(context.Context, string, time.Time, time.Duration, int) ([]string, error) {
	return nil, nil
}
func (s *memStore) RenewLease(context.Context, string, string, time.Time, time.Duration) error {
	return nil
}
func (s *memStore) ReleaseLease(context.Context, string, string) error { return nil }
func (s *memStore) TimedOut(context.Context, time.Time, int) ([]string, error) {
	return nil, nil
}
func (s *memStore) NonTerminalDefinitions(context.Context) ([]workflow.DefinitionRef, error) {
	return nil, nil
}

type memInboxEntry struct {
	consumer  string
	messageID string
}

type memTx struct {
	store          *memStore
	working        workflow.Instance
	bufferedOutbox []memOutboxEntry
	bufferedInbox  []memInboxEntry
}

func (t *memTx) Instance() *workflow.Instance { return &t.working }

func (t *memTx) InsertInbox(consumer string, env messaging.Envelope) (bool, error) {
	if set, ok := t.store.inbox[consumer]; ok {
		if _, dup := set[env.MessageID]; dup {
			return false, nil
		}
	}
	for _, ie := range t.bufferedInbox {
		if ie.consumer == consumer && ie.messageID == env.MessageID {
			return false, nil
		}
	}
	t.bufferedInbox = append(t.bufferedInbox, memInboxEntry{consumer, env.MessageID})
	return true, nil
}

func (t *memTx) SaveInstance(inst workflow.Instance) error {
	t.working = inst
	return nil
}

func (t *memTx) SaveAction(rec workflow.ActionRecord) error {
	actions := t.working.Actions
	for len(actions) <= rec.Index {
		actions = append(actions, workflow.ActionRecord{Index: len(actions)})
	}
	actions[rec.Index] = rec
	t.working.Actions = actions
	return nil
}

func (t *memTx) AppendOutbox(env messaging.Envelope, routingKey string) error {
	t.bufferedOutbox = append(t.bufferedOutbox, memOutboxEntry{env: env, routingKey: routingKey})
	return nil
}

// resultFromCommand builds a result envelope that mirrors what a real consumer
// emits: it carries the same WorkflowID/ActionName/CommandID/IdempotencyKey as
// the originating command so the engine's result validation passes.
func resultFromCommand(t *testing.T, cmd messaging.Envelope, messageType string, payload json.RawMessage) messaging.Envelope {
	t.Helper()
	env := messaging.NewEnvelope(messageType, cmd.CorrelationID, payload, time.Now)
	env.WorkflowID = cmd.WorkflowID
	env.ActionName = cmd.ActionName
	env.CommandID = cmd.CommandID
	env.IdempotencyKey = cmd.IdempotencyKey
	env.CausationID = cmd.MessageID
	return env
}

// ---------------------------------------------------------------------------
// Definition tests.
// ---------------------------------------------------------------------------

// TestPaymentTransferDefinition_Metadata asserts the definition registers under
// the expected type+version and exposes the three ordered actions.
func TestPaymentTransferDefinition_Metadata(t *testing.T) {
	def := NewPaymentTransferDefinition(NewPreparation(validCustomerReader(), validAccountReader()))

	if def.Type() != "payment-transfer" {
		t.Errorf("Type = %q, want %q", def.Type(), "payment-transfer")
	}
	if def.Version() != 1 {
		t.Errorf("Version = %d, want 1", def.Version())
	}

	actions := def.Actions()
	if len(actions) != 3 {
		t.Fatalf("Actions: got %d, want 3", len(actions))
	}
	wantNames := []string{"AuthorizeRisk", "PlaceFundsHold", "PostLedgerTransfer"}
	for i, want := range wantNames {
		if got := actions[i].Name(); got != want {
			t.Errorf("Actions[%d].Name = %q, want %q", i, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Compensation-order test through the REAL engine (brief Step 4).
//
// Simulates a terminal failure at the PostLedgerTransfer action (after the hold
// was placed). Asserts the engine emits compensation commands in REVERSE order:
// first release the hold (undo PlaceFundsHold), then void the risk
// authorization (undo AuthorizeRisk). Financial steps are NOT skipped.
// ---------------------------------------------------------------------------

func TestCompensationOrder_FailureAfterHold(t *testing.T) {
	store := newMemStore()
	registry := workflow.NewRegistry()
	def := NewPaymentTransferDefinition(NewPreparation(validCustomerReader(), validAccountReader()))
	if err := registry.Register(def); err != nil {
		t.Fatalf("register definition: %v", err)
	}
	engine := workflow.NewEngine(store, registry, workflow.EngineConfig{})

	const wid = "wf-comp-1"
	ctx := context.Background()

	// Start + Prepare dispatches the AuthorizeRisk command.
	input := validInput()
	inputBytes, _ := json.Marshal(input)
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID:    wid,
		Type:          "payment-transfer",
		Version:       1,
		Input:         inputBytes,
		CorrelationID: wid,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := engine.Prepare(ctx, wid); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// 1. AuthorizeRisk → authorized → advances to PlaceFundsHold.
	authCmd := store.lastOutbox()
	if authCmd.routingKey != "risk.authorize-payment.v1" {
		t.Fatalf("action 0 routing = %q, want risk.authorize-payment.v1", authCmd.routingKey)
	}
	authorizedEnv := resultFromCommand(t, authCmd.env, "risk.payment-authorized.v1",
		jsonMust(t, map[string]any{"authorization_id": "authz:" + wid, "customer_id": "C-100"}))
	if err := engine.ApplyResult(ctx, authorizedEnv); err != nil {
		t.Fatalf("ApplyResult authorized: %v", err)
	}

	// 2. PlaceFundsHold → held → advances to PostLedgerTransfer.
	holdCmd := store.lastOutbox()
	if holdCmd.routingKey != "core.place-hold.v1" {
		t.Fatalf("action 1 routing = %q, want core.place-hold.v1", holdCmd.routingKey)
	}
	heldEnv := resultFromCommand(t, holdCmd.env, "core.hold-placed.v1",
		jsonMust(t, map[string]any{"hold_id": "H-1", "account_no": "ACC-PAYER"}))
	if err := engine.ApplyResult(ctx, heldEnv); err != nil {
		t.Fatalf("ApplyResult held: %v", err)
	}

	// 3. PostLedgerTransfer → transfer-failed (business_rejected) → compensation.
	transferCmd := store.lastOutbox()
	if transferCmd.routingKey != "core.post-held-transfer.v1" {
		t.Fatalf("action 2 routing = %q, want core.post-held-transfer.v1", transferCmd.routingKey)
	}
	failedEnv := resultFromCommand(t, transferCmd.env, "core.transfer-failed.v1",
		jsonMust(t, failurePayload{
			ErrorClass:   string(workflow.BusinessRejected),
			ErrorMessage: "ledger invariant: insufficient available balance",
			WorkflowID:   wid,
		}))
	if err := engine.ApplyResult(ctx, failedEnv); err != nil {
		t.Fatalf("ApplyResult transfer-failed: %v", err)
	}

	// CRITICAL ASSERTION: the FIRST compensation command releases the hold
	// (undoes PlaceFundsHold, the most-recent succeeded action). Financial
	// undo steps are not skipped.
	releaseCmd := store.lastOutbox()
	if releaseCmd.routingKey != "core.release-hold.v1" {
		t.Errorf("first compensation routing = %q, want core.release-hold.v1", releaseCmd.routingKey)
	}
	if releaseCmd.env.ActionName != "PlaceFundsHold" {
		t.Errorf("first compensation action = %q, want PlaceFundsHold", releaseCmd.env.ActionName)
	}
	// The compensation command must carry the stable compensation idempotency key.
	wantKey := fmt.Sprintf("wf:%s:compensate:place-funds-hold", wid)
	if releaseCmd.env.IdempotencyKey != wantKey {
		t.Errorf("first compensation idempotency = %q, want %q", releaseCmd.env.IdempotencyKey, wantKey)
	}

	// 4. hold-released → compensation advances to AuthorizeRisk void.
	releasedEnv := resultFromCommand(t, releaseCmd.env, "core.hold-released.v1",
		jsonMust(t, map[string]any{"hold_id": "H-1", "account_no": "ACC-PAYER"}))
	if err := engine.ApplyResult(ctx, releasedEnv); err != nil {
		t.Fatalf("ApplyResult hold-released: %v", err)
	}

	// CRITICAL ASSERTION: the SECOND compensation command voids the risk
	// authorization (undoes AuthorizeRisk, the next succeeded action in
	// reverse order).
	voidCmd := store.lastOutbox()
	if voidCmd.routingKey != "risk.void-payment-authorization.v1" {
		t.Errorf("second compensation routing = %q, want risk.void-payment-authorization.v1", voidCmd.routingKey)
	}
	if voidCmd.env.ActionName != "AuthorizeRisk" {
		t.Errorf("second compensation action = %q, want AuthorizeRisk", voidCmd.env.ActionName)
	}
	wantVoidKey := fmt.Sprintf("wf:%s:compensate:authorize-risk", wid)
	if voidCmd.env.IdempotencyKey != wantVoidKey {
		t.Errorf("second compensation idempotency = %q, want %q", voidCmd.env.IdempotencyKey, wantVoidKey)
	}

	// 5. authorization-voided → compensation complete → StatusCompensated.
	voidedEnv := resultFromCommand(t, voidCmd.env, "risk.payment-authorization-voided.v1",
		jsonMust(t, map[string]any{"authorization_id": "authz:" + wid}))
	if err := engine.ApplyResult(ctx, voidedEnv); err != nil {
		t.Fatalf("ApplyResult voided: %v", err)
	}

	store.mu.Lock()
	inst := *store.instances[wid]
	store.mu.Unlock()
	if inst.Status != workflow.StatusCompensated {
		t.Errorf("final status = %q, want %q", inst.Status, workflow.StatusCompensated)
	}
}
