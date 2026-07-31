package workflows

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// TestAuthorizeRisk_Execute_DispatchContract asserts the EXACT forward dispatch
// contract from the saga brief: routing key, idempotency key, 15s deadline, and
// accepted result types for the risk.authorize-payment.v1 command.
func TestAuthorizeRisk_Execute_DispatchContract(t *testing.T) {
	action := &AuthorizeRisk{}
	view := buildView("wf-1", 0, "AuthorizeRisk", nil)

	dispatch, err := action.Execute(context.Background(), view)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if dispatch.RoutingKey != "risk.authorize-payment.v1" {
		t.Errorf("RoutingKey = %q, want risk.authorize-payment.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-1:authorize-risk"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"risk.payment-authorized.v1", "risk.payment-rejected.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	// The payload must carry the authorization_id, customer_id, amount, and
	// currency the risk consumer expects.
	var payload riskAuthorizePaymentPayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.AuthorizationID == "" {
		t.Errorf("payload.authorization_id is empty; must be derived from workflow id")
	}
	if payload.CustomerID != "C-100" {
		t.Errorf("payload.customer_id = %q, want C-100", payload.CustomerID)
	}
	if payload.AmountCents != 50000 {
		t.Errorf("payload.amount_cents = %d, want 50000", payload.AmountCents)
	}
	if payload.Currency != "CNY" {
		t.Errorf("payload.currency = %q, want CNY", payload.Currency)
	}
}

// TestAuthorizeRisk_ApplyResult_Authorized asserts the authorized result yields
// a success outcome whose Output carries the authorization_id (needed by the
// void compensation step).
func TestAuthorizeRisk_ApplyResult_Authorized(t *testing.T) {
	action := &AuthorizeRisk{}
	view := buildView("wf-1", 0, "AuthorizeRisk", nil)
	env := messaging.Envelope{
		MessageType: "risk.payment-authorized.v1",
		Payload:     jsonMust(t, map[string]any{"authorization_id": "authz-9", "customer_id": "C-100"}),
	}

	outcome, err := action.ApplyResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("outcome.Succeeded = false, want true (msg=%q)", outcome.Message)
	}

	var out riskAuthorizeOutput
	if err := json.Unmarshal(outcome.Output, &out); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if out.AuthorizationID != "authz-9" {
		t.Errorf("output.authorization_id = %q, want authz-9", out.AuthorizationID)
	}
}

// TestAuthorizeRisk_ApplyResult_Rejected asserts a domain rejection maps to
// BusinessRejected (terminal — triggers compensation of prior actions).
func TestAuthorizeRisk_ApplyResult_Rejected(t *testing.T) {
	action := &AuthorizeRisk{}
	view := buildView("wf-1", 0, "AuthorizeRisk", nil)
	env := messaging.Envelope{
		MessageType: "risk.payment-rejected.v1",
		Payload:     jsonMust(t, map[string]any{"authorization_id": "authz-9"}),
	}

	outcome, err := action.ApplyResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	if outcome.Succeeded {
		t.Fatal("outcome.Succeeded = true, want false")
	}
	if outcome.Class != workflow.BusinessRejected {
		t.Errorf("Class = %q, want %q", outcome.Class, workflow.BusinessRejected)
	}
}

// TestAuthorizeRisk_Compensate_DispatchContract asserts the EXACT compensation
// dispatch contract: void routing key, stable compensation idempotency key,
// 15s deadline, and the voided result type.
func TestAuthorizeRisk_Compensate_DispatchContract(t *testing.T) {
	action := &AuthorizeRisk{}
	view := compensateView("wf-7", 0, "AuthorizeRisk",
		jsonMust(t, riskAuthorizeOutput{AuthorizationID: "authz:wf-7"}))

	dispatch, err := action.Compensate(context.Background(), view)
	if err != nil {
		t.Fatalf("Compensate: %v", err)
	}

	if dispatch.RoutingKey != "risk.void-payment-authorization.v1" {
		t.Errorf("RoutingKey = %q, want risk.void-payment-authorization.v1", dispatch.RoutingKey)
	}
	if want := "wf:wf-7:compensate:authorize-risk"; dispatch.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q", dispatch.IdempotencyKey, want)
	}
	if dispatch.Deadline != 15*time.Second {
		t.Errorf("Deadline = %v, want 15s", dispatch.Deadline)
	}
	wantAccepted := []string{"risk.payment-authorization-voided.v1"}
	if !sameStrings(dispatch.AcceptedResultTypes, wantAccepted) {
		t.Errorf("AcceptedResultTypes = %v, want %v", dispatch.AcceptedResultTypes, wantAccepted)
	}

	// Payload must carry the authorization_id to void.
	var payload riskVoidAuthorizationPayload
	if err := json.Unmarshal(dispatch.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.AuthorizationID != "authz:wf-7" {
		t.Errorf("payload.authorization_id = %q, want authz:wf-7", payload.AuthorizationID)
	}
}

// TestAuthorizeRisk_ApplyCompensationResult_Voided asserts the voided result
// yields a successful compensation outcome.
func TestAuthorizeRisk_ApplyCompensationResult_Voided(t *testing.T) {
	action := &AuthorizeRisk{}
	view := compensateView("wf-7", 0, "AuthorizeRisk",
		jsonMust(t, riskAuthorizeOutput{AuthorizationID: "authz:wf-7"}))
	env := messaging.Envelope{
		MessageType: "risk.payment-authorization-voided.v1",
		Payload:     jsonMust(t, map[string]any{"authorization_id": "authz:wf-7"}),
	}

	outcome, err := action.ApplyCompensationResult(context.Background(), view, env)
	if err != nil {
		t.Fatalf("ApplyCompensationResult: %v", err)
	}
	if !outcome.Succeeded {
		t.Fatalf("outcome.Succeeded = false, want true (msg=%q)", outcome.Message)
	}
}
