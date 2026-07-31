package workflows

import (
	"context"
	"encoding/json"
	"fmt"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// fundsHoldPlacePayload is the wire format for the core.place-hold.v1 command.
// It mirrors the payload struct the core-banking consumer (Task 2) decodes; the
// consumer falls back to the envelope IdempotencyKey when the payload omits
// one, so idempotency is set on the envelope (Dispatch.IdempotencyKey).
type fundsHoldPlacePayload struct {
	AccountNo   string `json:"account_no"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// fundsHoldReleasePayload is the wire format for the core.release-hold.v1
// compensation command.
type fundsHoldReleasePayload struct {
	HoldID string `json:"hold_id"`
}

// fundsHoldPlacedResultPayload is the subset of the hold-placed result envelope
// this action needs: the hold_id, used downstream by PostLedgerTransfer and by
// this action's own release-hold compensation.
type fundsHoldPlacedResultPayload struct {
	HoldID string `json:"hold_id"`
}

// fundsHoldOutput is the forward Output recorded on the action record after a
// successful hold. PostLedgerTransfer reads it to reference the hold_id, and
// Compensate reads it to build the release command.
type fundsHoldOutput struct {
	HoldID string `json:"hold_id"`
}

// PlaceFundsHold is the second payment-transfer saga action. It dispatches a
// core.place-hold.v1 command against the payer account and waits for a held or
// hold-failed result. On compensation it dispatches core.release-hold.v1 to
// release the previously placed hold.
//
// Stateless: all per-instance state lives on the Instance/ActionRecord.
type PlaceFundsHold struct{}

// Name returns the action identifier.
func (*PlaceFundsHold) Name() string { return placeFundsHoldName }

// Execute builds the place-hold dispatch. The hold is placed against the payer
// account for the transfer amount; the consumer computes the hold's expiry from
// its own policy when the payload omits expires_at.
func (*PlaceFundsHold) Execute(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	tc, err := decodeContext(view)
	if err != nil {
		return workflow.Dispatch{}, err
	}
	wid := view.Instance.ID
	payload := fundsHoldPlacePayload{
		AccountNo:   tc.PayerAccountNo,
		AmountCents: tc.AmountMinor,
		Currency:    tc.Currency,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("place-funds-hold: marshal payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routePlaceHold,
		Payload:             body,
		AcceptedResultTypes: fundsHoldAcceptedResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      forwardKey(wid, "place-funds-hold"),
	}, nil
}

// ApplyResult classifies the result event:
//   - core.hold-placed.v1 → success; Output carries the hold_id.
//   - core.hold-failed.v1 → failure; the embedded error_class drives the
//     classification per the brief (rejected/insufficient → business_rejected;
//     broker/dependency → transient_failure; ledger invariant →
//     invariant_violation; structural → invalid_message).
func (*PlaceFundsHold) ApplyResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultHoldPlaced:
		var res fundsHoldPlacedResultPayload
		if err := json.Unmarshal(env.Payload, &res); err != nil {
			return workflow.Outcome{}, fmt.Errorf("place-funds-hold: decode placed payload: %w", err)
		}
		out, err := json.Marshal(fundsHoldOutput{HoldID: res.HoldID})
		if err != nil {
			return workflow.Outcome{}, fmt.Errorf("place-funds-hold: marshal output: %w", err)
		}
		return workflow.Outcome{Succeeded: true, Output: out}, nil
	case resultHoldFailed:
		class, msg := classifyFailure(env.Payload)
		return workflow.Outcome{Succeeded: false, Class: class, Message: msg}, nil
	default:
		return errUnexpectedMessageType(env, true), nil
	}
}

// Compensate builds the release-hold dispatch from the forward Output (the
// hold_id recorded when the hold was placed).
func (*PlaceFundsHold) Compensate(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	var out fundsHoldOutput
	if len(view.Action.Output) == 0 {
		return workflow.Dispatch{}, fmt.Errorf("place-funds-hold: no forward output to compensate")
	}
	if err := json.Unmarshal(view.Action.Output, &out); err != nil {
		return workflow.Dispatch{}, fmt.Errorf("place-funds-hold: decode forward output for release: %w", err)
	}
	body, err := json.Marshal(fundsHoldReleasePayload{HoldID: out.HoldID})
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("place-funds-hold: marshal release payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routeReleaseHold,
		Payload:             body,
		AcceptedResultTypes: fundsHoldCompensationResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      compensateKey(view.Instance.ID, "place-funds-hold"),
	}, nil
}

// ApplyCompensationResult treats a hold-released result as a successful undo.
// A hold-release-failed result is classified from its embedded error_class so
// the engine can retry transient release failures.
func (*PlaceFundsHold) ApplyCompensationResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultHoldReleased:
		return workflow.Outcome{Succeeded: true}, nil
	case resultHoldReleaseFailed:
		class, msg := classifyFailure(env.Payload)
		return workflow.Outcome{Succeeded: false, Class: class, Message: msg}, nil
	default:
		return errUnexpectedMessageType(env, false), nil
	}
}
