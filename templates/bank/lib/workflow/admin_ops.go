package workflow

import (
	"context"
	"fmt"
)

// This file adds the operator-driven compensation recovery operations. They are
// the engine-side counterparts to the PROTECTED admin gRPC surface:
//
//   - RetryCompensation    — re-dispatch the compensation command for an
//                            instance stuck in StatusCompensationFailed.
//   - ResolveCompensation  — mark a stuck compensation action resolved
//                            (compensated) and continue the compensation walk.
//                            The caller (admin service) is responsible for
//                            validating the external reconciliation state
//                            BEFORE invoking ResolveCompensation.
//
// Both operations are offered in two forms: a Tx-form (*Tx suffix) that runs
// against an instance already locked in a caller-owned Store.WithInstance( Tx)
// transaction, and a self-contained form that opens its own WithInstance
// transaction. The Tx-form lets the admin gateway persist its immutable audit
// row in the SAME transaction as the state change (atomic), so a crash can
// never leave a state transition without its audit record or vice versa.

// RetryCompensation re-dispatches the compensation command for an instance
// whose compensation has failed (StatusCompensationFailed). It is the
// operator-driven recovery path complementing the automatic retry inside
// ApplyCompensationResult. The instance transitions to StatusCompensating and
// the failed action is re-dispatched with a fresh CommandID, a stable
// semantic IdempotencyKey, and Attempt incremented past the value that
// exhausted automatic retries.
//
// Idempotent guard: the instance MUST be StatusCompensationFailed and the
// current action MUST be ActionCompensationFailed; any other state returns an
// error so a concurrent caller or a double-click cannot mutate a healthy
// instance. Returns the previous and new instance status.
func (e *Engine) RetryCompensation(ctx context.Context, id string) (InstanceStatus, InstanceStatus, error) {
	var prev, curr InstanceStatus
	err := e.store.WithInstance(ctx, id, func(tx Tx) error {
		var pErr error
		prev, curr, pErr = e.RetryCompensationTx(tx, ctx)
		return pErr
	})
	return prev, curr, err
}

// RetryCompensationTx is the Tx-form of RetryCompensation. It operates on the
// instance locked in the caller's Store.WithInstance transaction so the audit
// row can be committed atomically. See RetryCompensation for the contract.
func (e *Engine) RetryCompensationTx(tx Tx, ctx context.Context) (InstanceStatus, InstanceStatus, error) {
	current := tx.Instance()
	prev := current.Status

	if current.Status != StatusCompensationFailed {
		return prev, prev, fmt.Errorf(
			"RetryCompensation: %w: instance %q status %q is not %q",
			ErrInvalidCompensationState, current.ID, current.Status, StatusCompensationFailed)
	}
	actionIdx := current.CurrentAction
	if actionIdx < 0 || actionIdx >= len(current.Actions) {
		return prev, prev, fmt.Errorf(
			"RetryCompensation: %w: instance %q current action index %d out of range (have %d actions)",
			ErrInvalidCompensationState, current.ID, actionIdx, len(current.Actions))
	}
	actionRec := current.Actions[actionIdx]
	if actionRec.Status != ActionCompensationFailed {
		return prev, prev, fmt.Errorf(
			"RetryCompensation: %w: action %q status %q is not %q",
			ErrInvalidCompensationState, actionRec.Name, actionRec.Status, ActionCompensationFailed)
	}

	def, ok := e.registry.Get(current.Type, current.Version)
	if !ok {
		return prev, prev, fmt.Errorf("RetryCompensation: %w: type=%q version=%d",
			ErrDefinitionNotFound, current.Type, current.Version)
	}

	inst := *current
	inst.Status = StatusCompensating
	inst.Revision++
	if err := tx.SaveInstance(inst); err != nil {
		return prev, prev, fmt.Errorf("RetryCompensation: save instance: %w", err)
	}
	e.metrics.changeStatus(prev, StatusCompensating)

	// Re-dispatch with Attempt incremented past the value that exhausted
	// automatic retries. persistCompensationDispatch preserves the forward
	// Output (read from inst.Actions[actionIdx]) so Compensate can rebuild the
	// undo command, and uses a STABLE semantic IdempotencyKey so the recipient
	// deduplicates against the original compensation command.
	if err := e.persistCompensationDispatch(tx, inst, def, actionIdx, actionRec.Attempt+1, ctx); err != nil {
		return prev, StatusCompensating, err
	}
	return prev, StatusCompensating, nil
}

