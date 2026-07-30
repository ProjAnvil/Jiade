// Package workflows implements the payment-transfer saga definition for the
// bank payment workflow engine.
//
// This file defines PaymentReversalDefinition — the version-1 payment-reversal
// workflow Definition. A reversal is a SEPARATE workflow instance that
// references a SUCCEEDED payment-transfer and dispatches a single
// core.reverse-transfer.v1 command to undo the original ledger posting. It
// does NOT reopen or reuse the succeeded transfer instance; instead it
// carries the original_voucher_no in its own immutable ReversalContext.
//
// Reversal lifecycle:
//  1. Operator calls POST /payments/workflows/{id}/reverse on a succeeded
//     payment. The API looks up the transfer's voucher_no from the
//     PostLedgerTransfer action Output and starts a new payment-reversal
//     workflow carrying (original_workflow_id, original_voucher_no).
//  2. Engine.Prepare runs ReversalPreparation, which validates the input and
//     returns the immutable ReversalContext.
//  3. The single ReverseTransfer action dispatches core.reverse-transfer.v1.
//  4. On core.transfer-reversed.v1 the reversal workflow reaches
//     StatusSucceeded; the payment consumer detects this and marks the
//     ORIGINAL payment_intent reversed=true.
//  5. On a terminal failure the reversal workflow goes to StatusCompensated
//     with nothing to undo (the reversal action never succeeded).
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// ReversalInput is the JSON payload the engine passes to
// PaymentReversalDefinition.Prepare. OriginalWorkflowID references the
// SUCCEEDED payment-transfer workflow; OriginalVoucherNo identifies the
// ledger posting to reverse.
type ReversalInput struct {
	OriginalWorkflowID string `json:"original_workflow_id"`
	OriginalVoucherNo  string `json:"original_voucher_no"`
	Summary            string `json:"summary,omitempty"`
}

// ReversalContext is the immutable prepared context for a payment-reversal
// workflow. It mirrors ReversalInput; the Preparation phase exists to
// validate the input before the engine commits the instance to running.
type ReversalContext struct {
	OriginalWorkflowID string `json:"original_workflow_id"`
	OriginalVoucherNo  string `json:"original_voucher_no"`
	Summary            string `json:"summary,omitempty"`
}

// ReversalPreparation is the Preparation phase for the payment-reversal
// workflow. Unlike PaymentTransferDefinition's Preparation it performs no
// remote reads — a reversal only needs the original_voucher_no and
// original_workflow_id, both supplied by the caller that observed the
// transfer's success. The Preparation's role is purely input validation so
// misconfigured reversals fail fast at Prepare time rather than mid-saga.
type ReversalPreparation struct{}

// NewReversalPreparation returns a ReversalPreparation. It takes no
// dependencies because the reversal reads no external state during Prepare.
func NewReversalPreparation() *ReversalPreparation { return &ReversalPreparation{} }

// Prepare validates the ReversalInput and returns it as the immutable
// ReversalContext JSON. It rejects an empty original_workflow_id or
// original_voucher_no — a reversal without a concrete posting to undo is a
// caller bug, not a runtime contingency.
func (p *ReversalPreparation) Prepare(_ context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in ReversalInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("reversal prepare: invalid input: %w", err)
	}
	if in.OriginalWorkflowID == "" {
		return nil, errors.New("reversal prepare: original_workflow_id is required")
	}
	if in.OriginalVoucherNo == "" {
		return nil, errors.New("reversal prepare: original_voucher_no is required")
	}
	return json.Marshal(ReversalContext{
		OriginalWorkflowID: in.OriginalWorkflowID,
		OriginalVoucherNo:  in.OriginalVoucherNo,
		Summary:            in.Summary,
	})
}

// PaymentReversalDefinition is the payment-reversal workflow definition
// version 1. It owns a single action — ReverseTransfer — that dispatches
// core.reverse-transfer.v1 to undo the original ledger posting.
//
// On a terminal forward failure the engine calls beginCompensation, which
// finds no prior succeeded action (the reversal is the only action) and
// transitions the instance straight to StatusCompensated. There is nothing
// to undo: the reversal never succeeded, so the original posting stands.
type PaymentReversalDefinition struct {
	preparation *ReversalPreparation
}

// NewPaymentReversalDefinition wires the ReversalPreparation into the
// payment-reversal workflow definition. The preparation must be non-nil; a
// nil preparation panics at wiring time so a misconfigured engine fails fast.
func NewPaymentReversalDefinition(preparation *ReversalPreparation) *PaymentReversalDefinition {
	if preparation == nil {
		panic("workflows: NewPaymentReversalDefinition requires a non-nil ReversalPreparation")
	}
	return &PaymentReversalDefinition{preparation: preparation}
}

