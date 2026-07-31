package workflows

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// TestPostLedgerTransfer_Execute_DispatchContract asserts the EXACT forward
// dispatch contract: core.post-held-transfer.v1 routing, stable idempotency
// key, 15s deadline, and transfer-posted/transfer-failed accepted result
// types. It also verifies Execute reads the hold_id from the prior
// PlaceFundsHold action's Output.
func TestPostLedgerTransfer_Execute_DispatchContract(t *testing.T) {
	action := &PostLedgerTransfer{}
	prior := []workflow.ActionRecord{
		{Index: 0, Name: "AuthorizeRisk", Status: workflow.ActionSucceeded,
			Output: jsonMust(t, riskAuthorizeOutput{AuthorizationID: "authz:wf-3"})},
		{Index: 1, Name: "PlaceFundsHold", Status: workflow.ActionSucceeded,
			Output: jsonMust(t, fundsHoldOutput{HoldID: "H-3"})},
	}
	view := buildView("wf-3", 2, "PostLedgerTransfer", prior)

	dispatch, err := action.Execute(context.Background(), view)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if dispatch.RoutingKey != "core.post-held-transfer.v1" {
		t.Errorf("RoutingKey = %q, want core.post-held-transfer.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-3:post-ledger-transfer"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"core.transfer-posted.v1", "core.transfer-failed.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	var payload ledgerTransferPostPayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.HoldID != "H-3" {
		t.Errorf("payload.hold_id = %q, want H-3 (from prior PlaceFundsHold output)", payload.HoldID)
	}
	if payload.FromAccount != "ACC-PAYER" {
		t.Errorf("payload.from_account = %q, want ACC-PAYER", payload.FromAccount)
	}
	if payload.ToAccount != "ACC-PAYEE" {
		t.Errorf("payload.to_account = %q, want ACC-PAYEE", payload.ToAccount)
	}
	if payload.AmountCents != 50000 {
		t.Errorf("payload.amount_cents = %d, want 50000", payload.AmountCents)
	}
	if payload.Currency != "CNY" {
		t.Errorf("payload.currency = %q, want CNY", payload.Currency)
	}
}

// TestPostLedgerTransfer_Execute_MissingHoldID asserts Execute fails clearly
// when the prior PlaceFundsHold output is absent (defensive: the engine only
// dispatches action 2 after action 1 succeeds, but a garbled Instance must not
// silently produce a command without a hold_id).
func TestPostLedgerTransfer_Execute_MissingHoldID(t *testing.T) {
	action := &PostLedgerTransfer{}
	// No prior actions at all.
	view := buildView("wf-3", 2, "PostLedgerTransfer", nil)

	if _, err := action.Execute(context.Background(), view); err == nil {
		t.Fatal("Execute with no prior PlaceFundsHold output: want error, got nil")
	}
}

// TestPostLedgerTransfer_ApplyResult_Posted asserts a transfer-posted result
// yields success with the voucher_no in the Output (consumed by the
// reverse-transfer compensation).
func TestPostLedgerTransfer_ApplyResult_Posted(t *testing.T) {
	action := &PostLedgerTransfer{}
	prior := []workflow.ActionRecord{
		{Index: 1, Name: "PlaceFundsHold", Status: workflow.ActionSucceeded,
			Output: jsonMust(t, fundsHoldOutput{HoldID: "H-3"})},
	}
	view := buildView("wf-3", 2, "PostLedgerTransfer", prior)
	env := messaging.Envelope{
		MessageType: "core.transfer-posted.v1",
		Payload:     jsonMust(t, map[string]any{"voucher_no": "V-9001", "hold_id": "H-3"}),
	}

	outcome, err := action.ApplyResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("Succeeded = false, want true (msg=%q)", outcome.Message)
	}
	var out ledgerTransferOutput
	if err := json.Unmarshal(outcome.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.VoucherNo != "V-9001" {
		t.Errorf("output.voucher_no = %q, want V-9001", out.VoucherNo)
	}
}

// TestPostLedgerTransfer_ApplyResult_Failed_Classification asserts the
// transfer-failed result's embedded error_class is mapped to the engine's
// ErrorClass per the brief.
func TestPostLedgerTransfer_ApplyResult_Failed_Classification(t *testing.T) {
	action := &PostLedgerTransfer{}
	view := buildView("wf-3", 2, "PostLedgerTransfer", nil)

	cases := []struct {
		name      string
		className string
		want      workflow.ErrorClass
	}{
		{"insufficient funds", "business_rejected", workflow.BusinessRejected},
		{"dependency down", "transient_failure", workflow.TransientFailure},
		{"ledger invariant", "invariant_violation", workflow.InvariantViolation},
		{"malformed", "invalid_message", workflow.InvalidMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := messaging.Envelope{
				MessageType: "core.transfer-failed.v1",
				Payload: jsonMust(t, failurePayload{
					ErrorClass:   tc.className,
					ErrorMessage: tc.name,
				}),
			}
			outcome, err := action.ApplyResult(context.Background(), view, env)
			if err != nil {
				t.Fatalf("ApplyResult: %v", err)
			}
			if outcome.Succeeded {
				t.Fatal("Succeeded = true, want false")
			}
			if outcome.Class != tc.want {
				t.Errorf("Class = %q, want %q", outcome.Class, tc.want)
			}
		})
	}
}

// TestPostLedgerTransfer_Compensate_DispatchContract asserts the compensation
// dispatch contract: core.reverse-transfer.v1 routing, stable compensation
// idempotency key, 15s deadline, and reversed/reverse-failed result types.
func TestPostLedgerTransfer_Compensate_DispatchContract(t *testing.T) {
	action := &PostLedgerTransfer{}
	view := compensateView("wf-9", 2, "PostLedgerTransfer",
		jsonMust(t, ledgerTransferOutput{VoucherNo: "V-9"}))

	dispatch, err := action.Compensate(context.Background(), view)
	if err != nil {
		t.Fatalf("Compensate: %v", err)
	}

	if dispatch.RoutingKey != "core.reverse-transfer.v1" {
		t.Errorf("RoutingKey = %q, want core.reverse-transfer.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-9:compensate:post-ledger-transfer"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"core.transfer-reversed.v1", "core.transfer-reverse-failed.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	var payload ledgerTransferReversePayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.OriginalVoucherNo != "V-9" {
		t.Errorf("payload.original_voucher_no = %q, want V-9", payload.OriginalVoucherNo)
	}
}

// TestPostLedgerTransfer_ApplyCompensationResult_Reversed asserts a
// transfer-reversed result yields a successful compensation outcome.
func TestPostLedgerTransfer_ApplyCompensationResult_Reversed(t *testing.T) {
	action := &PostLedgerTransfer{}
	view := compensateView("wf-9", 2, "PostLedgerTransfer",
		jsonMust(t, ledgerTransferOutput{VoucherNo: "V-9"}))
	env := messaging.Envelope{
		MessageType: "core.transfer-reversed.v1",
		Payload:     jsonMust(t, map[string]any{"reversal_voucher_no": "RV-1", "original_voucher_no": "V-9"}),
	}

	outcome, err := action.ApplyCompensationResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyCompensationResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("Succeeded = false, want true (msg=%q)", outcome.Message)
	}
}
