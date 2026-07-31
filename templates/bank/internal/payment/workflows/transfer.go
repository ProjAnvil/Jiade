// Package workflows implements the payment-transfer saga definition for the
// bank payment workflow engine.
//
// This file defines PaymentTransferDefinition — the version-1 payment-transfer
// workflow Definition. It chains the Task-5 Preparation with three ordered
// actions — AuthorizeRisk, PlaceFundsHold, PostLedgerTransfer — and centralises
// the dispatch routing keys, accepted result types, the 15-second action
// deadline, and the shared failure-payload classifier consumed by all three
// actions.
package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// actionDispatchDeadline bounds how long an action waits for a result event
// before the engine's recovery loop re-dispatches. The saga brief fixes this at
// 15 seconds: long enough for a synchronous downstream decision (risk policy,
// hold placement, ledger posting), short enough that a stuck consumer is
// re-dispatched promptly rather than blocking the saga.
const actionDispatchDeadline = 15 * time.Second

// Forward command routing keys. These MUST match the message types the
// downstream consumers (Tasks 1-4) are subscribed to.
const (
	routeAuthorizePayment  = "risk.authorize-payment.v1"
	routePlaceHold         = "core.place-hold.v1"
	routePostHeldTransfer  = "core.post-held-transfer.v1"
)

// Compensation command routing keys. These MUST match the undo message types
// the downstream consumers accept.
const (
	routeVoidAuthorization = "risk.void-payment-authorization.v1"
	routeReleaseHold       = "core.release-hold.v1"
	routeReverseTransfer   = "core.reverse-transfer.v1"
)

// Accepted forward result event types per action. The engine rejects any result
// envelope whose message_type is not in the current action's AcceptedResultTypes
// (ErrInvalidMessage), so only these events reach ApplyResult.
var (
	authorizeRiskAcceptedResults = []string{
		"risk.payment-authorized.v1",
		"risk.payment-rejected.v1",
	}
	fundsHoldAcceptedResults = []string{
		"core.hold-placed.v1",
		"core.hold-failed.v1",
	}
	ledgerTransferAcceptedResults = []string{
		"core.transfer-posted.v1",
		"core.transfer-failed.v1",
	}
)

// Accepted compensation result event types per action.
var (
	authorizeRiskCompensationResults = []string{"risk.payment-authorization-voided.v1"}
	fundsHoldCompensationResults     = []string{"core.hold-released.v1", "core.hold-release-failed.v1"}
	ledgerTransferCompensationResults = []string{
		"core.transfer-reversed.v1",
		"core.transfer-reverse-failed.v1",
	}
)

