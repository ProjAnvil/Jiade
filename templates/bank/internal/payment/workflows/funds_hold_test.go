package workflows

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// TestPlaceFundsHold_Execute_DispatchContract asserts the EXACT forward dispatch
// contract: core.place-hold.v1 routing, stable idempotency key, 15s deadline,
// and held/hold-failed accepted result types.
func TestPlaceFundsHold_Execute_DispatchContract(t *testing.T) {
	action := &PlaceFundsHold{}
	view := buildView("wf-2", 1, "PlaceFundsHold", nil)

	dispatch, err := action.Execute(context.Background(), view)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if dispatch.RoutingKey != "core.place-hold.v1" {
		t.Errorf("RoutingKey = %q, want core.place-hold.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-2:place-funds-hold"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"core.hold-placed.v1", "core.hold-failed.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	var payload fundsHoldPlacePayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.AccountNo != "ACC-PAYER" {
		t.Errorf("payload.account_no = %q, want ACC-PAYER", payload.AccountNo)
	}
	if payload.AmountCents != 50000 {
		t.Errorf("payload.amount_cents = %d, want 50000", payload.AmountCents)
	}
	if payload.Currency != "CNY" {
		t.Errorf("payload.currency = %q, want CNY", payload.Currency)
	}
}

// TestPlaceFundsHold_ApplyResult_Held asserts a hold-placed result yields
// success with the hold_id in the Output (consumed by PostLedgerTransfer and
// by the release-hold compensation).
func TestPlaceFundsHold_ApplyResult_Held(t *testing.T) {
	action := &PlaceFundsHold{}
	view := buildView("wf-2", 1, "PlaceFundsHold", nil)
	env := messaging.Envelope{
		MessageType: "core.hold-placed.v1",
		Payload:     jsonMust(t, map[string]any{"hold_id": "H-42", "account_no": "ACC-PAYER"}),
	}

	outcome, err := action.ApplyResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("Succeeded = false, want true (msg=%q)", outcome.Message)
	}
	var out fundsHoldOutput
	if err := json.Unmarshal(outcome.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.HoldID != "H-42" {
		t.Errorf("output.hold_id = %q, want H-42", out.HoldID)
	}
}

// TestPlaceFundsHold_ApplyResult_HoldFailed_Classification asserts the
// hold-failed result's embedded error_class is mapped to the workflow engine's
// ErrorClass per the brief: rejected/insufficient → business_rejected;
// broker/dependency → transient_failure; ledger balance errors →
// invariant_violation; structural → invalid_message; empty → unknown_outcome.
func TestPlaceFundsHold_ApplyResult_HoldFailed_Classification(t *testing.T) {
	action := &PlaceFundsHold{}
	view := buildView("wf-2", 1, "PlaceFundsHold", nil)

	cases := []struct {
		name      string
		className string
		want      workflow.ErrorClass
	}{
		{"insufficient funds", "business_rejected", workflow.BusinessRejected},
		{"broker unavailable", "transient_failure", workflow.TransientFailure},
		{"ledger balance error", "invariant_violation", workflow.InvariantViolation},
		{"malformed command", "invalid_message", workflow.InvalidMessage},
		{"unrecognised class", "some_future_class", workflow.UnknownOutcome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := messaging.Envelope{
				MessageType: "core.hold-failed.v1",
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

// TestPlaceFundsHold_Compensate_DispatchContract asserts the compensation
// dispatch contract: core.release-hold.v1 routing, stable compensation
// idempotency key, 15s deadline, and released/release-failed result types.
func TestPlaceFundsHold_Compensate_DispatchContract(t *testing.T) {
	action := &PlaceFundsHold{}
	view := compensateView("wf-8", 1, "PlaceFundsHold",
		jsonMust(t, fundsHoldOutput{HoldID: "H-8"}))

	dispatch, err := action.Compensate(context.Background(), view)
	if err != nil {
		t.Fatalf("Compensate: %v", err)
	}

	if dispatch.RoutingKey != "core.release-hold.v1" {
		t.Errorf("RoutingKey = %q, want core.release-hold.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-8:compensate:place-funds-hold"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"core.hold-released.v1", "core.hold-release-failed.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	var payload fundsHoldReleasePayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.HoldID != "H-8" {
		t.Errorf("payload.hold_id = %q, want H-8", payload.HoldID)
	}
}

// TestPlaceFundsHold_ApplyCompensationResult_Released asserts a hold-released
// result yields a successful compensation outcome.
func TestPlaceFundsHold_ApplyCompensationResult_Released(t *testing.T) {
	action := &PlaceFundsHold{}
	view := compensateView("wf-8", 1, "PlaceFundsHold",
		jsonMust(t, fundsHoldOutput{HoldID: "H-8"}))
	env := messaging.Envelope{
		MessageType: "core.hold-released.v1",
		Payload:     jsonMust(t, map[string]any{"hold_id": "H-8", "account_no": "ACC-PAYER"}),
	}

	outcome, err := action.ApplyCompensationResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyCompensationResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("Succeeded = false, want true (msg=%q)", outcome.Message)
	}
}
