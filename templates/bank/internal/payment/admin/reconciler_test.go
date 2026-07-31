package admin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"bank/internal/platform/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeInspector is a CoreBankingInspector stub whose methods return the
// configured errors. The captured-call fields let tests assert the reconciler
// routed validation to the right inspector method with the right identifiers.
type fakeInspector struct {
	holdReleasedErr      error
	reversalVoucherErr   error
	balancesReconcileErr error

	gotHoldID        string
	gotVoucherNo     string
	holdReleasedCall int
	voucherCall      int
	balanceCall      int
}

func (f *fakeInspector) HoldReleased(_ context.Context, holdID string) error {
	f.gotHoldID = holdID
	f.holdReleasedCall++
	return f.holdReleasedErr
}
func (f *fakeInspector) ReversalVoucherExists(_ context.Context, voucherNo string) error {
	f.gotVoucherNo = voucherNo
	f.voucherCall++
	return f.reversalVoucherErr
}
func (f *fakeInspector) BalancesReconcile(_ context.Context, voucherNo string) error {
	f.gotVoucherNo = voucherNo
	f.balanceCall++
	return f.balancesReconcileErr
}

// fakeInstanceReader serves a fixed instance snapshot for a requested id.
type fakeInstanceReader struct {
	inst workflow.Instance
	err  error
}

func (r fakeInstanceReader) Instance(_ context.Context, _ string) (workflow.Instance, error) {
	return r.inst, r.err
}

// buildInstance builds a workflow.Instance carrying an action named name with
// the given forward Output JSON, set to compensation_failed.
func buildInstance(t *testing.T, workflowID, name string, output json.RawMessage) workflow.Instance {
	t.Helper()
	out := output
	if out == nil {
		out = json.RawMessage(`{}`)
	}
	return workflow.Instance{
		ID:     workflowID,
		Type:   "payment-transfer",
		Status: workflow.StatusCompensationFailed,
		Actions: []workflow.ActionRecord{
			{
				Index:  0,
				Name:   name,
				Status: workflow.ActionCompensationFailed,
				Output: out,
			},
		},
	}
}

func TestReconciler_FundsHold_RequiresHoldReleased(t *testing.T) {
	insp := &fakeInspector{holdReleasedErr: errors.New("hold H-1 still active")}
	inst := buildInstance(t, "wf-hold", "PlaceFundsHold", json.RawMessage(`{"hold_id":"H-1"}`))
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	err := r.ValidateReconciliation(context.Background(), "wf-hold", "PlaceFundsHold")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
	}
	if insp.gotHoldID != "H-1" {
		t.Fatalf("inspector got hold_id %q, want %q", insp.gotHoldID, "H-1")
	}
	if insp.voucherCall != 0 || insp.balanceCall != 0 {
		t.Fatalf("voucher/balance inspectors should not run for funds-hold")
	}
}

func TestReconciler_FundsHold_AcceptsWhenHoldReleased(t *testing.T) {
	insp := &fakeInspector{}
	inst := buildInstance(t, "wf-hold", "PlaceFundsHold", json.RawMessage(`{"hold_id":"H-7"}`))
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	if err := r.ValidateReconciliation(context.Background(), "wf-hold", "PlaceFundsHold"); err != nil {
		t.Fatalf("ValidateReconciliation returned %v, want nil", err)
	}
	if insp.gotHoldID != "H-7" {
		t.Fatalf("inspector got hold_id %q, want %q", insp.gotHoldID, "H-7")
	}
}

func TestReconciler_FundsHold_RejectsMissingHoldID(t *testing.T) {
	insp := &fakeInspector{}
	inst := buildInstance(t, "wf-hold", "PlaceFundsHold", json.RawMessage(`{}`))
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	err := r.ValidateReconciliation(context.Background(), "wf-hold", "PlaceFundsHold")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
	}
	if insp.holdReleasedCall != 0 {
		t.Fatalf("inspector must not be called when hold_id is absent")
	}
}