// Type returns the workflow definition type identifier.
func (d *PaymentReversalDefinition) Type() string { return "payment-reversal" }

// Version returns the workflow definition version.
func (d *PaymentReversalDefinition) Version() int { return 1 }

// Prepare delegates to ReversalPreparation, which validates the input and
// returns the immutable ReversalContext JSON.
func (d *PaymentReversalDefinition) Prepare(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return d.preparation.Prepare(ctx, input)
}

// Actions returns the single reversal action. A fresh slice is returned per
// call because the action is stateless.
func (d *PaymentReversalDefinition) Actions() []workflow.Action {
	return []workflow.Action{&ReverseTransfer{}}
}

// reverseTransferName centralises the Action Name() value.
const reverseTransferName = "ReverseTransfer"

// Compile-time interface compliance checks.
var (
	_ workflow.Action     = (*ReverseTransfer)(nil)
	_ workflow.Definition = (*PaymentReversalDefinition)(nil)
)

// ReverseTransfer is the single payment-reversal saga action. It dispatches
// core.reverse-transfer.v1 carrying the original_voucher_no from the
// immutable ReversalContext. The downstream core-banking consumer (Task 4)
// reverses the ledger posting and emits core.transfer-reversed.v1.
//
// Stateless: all per-instance state lives on the Instance/ActionRecord.
type ReverseTransfer struct{}

// Name returns the action identifier.
func (*ReverseTransfer) Name() string { return reverseTransferName }

// Execute builds the reverse-transfer dispatch from the ReversalContext. The
// routing key reuses routeReverseTransfer (core.reverse-transfer.v1) so the
// core-banking consumer's existing CmdReverseTransfer binding handles the
// command. Accepted result types match the transfer-posted compensation
// outcomes (core.transfer-reversed.v1 / core.transfer-reverse-failed.v1).
func (*ReverseTransfer) Execute(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	if len(view.Instance.PreparedContext) == 0 {
		return workflow.Dispatch{}, fmt.Errorf("reverse-transfer: instance %q has no prepared context", view.Instance.ID)
	}
	var rc ReversalContext
	if err := json.Unmarshal(view.Instance.PreparedContext, &rc); err != nil {
		return workflow.Dispatch{}, fmt.Errorf("reverse-transfer: decode reversal context: %w", err)
	}
	payload := ledgerTransferReversePayload{OriginalVoucherNo: rc.OriginalVoucherNo}
	body, err := json.Marshal(payload)
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("reverse-transfer: marshal payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routeReverseTransfer,
		Payload:             body,
		AcceptedResultTypes: ledgerTransferCompensationResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      forwardKey(view.Instance.ID, "reverse-transfer"),
	}, nil
}

// ApplyResult classifies the result event:
//   - core.transfer-reversed.v1 → success; the reversal is complete.
//   - core.transfer-reverse-failed.v1 → failure; the embedded error_class
//     drives the classification. A terminal class triggers compensation
//     (which has nothing to undo, so the instance goes to compensated);
//     a transient class leaves the action failed for recovery.
func (*ReverseTransfer) ApplyResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultTransferReversed:
		return workflow.Outcome{Succeeded: true}, nil
	case resultTransferReverseFailed:
		class, msg := classifyFailure(env.Payload)
		return workflow.Outcome{Succeeded: false, Class: class, Message: msg}, nil
	default:
		return errUnexpectedMessageType(env, true), nil
	}
}

// Compensate is unreachable in the v1 reversal workflow: ReverseTransfer is
// the only action, so a terminal forward failure yields no prior succeeded
// action and the engine transitions the instance straight to
// StatusCompensated without calling Compensate. The method exists to satisfy
// the workflow.Action interface and returns an error defensively — if a
// future action is appended after ReverseTransfer and fails, this method
// surfaces the unexpected call rather than silently emitting an undo command
// for a reversal that was never the original posting's forward step.
func (*ReverseTransfer) Compensate(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	return workflow.Dispatch{}, fmt.Errorf("reverse-transfer: compensate is unreachable for instance %q (no forward Output to undo)", view.Instance.ID)
}

// ApplyCompensationResult is the symmetric defensive stub for the
// compensation direction. It yields UnknownOutcome so the engine retries
// the (unreachable) compensation up to CompensationMaxAttempts.
func (*ReverseTransfer) ApplyCompensationResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	return errUnexpectedMessageType(env, false), nil
}