// ResolveCompensation marks the named action's compensation as resolved
// (ActionCompensated) and continues the compensation walk: it dispatches the
// previous succeeded action's compensation, or transitions the instance to
// StatusCompensated when no succeeded action remains to undo. It is the
// engine-side of the operator-driven reconciliation path.
//
// The caller (admin service) MUST validate the external reconciliation state
// (funds hold released / reversal voucher + balance reconciliation) BEFORE
// invoking this method; the engine performs only the workflow-state
// precondition check and the state transition. Returns the previous and new
// instance status.
func (e *Engine) ResolveCompensation(ctx context.Context, id, actionName string) (InstanceStatus, InstanceStatus, error) {
	var prev, curr InstanceStatus
	err := e.store.WithInstance(ctx, id, func(tx Tx) error {
		var pErr error
		prev, curr, pErr = e.ResolveCompensationTx(tx, ctx, actionName)
		return pErr
	})
	return prev, curr, err
}

// ResolveCompensationTx is the Tx-form of ResolveCompensation. It operates on
// the instance locked in the caller's Store.WithInstance transaction so the
// audit row can be committed atomically. See ResolveCompensation for the
// contract.
func (e *Engine) ResolveCompensationTx(tx Tx, ctx context.Context, actionName string) (InstanceStatus, InstanceStatus, error) {
	current := tx.Instance()
	prev := current.Status

	if current.Status != StatusCompensating && current.Status != StatusCompensationFailed {
		return prev, prev, fmt.Errorf(
			"ResolveCompensation: %w: instance %q status %q is not %q or %q",
			ErrInvalidCompensationState, current.ID, current.Status, StatusCompensating, StatusCompensationFailed)
	}

	// Locate the action by Name — not positional index — so the operator's
	// reference is robust to action reordering.
	actionIdx := -1
	for i, a := range current.Actions {
		if a.Name == actionName {
			actionIdx = i
			break
		}
	}
	if actionIdx < 0 {
		return prev, prev, fmt.Errorf("ResolveCompensation: %w: action %q not found on instance %q",
			ErrInvalidMessage, actionName, current.ID)
	}
	actionRec := current.Actions[actionIdx]
	if actionRec.Status != ActionCompensationFailed && actionRec.Status != ActionCompensating {
		return prev, prev, fmt.Errorf(
			"ResolveCompensation: %w: action %q status %q is not %q or %q",
			ErrInvalidCompensationState, actionName, actionRec.Status, ActionCompensationFailed, ActionCompensating)
	}

	def, ok := e.registry.Get(current.Type, current.Version)
	if !ok {
		return prev, prev, fmt.Errorf("ResolveCompensation: %w: type=%q version=%d",
			ErrDefinitionNotFound, current.Type, current.Version)
	}

	inst := *current

	// Mark the resolved action compensated and clear its transient error
	// provenance. The forward Output is preserved by SaveAction's
	// COALESCE(EXCLUDED.output, workflow_action.output) so a future audit can
	// still reconstruct what was undone.
	actionRec.Status = ActionCompensated
	actionRec.LastErrorClass = ""
	actionRec.LastError = ""
	if err := tx.SaveAction(actionRec); err != nil {
		return prev, prev, fmt.Errorf("ResolveCompensation: save compensated action %q: %w", actionName, err)
	}

	// Continue the compensation walk in reverse order: dispatch the previous
	// succeeded action's compensation, or finish when none remain. This mirrors
	// the success tail of applyCompensationResult so the operator-resolved
	// action integrates with the normal compensation flow.
	target := lastSucceededBefore(inst, actionIdx)
	if target < 0 {
		inst.Status = StatusCompensated
		inst.Revision++
		if err := tx.SaveInstance(inst); err != nil {
			return prev, prev, fmt.Errorf("ResolveCompensation: save instance: %w", err)
		}
		e.metrics.changeStatus(prev, StatusCompensated)
		return prev, StatusCompensated, nil
	}

	inst.CurrentAction = target
	if inst.Status != StatusCompensating {
		inst.Status = StatusCompensating
	}
	inst.Revision++
	if err := tx.SaveInstance(inst); err != nil {
		return prev, prev, fmt.Errorf("ResolveCompensation: save instance: %w", err)
	}
	if prev != StatusCompensating {
		e.metrics.changeStatus(prev, StatusCompensating)
	}
	if err := e.persistCompensationDispatch(tx, inst, def, target, 1, ctx); err != nil {
		return prev, inst.Status, err
	}
	return prev, inst.Status, nil
}