func TestReconciler_LedgerTransfer_RequiresReversalVoucherAndBalances(t *testing.T) {
	// Reversal voucher missing → reject before balance check.
	t.Run("reversal voucher missing", func(t *testing.T) {
		insp := &fakeInspector{reversalVoucherErr: errors.New("no reversal voucher for V-9")}
		inst := buildInstance(t, "wf-xfer", "PostLedgerTransfer", json.RawMessage(`{"voucher_no":"V-9"}`))
		r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

		err := r.ValidateReconciliation(context.Background(), "wf-xfer", "PostLedgerTransfer")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
		}
		if insp.voucherCall != 1 {
			t.Fatalf("voucher inspector call = %d, want 1", insp.voucherCall)
		}
		if insp.balanceCall != 0 {
			t.Fatalf("balance inspector must not run when reversal voucher is missing")
		}
	})

	// Reversal voucher exists but balances do not reconcile → reject.
	t.Run("balances do not reconcile", func(t *testing.T) {
		insp := &fakeInspector{balancesReconcileErr: errors.New("ledger out by 500")}
		inst := buildInstance(t, "wf-xfer", "PostLedgerTransfer", json.RawMessage(`{"voucher_no":"V-9"}`))
		r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

		err := r.ValidateReconciliation(context.Background(), "wf-xfer", "PostLedgerTransfer")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
		}
		if insp.balanceCall != 1 {
			t.Fatalf("balance inspector call = %d, want 1", insp.balanceCall)
		}
	})

	// Both checks pass → accept, and balance must be checked AFTER voucher.
	t.Run("voucher and balances both pass", func(t *testing.T) {
		insp := &fakeInspector{}
		inst := buildInstance(t, "wf-xfer", "PostLedgerTransfer", json.RawMessage(`{"voucher_no":"V-9"}`))
		r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

		if err := r.ValidateReconciliation(context.Background(), "wf-xfer", "PostLedgerTransfer"); err != nil {
			t.Fatalf("ValidateReconciliation returned %v, want nil", err)
		}
		if insp.gotVoucherNo != "V-9" {
			t.Fatalf("inspector got voucher_no %q, want %q", insp.gotVoucherNo, "V-9")
		}
		if insp.voucherCall != 1 || insp.balanceCall != 1 {
			t.Fatalf("voucher/balance calls = %d/%d, want 1/1", insp.voucherCall, insp.balanceCall)
		}
	})
}

func TestReconciler_LedgerTransfer_RejectsMissingVoucherNo(t *testing.T) {
	insp := &fakeInspector{}
	inst := buildInstance(t, "wf-xfer", "PostLedgerTransfer", json.RawMessage(`{}`))
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	err := r.ValidateReconciliation(context.Background(), "wf-xfer", "PostLedgerTransfer")
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
	}
	if insp.voucherCall != 0 || insp.balanceCall != 0 {
		t.Fatalf("inspectors must not be called when voucher_no is absent")
	}
}

func TestReconciler_NonFinancialAction_NeedsNoExternalValidation(t *testing.T) {
	// AuthorizeRisk (risk void) is non-financial: no durable external state to
	// reconcile, so validation is a no-op regardless of inspector state.
	insp := &fakeInspector{holdReleasedErr: errors.New("must not be called")}
	inst := buildInstance(t, "wf-risk", "AuthorizeRisk", nil)
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	if err := r.ValidateReconciliation(context.Background(), "wf-risk", "AuthorizeRisk"); err != nil {
		t.Fatalf("ValidateReconciliation returned %v, want nil", err)
	}
}

func TestReconciler_MissingInstance_NotFound(t *testing.T) {
	insp := &fakeInspector{}
	r := NewActionReconciler(fakeInstanceReader{err: workflow.ErrInstanceNotFound}, insp)

	err := r.ValidateReconciliation(context.Background(), "wf-missing", "PlaceFundsHold")
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.NotFound, err)
	}
}

func TestReconciler_FindsActionByName(t *testing.T) {
	// An instance with several actions; the reconciler must pick the one whose
	// Name matches, not a positional index.
	insp := &fakeInspector{}
	inst := workflow.Instance{
		ID:     "wf-multi",
		Status: workflow.StatusCompensationFailed,
		Actions: []workflow.ActionRecord{
			{Name: "AuthorizeRisk", Status: workflow.ActionCompensated},
			{Name: "PlaceFundsHold", Status: workflow.ActionCompensationFailed, Output: json.RawMessage(`{"hold_id":"H-multi"}`)},
		},
	}
	r := NewActionReconciler(fakeInstanceReader{inst: inst}, insp)

	if err := r.ValidateReconciliation(context.Background(), "wf-multi", "PlaceFundsHold"); err != nil {
		t.Fatalf("ValidateReconciliation returned %v, want nil", err)
	}
	if insp.gotHoldID != "H-multi" {
		t.Fatalf("inspector got hold_id %q, want %q", insp.gotHoldID, "H-multi")
	}
}
