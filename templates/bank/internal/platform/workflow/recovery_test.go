package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"bank/internal/platform/messaging"
)

// ---------------------------------------------------------------------------
// In-memory Store recovery method implementations (Task 6).
//
// These extend the memoryStore defined in engine_test.go with the five
// recovery methods added to the Store interface. Each method mirrors the
// semantics the production PostgresStore will implement in Task 7: claim
// atomically sets the lease; timed-out / non-terminal queries are read-only.
// ---------------------------------------------------------------------------

// isTerminal reports whether an InstanceStatus is terminal — no further
// processing is needed. Used by ClaimRunnable and NonTerminalDefinitions to
// skip instances the recovery loop cannot advance.
func isTerminal(status InstanceStatus) bool {
	switch status {
	case StatusSucceeded, StatusRejected, StatusCompensated, StatusCompensationFailed:
		return true
	default:
		return false
	}
}

// isRunnable reports whether a non-terminal instance has work for the recovery
// loop right now: preparing (needs Prepare), running with a timed-out waiting
// action (needs re-dispatch), or compensating with a timed-out compensating
// action. This encodes the ClaimRunnable "runnable" contract for the in-memory
// store; the PostgresStore expresses the same logic in SQL.
//
// A transiently-failed forward action (StatusRunning + ActionFailed) is
// intentionally NOT runnable: processInstance only knows how to Prepare and
// Redispatch waiting actions, neither of which retries a failed action, so
// claiming such an instance would release it on every poll tick forever
// (busyspin). Forward-retry of transiently-failed actions is a known limitation
// that is NOT implemented in this engine plan; such instances require operator
// intervention or a future retry path.
func isRunnable(inst *Instance, now time.Time) bool {
	switch inst.Status {
	case StatusPreparing:
		return true
	case StatusRunning:
		if inst.CurrentAction < 0 || inst.CurrentAction >= len(inst.Actions) {
			return false
		}
		action := inst.Actions[inst.CurrentAction]
		switch action.Status {
		case ActionWaitingResult:
			return !action.DeadlineAt.IsZero() && now.After(action.DeadlineAt)
			// ActionFailed is deliberately excluded: see the function comment.
		}
	case StatusCompensating:
		if inst.CurrentAction < 0 || inst.CurrentAction >= len(inst.Actions) {
			return false
		}
		action := inst.Actions[inst.CurrentAction]
		if action.Status == ActionCompensating {
			return !action.DeadlineAt.IsZero() && now.After(action.DeadlineAt)
		}
	}
	return false
}

// ClaimRunnable atomically claims up to limit instances that (a) are runnable,
// (b) are non-terminal, and (c) have no active lease held by another owner.
// On each claimed instance it sets LeaseOwner=owner, LeaseUntil=now+lease,
// and bumps Revision.
func (s *memoryStore) ClaimRunnable(_ context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, nil
	}
	var ids []string
	for id, inst := range s.instances {
		if len(ids) >= limit {
			break
		}
		if isTerminal(inst.Status) {
			continue
		}
		// A lease that has not yet expired blocks claiming — even by the same
		// owner (use RenewLease to extend an active lease).
		if inst.LeaseOwner != "" && now.Before(inst.LeaseUntil) {
			continue
		}
		if !isRunnable(inst, now) {
			continue
		}
		inst.LeaseOwner = owner
		inst.LeaseUntil = now.Add(lease)
		inst.Revision++
		ids = append(ids, id)
	}
	return ids, nil
}

// RenewLease extends the lease on instance id for owner until now+lease. It
// fails with ErrLeaseNotHeld if the caller does not currently own the lease.
func (s *memoryStore) RenewLease(_ context.Context, id, owner string, now time.Time, lease time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return ErrInstanceNotFound
	}
	if inst.LeaseOwner != owner {
		return ErrLeaseNotHeld
	}
	inst.LeaseUntil = now.Add(lease)
	inst.Revision++
	return nil
}

// ReleaseLease clears the lease on instance id if currently held by owner. It
// is a no-op if the instance is missing or leased by a different owner (the
// lease may have expired and been re-claimed between claim and release).
func (s *memoryStore) ReleaseLease(_ context.Context, id, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	if !ok {
		return ErrInstanceNotFound
	}
	if inst.LeaseOwner != owner {
		return nil
	}
	inst.LeaseOwner = ""
	inst.LeaseUntil = time.Time{}
	inst.Revision++
	return nil
}