// failurePayload mirrors the failure wire format the core-banking consumer
// (Task 4) emits for terminal and structural failures. The consumer classifies
// the service error and stamps the workflow engine ErrorClass into error_class
// so the saga's ApplyResult does not have to re-derive it.
type failurePayload struct {
	ErrorClass   string `json:"error_class"`
	ErrorMessage string `json:"error_message,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
}

// classifyFailure decodes a failure event payload and maps its error_class to
// the workflow engine's ErrorClass per the brief's classification rules:
//
//   - business_rejected  → rejected / insufficient-funds (terminal, compensates)
//   - transient_failure  → broker / dependency down (retryable, left running)
//   - invariant_violation → ledger balance / hold-state error (terminal)
//   - invalid_message    → structural / unknown command (terminal)
//   - empty/unrecognised → unknown_outcome (left running for recovery)
//
// A payload that cannot be decoded yields unknown_outcome so a malformed event
// does not spuriously trigger compensation.
func classifyFailure(payload json.RawMessage) (workflow.ErrorClass, string) {
	var fp failurePayload
	if err := json.Unmarshal(payload, &fp); err != nil {
		return workflow.UnknownOutcome, fmt.Sprintf("decode failure payload: %v", err)
	}
	switch workflow.ErrorClass(fp.ErrorClass) {
	case workflow.BusinessRejected:
		return workflow.BusinessRejected, fp.ErrorMessage
	case workflow.TransientFailure:
		return workflow.TransientFailure, fp.ErrorMessage
	case workflow.InvariantViolation:
		return workflow.InvariantViolation, fp.ErrorMessage
	case workflow.InvalidMessage:
		return workflow.InvalidMessage, fp.ErrorMessage
	default:
		return workflow.UnknownOutcome, fp.ErrorMessage
	}
}

// decodeContext unmarshals the immutable PreparedContext from a View into a
// TransferContext. Every action reads the same context produced by Preparation;
// none ever mutates it.
func decodeContext(view workflow.View) (TransferContext, error) {
	var tc TransferContext
	if len(view.Instance.PreparedContext) == 0 {
		return TransferContext{}, fmt.Errorf("instance %q has no prepared context", view.Instance.ID)
	}
	if err := json.Unmarshal(view.Instance.PreparedContext, &tc); err != nil {
		return TransferContext{}, fmt.Errorf("decode transfer context: %w", err)
	}
	return tc, nil
}

// PaymentTransferDefinition is the payment-transfer saga workflow definition
// version 1. It chains the Task-5 Preparation (immutable TransferContext) with
// three ordered actions:
//
//  1. AuthorizeRisk      → risk.authorize-payment.v1
//  2. PlaceFundsHold     → core.place-hold.v1
//  3. PostLedgerTransfer → core.post-held-transfer.v1
//
// On a terminal forward failure the engine compensates in REVERSE order
// (PostLedgerTransfer → PlaceFundsHold → AuthorizeRisk), emitting one undo
// command per succeeded action. Financial steps are never auto-skipped: each
// succeeded action is undone by its own Compensate dispatch.
type PaymentTransferDefinition struct {
	preparation *Preparation
}

// NewPaymentTransferDefinition wires the Preparation (Task 5) into the
// payment-transfer workflow definition. The preparation must be non-nil; a nil
// preparation panics at wiring time so a misconfigured engine fails fast.
func NewPaymentTransferDefinition(preparation *Preparation) *PaymentTransferDefinition {
	if preparation == nil {
		panic("workflows: NewPaymentTransferDefinition requires a non-nil Preparation")
	}
	return &PaymentTransferDefinition{preparation: preparation}
}

// Type returns the workflow definition type identifier.
func (d *PaymentTransferDefinition) Type() string { return "payment-transfer" }

// Version returns the workflow definition version.
func (d *PaymentTransferDefinition) Version() int { return 1 }

// Prepare delegates to the Task-5 Preparation, which reads customer and account
// snapshots, validates them, and returns the immutable TransferContext JSON.
func (d *PaymentTransferDefinition) Prepare(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	return d.preparation.Prepare(ctx, input)
}

// Actions returns the three ordered saga actions. A fresh slice is returned per
// call because the actions are stateless (all state lives on the Instance and
// ActionRecord, read through the View).
func (d *PaymentTransferDefinition) Actions() []workflow.Action {
	return []workflow.Action{
		&AuthorizeRisk{},
		&PlaceFundsHold{},
		&PostLedgerTransfer{},
	}
}

// authorizeRiskName / placeFundsHoldName / postLedgerTransferName centralise
// the Action Name() values so prior-action lookups (e.g. PostLedgerTransfer
// finding the hold_id) match by Name rather than fragile positional index.
const (
	authorizeRiskName       = "AuthorizeRisk"
	placeFundsHoldName      = "PlaceFundsHold"
	postLedgerTransferName  = "PostLedgerTransfer"
)

// priorActionOutput returns the forward Output of the most-recent succeeded
// action with the given Name, or an error if none is found. It lets a downstream
// action read an upstream action's Output by semantic name rather than by a
// hard-coded index that would break if the action order changed.
func priorActionOutput(actions []workflow.ActionRecord, name string) (json.RawMessage, error) {
	for _, a := range actions {
		if a.Name == name && a.Status == workflow.ActionSucceeded {
			return a.Output, nil
		}
	}
	return nil, fmt.Errorf("prior action %q has no succeeded output", name)
}

// Compile-time interface compliance checks. These guarantee the action types
// satisfy workflow.Action at build time; a signature drift becomes a compile
// error rather than a runtime failure during engine dispatch.
var (
	_ workflow.Action = (*AuthorizeRisk)(nil)
	_ workflow.Action = (*PlaceFundsHold)(nil)
	_ workflow.Action = (*PostLedgerTransfer)(nil)
	_ workflow.Definition = (*PaymentTransferDefinition)(nil)
)

// messageType constants for the result events these actions accept. Centralised
// so the AcceptedResultTypes slices and the ApplyResult switch arms share one
// source of truth.
const (
	resultRiskAuthorized       = "risk.payment-authorized.v1"
	resultRiskRejected         = "risk.payment-rejected.v1"
	resultRiskVoided           = "risk.payment-authorization-voided.v1"
	resultHoldPlaced           = "core.hold-placed.v1"
	resultHoldFailed           = "core.hold-failed.v1"
	resultHoldReleased         = "core.hold-released.v1"
	resultHoldReleaseFailed    = "core.hold-release-failed.v1"
	resultTransferPosted       = "core.transfer-posted.v1"
	resultTransferFailed       = "core.transfer-failed.v1"
	resultTransferReversed     = "core.transfer-reversed.v1"
	resultTransferReverseFailed = "core.transfer-reverse-failed.v1"
)

// errUnexpectedMessageType builds an Outcome for an event the engine should
// have filtered out. It returns InvalidMessage for the forward direction (the
// instance stays running for recovery; it never triggers compensation on an
// unrecognised event) and UnknownOutcome for the compensation direction (the
// engine retries the compensation on UnknownOutcome up to CompensationMaxAttempts).
func errUnexpectedMessageType(env messaging.Envelope, forward bool) workflow.Outcome {
	if forward {
		return workflow.Outcome{
			Succeeded: false,
			Class:     workflow.InvalidMessage,
			Message:   fmt.Sprintf("unexpected result message type %q", env.MessageType),
		}
	}
	return workflow.Outcome{
		Succeeded: false,
		Class:     workflow.UnknownOutcome,
		Message:   fmt.Sprintf("unexpected compensation result message type %q", env.MessageType),
	}
}
