package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// claimLimit is the maximum number of instances Recovery.Run claims and
// processes per poll cycle. It bounds the per-tick work so a large backlog
// cannot starve context cancellation.
const claimLimit = 100

// RecoveryConfig tunes the Recovery loop. The zero value is NOT ready for
// production use — NewRecovery fills in safe defaults, but callers should
// always set Owner to a stable, unique identifier for the running process
// (e.g. pod name + replica id) so leases are attributable.
type RecoveryConfig struct {
	// Owner identifies the process holding leases acquired by this Recovery.
	// Must be non-empty; two Recoveries with the same Owner will steal each
	// other's leases after expiry.
	Owner string

	// Lease is the duration for which a claimed instance is held before
	// another owner may steal it. Default 30s.
	Lease time.Duration

	// PollInterval is the delay between ClaimRunnable sweeps. Default 1s.
	// Lower values reduce recovery latency at the cost of database load.
	PollInterval time.Duration

	// Now supplies the clock used for lease deadlines and timeout checks.
	// Default time.Now. Tests inject a controllable clock.
	Now func() time.Time
}

// Recovery is the startup-and-background loop that drives workflow instances
// forward when no external event arrives: it claims runnable instances
// (preparing, timed-out, transiently failed), resumes them via the Engine,
// and releases the lease on completion. It also runs a startup definition
// audit so an operator is alerted if any non-terminal instance references an
// unregistered definition.
//
// Recovery is safe for concurrent use only when each Recovery has a distinct
// Owner; a single Recovery.Run should be invoked per process.
type Recovery struct {
	store    Store
	engine   *Engine
	registry *Registry
	cfg      RecoveryConfig
}

// NewRecovery wires a Store, Engine, Registry, and RecoveryConfig into a
// Recovery, applying safe defaults for any zero-valued config field:
//
//   - Lease        = 30 * time.Second
//   - PollInterval = 1 * time.Second
//   - Now          = time.Now
//
// Explicit non-zero values are preserved. Owner is NOT defaulted — callers
// MUST set it; an empty Owner produces leases that cannot be attributed.
func NewRecovery(store Store, engine *Engine, registry *Registry, cfg RecoveryConfig) *Recovery {
	if cfg.Lease <= 0 {
		cfg.Lease = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 1 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Recovery{store: store, engine: engine, registry: registry, cfg: cfg}
}

// AuditDefinitions enumerates every non-terminal instance's (Type, Version)
// and verifies each is registered in the Registry. If any is missing, it
// returns a wrapping ErrDefinitionUnavailable so the caller can fail fast at
// startup rather than silently abandoning instances the worker cannot
// process.
//
// This MUST run once before Recovery.Run begins processing; Run calls it
// internally, but operators may also invoke it directly for a pre-flight
// check.
func (r *Recovery) AuditDefinitions(ctx context.Context) error {
	refs, err := r.store.NonTerminalDefinitions(ctx)
	if err != nil {
		return fmt.Errorf("enumerate non-terminal definitions: %w", err)
	}
	for _, ref := range refs {
		if _, ok := r.registry.Get(ref.Type, ref.Version); !ok {
			return fmt.Errorf("%w: type=%q version=%d", ErrDefinitionUnavailable, ref.Type, ref.Version)
		}
	}
	return nil
}

// Run is the blocking recovery loop. It performs a startup definition audit
// (failing fast on ErrDefinitionUnavailable), then repeatedly:
//  1. Claims a batch of runnable instances via Store.ClaimRunnable.
//  2. For each claimed instance: calls Engine.Prepare (advances preparing
//     instances to running) then Engine.Redispatch (re-emits commands for
//     timed-out waiting actions).
//  3. Releases the lease on each instance after processing.
//
// Run blocks until ctx is cancelled and returns ctx.Err(). Processing errors
// on individual instances do not stop the loop — the lease is released and
// the instance becomes claimable again on the next poll.
func (r *Recovery) Run(ctx context.Context) error {
	if err := r.AuditDefinitions(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Swallow per-tick errors — a transient Store failure should not
			// kill the loop. The next tick will retry.
			_ = r.runOnce(ctx)
		}
	}
}

// runOnce performs one claim-process-release sweep. It honours context
// cancellation between claiming and processing each instance so a graceful
// shutdown does not leave dangling leases.
func (r *Recovery) runOnce(ctx context.Context) error {
	now := r.cfg.Now()

	ids, err := r.store.ClaimRunnable(ctx, r.cfg.Owner, now, r.cfg.Lease, claimLimit)
	if err != nil {
		return fmt.Errorf("claim runnable: %w", err)
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.processInstance(ctx, id)
	}
	return nil
}

// processInstance resumes one claimed instance and releases its lease. Both
// Engine.Prepare and Engine.Redispatch are idempotent: Prepare is a no-op on
// an already-running instance, and Redispatch is a no-op when the current
// action has not timed out. This makes processInstance safe to call
// speculatively — the Store's ClaimRunnable already filtered to runnable
// instances, but the instance state may have changed between claim and
// processing (e.g. a concurrent ApplyResult advanced the action).
func (r *Recovery) processInstance(ctx context.Context, id string) {
	// Prepare advances StatusPreparing -> StatusRunning (no-op if already
	// running). A missing definition surfaces as an error here; release the
	// lease so the instance is re-visible, and move on.
	if err := r.engine.Prepare(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
		_ = r.store.ReleaseLease(ctx, id, r.cfg.Owner)
		return
	}

	// Redispatch re-emits the command for a timed-out waiting action (no-op
	// if the action is still within its deadline).
	if err := r.engine.Redispatch(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
		_ = r.store.ReleaseLease(ctx, id, r.cfg.Owner)
		return
	}

	// Release the lease. Processing is complete (or was a no-op); another
	// owner may claim the instance on the next poll if it is still runnable.
	_ = r.store.ReleaseLease(ctx, id, r.cfg.Owner)
}
