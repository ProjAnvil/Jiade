package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"bank/internal/platform/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Action name constants mirrored from internal/payment/workflows so this
// package does not import the workflows package (which would pull the payment
// definition into the admin surface). A rename in workflows transfers here.
const (
	actionPlaceFundsHold     = "PlaceFundsHold"
	actionPostLedgerTransfer = "PostLedgerTransfer"
)

// Reconciler validates that the external core-banking state permits the named
// action's compensation to be marked resolved. The admin service calls
// ValidateReconciliation before RecordReconciliation's state transition.
type Reconciler interface {
	// ValidateReconciliation returns nil if the current external state for the
	// action permits resolving its compensation; otherwise it returns a gRPC
	// status error (typically FailedPrecondition).
	ValidateReconciliation(ctx context.Context, workflowID, actionName string) error
}

// CoreBankingInspector verifies ONE specific external core-banking condition
// for a reconciliation check. Each method receives ONLY the identifier the
// inspector needs to look up the specific hold or posting: the reconciler
// parses the action's forward Output JSON and extracts the hold_id (for a
// funds hold) or voucher_no (for a ledger transfer) BEFORE invoking the
// inspector, and rejects with FailedPrecondition when that identifier is
// missing from the Output. This keeps the inspector trivial to fake in tests
// and gives operators a precise error ("hold_id absent") instead of a generic
// core-banking lookup of an empty key.
//
// Each method returns nil if the named condition currently holds, or a
// descriptive error otherwise (the reconciler wraps that error as
// FailedPrecondition).
type CoreBankingInspector interface {
	// HoldReleased verifies the funds hold identified by holdID is currently
	// released in core-banking.
	HoldReleased(ctx context.Context, holdID string) error
	// ReversalVoucherExists verifies a reversal voucher exists for the original
	// posting identified by voucherNo.
	ReversalVoucherExists(ctx context.Context, voucherNo string) error
	// BalancesReconcile verifies the ledger balances for the original posting
	// identified by voucherNo reconcile (the reversal netted it to zero).
	BalancesReconcile(ctx context.Context, voucherNo string) error
}

// InstanceReader loads a read-only workflow instance snapshot. The production
// implementation wraps *workflow.PostgresStore.WithInstance with a read-only
// callback.
type InstanceReader interface {
	Instance(ctx context.Context, workflowID string) (workflow.Instance, error)
}

// ActionReconciler is the concrete Reconciler. It loads the instance, finds
// the action by name, and dispatches the per-action-type validation to the
// CoreBankingInspector.
//
// Validation rules (brief Step 1):
//   - PlaceFundsHold     → the hold MUST be released.
//   - PostLedgerTransfer → a reversal voucher MUST exist AND balances MUST
//     reconcile (checked in that order).
//   - any other action   → no durable financial state to validate (e.g. a risk
//     void), so the reconciler accepts.
type ActionReconciler struct {
	reader InstanceReader
	core   CoreBankingInspector
}

// NewActionReconciler wires the instance reader and core-banking inspector.
func NewActionReconciler(reader InstanceReader, core CoreBankingInspector) *ActionReconciler {
	return &ActionReconciler{reader: reader, core: core}
}

func (r *ActionReconciler) ValidateReconciliation(ctx context.Context, workflowID, actionName string) error {
	inst, err := r.reader.Instance(ctx, workflowID)
	if err != nil {
		return mapInstanceError(err, workflowID)
	}
	action, ok := findAction(inst, actionName)
	if !ok {
		return status.Errorf(codes.FailedPrecondition, "action %q not found on workflow %q", actionName, workflowID)
	}
	switch actionName {
	case actionPlaceFundsHold:
		holdID, err := actionOutputString(action, "hold_id")
		if err != nil {
			// Misrecorded forward Output: the compensation cannot be validated
			// against core-banking. Surface as FailedPrecondition so the
			// operator sees the data gap without a misleading core-banking
			// lookup of an empty hold_id. The inspector is NOT called.
			return status.Errorf(codes.FailedPrecondition,
				"funds hold action %q on %q: %v", actionName, workflowID, err)
		}
		if err := r.core.HoldReleased(ctx, holdID); err != nil {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("funds hold not released: %v", err))
		}
		return nil
	case actionPostLedgerTransfer:
		voucherNo, err := actionOutputString(action, "voucher_no")
		if err != nil {
			return status.Errorf(codes.FailedPrecondition,
				"ledger transfer action %q on %q: %v", actionName, workflowID, err)
		}
		// Order matters: a missing reversal voucher is the more fundamental
		// failure, so check it BEFORE balance reconciliation.
		if err := r.core.ReversalVoucherExists(ctx, voucherNo); err != nil {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("no reversal voucher: %v", err))
		}
		if err := r.core.BalancesReconcile(ctx, voucherNo); err != nil {
			return status.Error(codes.FailedPrecondition, fmt.Sprintf("balances do not reconcile: %v", err))
		}
		return nil
	default:
		// Non-financial action (e.g. AuthorizeRisk, a risk void): the
		// compensation command is itself the undo and leaves no durable
		// financial state requiring external reconciliation, so validation is
		// a no-op.
		return nil
	}
}

// actionOutputString extracts a single string field by key from the action's
// forward Output JSON. Returns a descriptive error when the Output is missing,
// not a JSON object, lacks the key, or holds an empty string — every case the
// reconciler maps to FailedPrecondition. A nil/empty Output yields the same
// "missing key" error as an empty JSON object (`{}`) for consistency.
func actionOutputString(action workflow.ActionRecord, key string) (string, error) {
	if len(action.Output) == 0 {
		return "", fmt.Errorf("forward output has no %q", key)
	}
	var m map[string]any
	if err := json.Unmarshal(action.Output, &m); err != nil {
		return "", fmt.Errorf("decode forward output: %w", err)
	}
	raw, ok := m[key]
	if !ok {
		return "", fmt.Errorf("forward output has no %q", key)
	}
	s, _ := raw.(string)
	if s == "" {
		return "", fmt.Errorf("forward output %q is empty", key)
	}
	return s, nil
}

// findAction returns the action record named actionName on the instance. The
// match is by Name (not positional index) so the operator's reference is
// robust to action reordering.
func findAction(inst workflow.Instance, actionName string) (workflow.ActionRecord, bool) {
	for _, a := range inst.Actions {
		if a.Name == actionName {
			return a, true
		}
	}
	return workflow.ActionRecord{}, false
}

// mapInstanceError translates a store/reader error into a gRPC status. A
// not-found instance surfaces as NotFound so operators see a clear 404.
func mapInstanceError(err error, workflowID string) error {
	if err == nil {
		return nil
	}
	if err == workflow.ErrInstanceNotFound {
		return status.Errorf(codes.NotFound, "workflow %q not found", workflowID)
	}
	return status.Errorf(codes.Internal, "load workflow %q: %v", workflowID, err)
}
