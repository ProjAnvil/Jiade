// Package workflows implements the payment-transfer saga definition for the
// bank payment workflow engine.
//
// This file holds the tests for the payment-reversal workflow (Task 8):
//   - The reversal definition registers alongside payment-transfer (both
//     versions coexist in one Registry).
//   - Reversal creates a NEW workflow instance — it does NOT reopen or reuse
//     the succeeded payment-transfer instance.
//   - The reversal action dispatches core.reverse-transfer.v1 with the
//     original_voucher_no, proving the money-back path fires.
//   - A succeeded transfer instance remains succeeded after a reversal is
//     started (isolation between the two workflow lifecycles).
package workflows

import (
	"context"
	"encoding/json"
	"testing"

	"bank/internal/platform/workflow"
)

// TestPaymentReversalDefinition_Metadata asserts the reversal definition
// registers under the expected type+version and exposes exactly one action.
func TestPaymentReversalDefinition_Metadata(t *testing.T) {
	def := NewPaymentReversalDefinition(NewReversalPreparation())

	if def.Type() != "payment-reversal" {
		t.Errorf("Type = %q, want %q", def.Type(), "payment-reversal")
	}
	if def.Version() != 1 {
		t.Errorf("Version = %d, want 1", def.Version())
	}
	actions := def.Actions()
	if len(actions) != 1 {
		t.Fatalf("Actions: got %d, want 1", len(actions))
	}
	if got := actions[0].Name(); got != "ReverseTransfer" {
		t.Errorf("Actions[0].Name = %q, want %q", got, "ReverseTransfer")
	}
}

// TestPaymentReversalDefinition_CoexistsWithTransfer asserts both the
// payment-transfer and payment-reversal definitions register in the same
// Registry without colliding.
func TestPaymentReversalDefinition_CoexistsWithTransfer(t *testing.T) {
	registry := workflow.NewRegistry()
	if err := registry.Register(NewPaymentTransferDefinition(NewPreparation(validCustomerReader(), validAccountReader()))); err != nil {
		t.Fatalf("register payment-transfer: %v", err)
	}
	if err := registry.Register(NewPaymentReversalDefinition(NewReversalPreparation())); err != nil {
		t.Fatalf("register payment-reversal: %v", err)
	}
	if _, ok := registry.Get("payment-transfer", 1); !ok {
		t.Error("payment-transfer v1 not found after registering reversal")
	}
	if _, ok := registry.Get("payment-reversal", 1); !ok {
		t.Error("payment-reversal v1 not found")
	}
}

// TestReversal_CreatesNewWorkflow_DispatchesReverseTransfer is the central
// reversal test. It drives a payment-transfer workflow all the way to
// StatusSucceeded, then starts a SEPARATE payment-reversal workflow and
// asserts:
//  1. The reversal dispatches core.reverse-transfer.v1 with the original
//     voucher_no from the succeeded transfer.
//  2. The original transfer instance remains StatusSucceeded — the reversal
//     is a brand-new workflow, not a reopen of the transfer.
//  3. The reversal instance has a distinct ID from the transfer instance.
func TestReversal_CreatesNewWorkflow_DispatchesReverseTransfer(t *testing.T) {
	store := newMemStore()
	registry := workflow.NewRegistry()
	if err := registry.Register(NewPaymentTransferDefinition(NewPreparation(validCustomerReader(), validAccountReader()))); err != nil {
		t.Fatalf("register payment-transfer: %v", err)
	}
	if err := registry.Register(NewPaymentReversalDefinition(NewReversalPreparation())); err != nil {
		t.Fatalf("register payment-reversal: %v", err)
	}
	engine := workflow.NewEngine(store, registry, workflow.EngineConfig{})
	ctx := context.Background()

	const transferWID = "wf-transfer-success"
	driveTransferToSuccess(t, ctx, engine, store, transferWID)

	// Capture the voucher_no from the transfer's final action output — the
	// reversal must reference it.
	store.mu.Lock()
	transferVoucher := ""
	for _, a := range store.instances[transferWID].Actions {
		if a.Name == postLedgerTransferName && a.Status == workflow.ActionSucceeded {
			var out ledgerTransferOutput
			_ = json.Unmarshal(a.Output, &out)
			transferVoucher = out.VoucherNo
		}
	}
	store.mu.Unlock()
	if transferVoucher == "" {
		t.Fatalf("no voucher_no captured from transfer %s", transferWID)
	}

	// Start the reversal as a NEW workflow instance.
	const reversalWID = "wf-reversal-1"
	reversalInput := ReversalInput{
		OriginalWorkflowID: transferWID,
		OriginalVoucherNo:  transferVoucher,
	}
	inputBytes, _ := json.Marshal(reversalInput)
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID:    reversalWID,
		Type:          "payment-reversal",
		Version:       1,
		Input:         inputBytes,
		CorrelationID: reversalWID,
	}); err != nil {
		t.Fatalf("reversal Start: %v", err)
	}
	if err := engine.Prepare(ctx, reversalWID); err != nil {
		t.Fatalf("reversal Prepare: %v", err)
	}

	// 1. The reversal dispatched core.reverse-transfer.v1.
	cmd := store.lastOutbox()
	if cmd.routingKey != routeReverseTransfer {
		t.Errorf("reversal routing key = %q, want %q", cmd.routingKey, routeReverseTransfer)
	}
	if cmd.env.ActionName != "ReverseTransfer" {
		t.Errorf("reversal action name = %q, want ReverseTransfer", cmd.env.ActionName)
	}
	var payload ledgerTransferReversePayload
	if err := json.Unmarshal(cmd.env.Payload, &payload); err != nil {
		t.Fatalf("decode reverse payload: %v", err)
	}
	if payload.OriginalVoucherNo != transferVoucher {
		t.Errorf("payload original_voucher_no = %q, want %q", payload.OriginalVoucherNo, transferVoucher)
	}

	// 2. The original transfer instance remains succeeded — the reversal did
	// NOT reopen it.
	store.mu.Lock()
	transferInst := *store.instances[transferWID]
	reversalInst := *store.instances[reversalWID]
	store.mu.Unlock()
	if transferInst.Status != workflow.StatusSucceeded {
		t.Errorf("transfer status = %q, want %q (reversal must not reopen the transfer)",
			transferInst.Status, workflow.StatusSucceeded)
	}
	// 3. Distinct instance IDs.
	if reversalInst.ID == transferInst.ID {
		t.Fatalf("reversal instance ID %q == transfer instance ID (must be separate workflows)", reversalInst.ID)
	}
	if reversalInst.Type != "payment-reversal" {
		t.Errorf("reversal type = %q, want payment-reversal", reversalInst.Type)
	}
}