// TimedOut returns up to limit IDs of instances whose current action is
// waiting for a result (forward or compensation) and whose DeadlineAt has
// passed. Read-only — does not claim or modify instances.
func (s *memoryStore) TimedOut(_ context.Context, now time.Time, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		return nil, nil
	}
	var ids []string
	for id, inst := range s.instances {
		if len(ids) >= limit {
			break
		}
		if inst.Status != StatusRunning && inst.Status != StatusCompensating {
			continue
		}
		if inst.CurrentAction < 0 || inst.CurrentAction >= len(inst.Actions) {
			continue
		}
		action := inst.Actions[inst.CurrentAction]
		if action.DeadlineAt.IsZero() || !now.After(action.DeadlineAt) {
			continue
		}
		if action.Status != ActionWaitingResult && action.Status != ActionCompensating {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// NonTerminalDefinitions returns the distinct (Type, Version) pairs of all
// non-terminal instances. Used by AuditDefinitions at startup.
func (s *memoryStore) NonTerminalDefinitions(_ context.Context) ([]DefinitionRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[DefinitionRef]struct{})
	var refs []DefinitionRef
	for _, inst := range s.instances {
		if isTerminal(inst.Status) {
			continue
		}
		ref := DefinitionRef{Type: inst.Type, Version: inst.Version}
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs, nil
}

// outboxEnvelope returns a copy of the envelope at outbox index idx, for test
// assertions. Panics on out-of-range like a test helper.
func (s *memoryStore) outboxEnvelope(idx int) messaging.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outbox[idx].env
}

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

// setLease sets LeaseOwner/LeaseUntil on the instance for lease-test setup.
func setLease(t *testing.T, store *memoryStore, id, owner string, until time.Time) {
	t.Helper()
	err := store.WithInstance(context.Background(), id, func(tx Tx) error {
		inst := *tx.Instance()
		inst.LeaseOwner = owner
		inst.LeaseUntil = until
		inst.Revision++
		return tx.SaveInstance(inst)
	})
	if err != nil {
		t.Fatalf("setLease %q: %v", id, err)
	}
}

// ---------------------------------------------------------------------------
// Task 6 recovery tests.
// ---------------------------------------------------------------------------

// TestLeaseCannotBeStolenBeforeExpiry verifies that ClaimRunnable will NOT
// claim an instance whose lease is still held by another owner (before the
// lease deadline).
func TestLeaseCannotBeStolenBeforeExpiry(t *testing.T) {
	store := newMemoryStore()
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.Create(context.Background(), StartRequest{
		WorkflowID: "wf-lease",
		Type:       "payment-transfer",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Grant a lease to owner-A until baseTime + 10s.
	setLease(t, store, "wf-lease", "owner-A", baseTime.Add(10*time.Second))

	// owner-B tries to claim at baseTime (lease still has 10s to run).
	ids, err := store.ClaimRunnable(context.Background(), "owner-B", baseTime, 5*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimRunnable: %v", err)
	}
	for _, id := range ids {
		if id == "wf-lease" {
			t.Errorf("instance was claimed before lease expiry; ids=%v", ids)
		}
	}

	inst := store.instance("wf-lease")
	if inst.LeaseOwner != "owner-A" {
		t.Errorf("LeaseOwner = %q, want owner-A (lease not stolen)", inst.LeaseOwner)
	}
}

// TestLeaseCanBeClaimedAfterExpiry verifies that ClaimRunnable CAN claim an
// instance once the held lease has expired.
func TestLeaseCanBeClaimedAfterExpiry(t *testing.T) {
	store := newMemoryStore()
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	if _, err := store.Create(context.Background(), StartRequest{
		WorkflowID: "wf-lease",
		Type:       "payment-transfer",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Lease to owner-A until baseTime + 5s.
	setLease(t, store, "wf-lease", "owner-A", baseTime.Add(5*time.Second))

	// owner-B claims at baseTime + 6s — past the lease expiry.
	ids, err := store.ClaimRunnable(context.Background(), "owner-B", baseTime.Add(6*time.Second), 5*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimRunnable: %v", err)
	}

	found := false
	for _, id := range ids {
		if id == "wf-lease" {
			found = true
		}
	}
	if !found {
		t.Fatalf("instance NOT claimed after expiry; ids=%v", ids)
	}

	inst := store.instance("wf-lease")
	if inst.LeaseOwner != "owner-B" {
		t.Errorf("LeaseOwner = %q, want owner-B (claimed after expiry)", inst.LeaseOwner)
	}
	untilWant := baseTime.Add(6 * time.Second).Add(5 * time.Second)
	if !inst.LeaseUntil.Equal(untilWant) {
		t.Errorf("LeaseUntil = %v, want %v (now+lease)", inst.LeaseUntil, untilWant)
	}
}

// TestTimeoutRedispatchKeepsCommandIDAndIdempotencyKey verifies that a waiting
// action whose DeadlineAt has passed gets re-dispatched with a NEW MessageID
// but the SAME CommandID and IdempotencyKey — so the recipient deduplicates
// the command while the transport envelope is fresh.
func TestTimeoutRedispatchKeepsCommandIDAndIdempotencyKey(t *testing.T) {
	store := newMemoryStore()

	// Injected clock — advanced past the action deadline to trigger timeout.
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := baseTime
	nowFn := func() time.Time { return clock }

	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{Now: nowFn})

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-timeout",
		Type:       "payment-transfer",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Capture the original dispatch envelope.
	if store.outboxCount() != 1 {
		t.Fatalf("outbox count after Prepare = %d, want 1", store.outboxCount())
	}
	original := store.outboxEnvelope(0)

	// Advance clock past the action's deadline (linearAction uses 30s).
	clock = baseTime.Add(31 * time.Second)

	// Confirm the store's TimedOut query surfaces the instance.
	timedOut, err := store.TimedOut(context.Background(), clock, 10)
	if err != nil {
		t.Fatalf("TimedOut: %v", err)
	}
	if len(timedOut) != 1 || timedOut[0] != "wf-timeout" {
		t.Fatalf("TimedOut = %v, want [wf-timeout]", timedOut)
	}

	// Re-dispatch the timed-out action.
	if err := engine.Redispatch(context.Background(), instance.ID); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}

	if store.outboxCount() != 2 {
		t.Fatalf("outbox count after Redispatch = %d, want 2", store.outboxCount())
	}
	redispatched := store.outboxEnvelope(1)

	// NEW MessageID — the envelope is a fresh transport message.
	if redispatched.MessageID == original.MessageID {
		t.Errorf("MessageID unchanged: %q (want a fresh UUID)", redispatched.MessageID)
	}
	// SAME CommandID — the recipient deduplicates against this ID.
	if redispatched.CommandID != original.CommandID {
		t.Errorf("CommandID changed: %q -> %q (must stay stable)", original.CommandID, redispatched.CommandID)
	}
	// SAME IdempotencyKey — semantic identity for downstream dedup.
	if redispatched.IdempotencyKey != original.IdempotencyKey {
		t.Errorf("IdempotencyKey changed: %q -> %q (must stay stable)", original.IdempotencyKey, redispatched.IdempotencyKey)
	}
}

// TestDefinitionAuditFailsOnMissingDefinition verifies that AuditDefinitions
// returns ErrDefinitionUnavailable when a non-terminal instance's (type,
// version) is not registered in the Registry.
func TestDefinitionAuditFailsOnMissingDefinition(t *testing.T) {
	store := newMemoryStore()

	// Instance references a definition NOT in the registry.
	if _, err := store.Create(context.Background(), StartRequest{
		WorkflowID: "wf-orphan",
		Type:       "missing-flow",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	registry := registryWith(linearDefinition()) // only payment-transfer v1
	engine := NewEngine(store, registry, EngineConfig{})
	recovery := NewRecovery(store, engine, registry, RecoveryConfig{})

	err := recovery.AuditDefinitions(context.Background())
	if !errors.Is(err, ErrDefinitionUnavailable) {
		t.Fatalf("AuditDefinitions: error=%v, want ErrDefinitionUnavailable", err)
	}
}

// TestDefinitionAuditPassesWhenAllRegistered verifies that AuditDefinitions
// returns nil when every non-terminal instance's definition is registered.
func TestDefinitionAuditPassesWhenAllRegistered(t *testing.T) {
	store := newMemoryStore()
	registry := registryWith(linearDefinition())

	if _, err := store.Create(context.Background(), StartRequest{
		WorkflowID: "wf-ok",
		Type:       "payment-transfer",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(store, registry, EngineConfig{})
	recovery := NewRecovery(store, engine, registry, RecoveryConfig{})

	if err := recovery.AuditDefinitions(context.Background()); err != nil {
		t.Fatalf("AuditDefinitions: unexpected error=%v", err)
	}
}

// TestRecoveryRunProcessesPreparingInstance is a focused integration test:
// Recovery.Run claims a preparing instance and calls Engine.Prepare on it,
// then exits when the context is cancelled. Verifies the Run loop wiring.
func TestRecoveryRunProcessesPreparingInstance(t *testing.T) {
	store := newMemoryStore()
	registry := registryWith(linearDefinition())
	engine := NewEngine(store, registry, EngineConfig{})

	if _, err := store.Create(context.Background(), StartRequest{
		WorkflowID: "wf-run",
		Type:       "payment-transfer",
		Version:    1,
		Input:      json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	recovery := NewRecovery(store, engine, registry, RecoveryConfig{
		Owner:        "recovery-test",
		Lease:        5 * time.Second,
		PollInterval: 10 * time.Millisecond,
	})

	// Run for up to 1s — enough for several poll cycles.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = recovery.Run(ctx)

	// The instance should have been claimed, prepared, and released.
	got := store.instance("wf-run")
	assertStatus(t, got, StatusRunning)
	assertActionStatus(t, got, 0, ActionWaitingResult)
	// Lease released after processing.
	if got.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want empty (released after processing)", got.LeaseOwner)
	}
}