// TestReversal_AcceptsTransferReversedResult proves the reversal action
// classifies core.transfer-reversed.v1 as success and transitions the
// reversal workflow to StatusSucceeded.
func TestReversal_AcceptsTransferReversedResult(t *testing.T) {
	store := newMemStore()
	registry := workflow.NewRegistry()
	if err := registry.Register(NewPaymentReversalDefinition(NewReversalPreparation())); err != nil {
		t.Fatalf("register: %v", err)
	}
	engine := workflow.NewEngine(store, registry, workflow.EngineConfig{})
	ctx := context.Background()

	const reversalWID = "wf-rev-result"
	inputBytes, _ := json.Marshal(ReversalInput{
		OriginalWorkflowID: "wf-orig-1",
		OriginalVoucherNo:  "V-1",
	})
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: reversalWID, Type: "payment-reversal", Version: 1,
		Input: inputBytes, CorrelationID: reversalWID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, reversalWID); err != nil {
		t.Fatal(err)
	}
	// Drive the result: transfer-reversed → reversal succeeds.
	cmd := store.lastOutbox()
	reversedEnv := resultFromCommand(t, cmd.env, resultTransferReversed, jsonMust(t, map[string]any{
		"voucher_no": "V-REVERSED",
	}))
	if err := engine.ApplyResult(ctx, reversedEnv); err != nil {
		t.Fatalf("ApplyResult reversed: %v", err)
	}
	store.mu.Lock()
	inst := *store.instances[reversalWID]
	store.mu.Unlock()
	if inst.Status != workflow.StatusSucceeded {
		t.Errorf("reversal status = %q, want %q", inst.Status, workflow.StatusSucceeded)
	}
}

// TestReversal_RejectsEmptyVoucher asserts the reversal Preparation rejects
// an input missing the voucher_no — the reversal must have a concrete posting
// to reverse.
func TestReversal_RejectsEmptyVoucher(t *testing.T) {
	prep := NewReversalPreparation()
	_, err := prep.Prepare(context.Background(), jsonMust(t, ReversalInput{OriginalWorkflowID: "wf-1"}))
	if err == nil {
		t.Fatal("Prepare with empty voucher: expected error, got nil")
	}
}

// driveTransferToSuccess advances a payment-transfer workflow from Start all
// the way to StatusSucceeded by feeding each action a successful result. It
// is the shared setup for reversal tests that need a succeeded transfer.
func driveTransferToSuccess(t *testing.T, ctx context.Context, engine *workflow.Engine, store *memStore, wid string) {
	t.Helper()
	input := validInput()
	inputBytes, _ := json.Marshal(input)
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wid, Type: "payment-transfer", Version: 1,
		Input: inputBytes, CorrelationID: wid,
	}); err != nil {
		t.Fatalf("transfer Start: %v", err)
	}
	if err := engine.Prepare(ctx, wid); err != nil {
		t.Fatalf("transfer Prepare: %v", err)
	}

	// 1. AuthorizeRisk → authorized.
	authCmd := store.lastOutbox()
	authorizedEnv := resultFromCommand(t, authCmd.env, resultRiskAuthorized,
		jsonMust(t, map[string]any{"authorization_id": "authz:" + wid, "customer_id": "C-100"}))
	if err := engine.ApplyResult(ctx, authorizedEnv); err != nil {
		t.Fatalf("ApplyResult authorized: %v", err)
	}

	// 2. PlaceFundsHold → held.
	holdCmd := store.lastOutbox()
	heldEnv := resultFromCommand(t, holdCmd.env, resultHoldPlaced,
		jsonMust(t, map[string]any{"hold_id": "H-1", "account_no": "ACC-PAYER"}))
	if err := engine.ApplyResult(ctx, heldEnv); err != nil {
		t.Fatalf("ApplyResult held: %v", err)
	}

	// 3. PostLedgerTransfer → posted (terminal success of the forward saga).
	transferCmd := store.lastOutbox()
	postedEnv := resultFromCommand(t, transferCmd.env, resultTransferPosted,
		jsonMust(t, map[string]any{"voucher_no": "V-" + wid}))
	if err := engine.ApplyResult(ctx, postedEnv); err != nil {
		t.Fatalf("ApplyResult posted: %v", err)
	}

	store.mu.Lock()
	inst := *store.instances[wid]
	store.mu.Unlock()
	if inst.Status != workflow.StatusSucceeded {
		t.Fatalf("transfer setup: status = %q, want %q", inst.Status, workflow.StatusSucceeded)
	}
}
