package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// riskAuthorizePaymentPayload is the wire format for the
// risk.authorize-payment.v1 command. It mirrors the payload struct the risk
// consumer (Task 1) decodes. The consumer falls back to the envelope
// IdempotencyKey when the payload omits one, so the saga sets idempotency on the
// envelope (via Dispatch.IdempotencyKey) and omits it from the payload.
type riskAuthorizePaymentPayload struct {
	AuthorizationID string `json:"authorization_id"`
	CustomerID      string `json:"customer_id"`
	AmountCents     int64  `json:"amount_cents"`
	Currency        string `json:"currency"`
}

// riskVoidAuthorizationPayload is the wire format for the
// risk.void-payment-authorization.v1 compensation command.
type riskVoidAuthorizationPayload struct {
	AuthorizationID string `json:"authorization_id"`
}

// riskAuthorizeResultPayload is the subset of the risk consumer's
// authorize-result envelope that this action needs: the authorization_id,
// echoed back so the saga can reference it when voiding.
type riskAuthorizeResultPayload struct {
	AuthorizationID string `json:"authorization_id"`
}

// riskAuthorizeOutput is the forward Output recorded on the action record after
// a successful authorization. Compensate reads it to build the void command.
// Keeping the stored output small (just the authorization_id) decouples the
// stored shape from the full result envelope's wire format.
type riskAuthorizeOutput struct {
	AuthorizationID string `json:"authorization_id"`
}

// AuthorizeRisk is the first payment-transfer saga action. It dispatches a
// risk.authorize-payment.v1 command and waits for an authorized or rejected
// result. On compensation it dispatches risk.void-payment-authorization.v1 to
// undo the authorization.
//
// The action is stateless: all per-instance state lives on the Instance and
// ActionRecord, read through the View. Multiple instances share the same action
// value safely.
type AuthorizeRisk struct{}

// Name returns the action identifier, stable across the saga definition. The
// engine stamps it onto outbound command envelopes and validates it on inbound
// result envelopes.
func (*AuthorizeRisk) Name() string { return authorizeRiskName }

// Execute builds the authorize-payment dispatch from the immutable
// TransferContext. The authorization_id is derived deterministically from the
// workflow instance id so it is stable across re-dispatches (the risk service
// deduplicates by IdempotencyKey, and the void compensation references the same
// authorization_id).
func (*AuthorizeRisk) Execute(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	tc, err := decodeContext(view)
	if err != nil {
		return workflow.Dispatch{}, err
	}
	wid := view.Instance.ID
	payload := riskAuthorizePaymentPayload{
		AuthorizationID: authorizationID(wid),
		CustomerID:      tc.PayerCustomerID,
		AmountCents:     tc.AmountMinor,
		Currency:        tc.Currency,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("authorize-risk: marshal payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routeAuthorizePayment,
		Payload:             body,
		AcceptedResultTypes: authorizeRiskAcceptedResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      forwardKey(wid, "authorize-risk"),
	}, nil
}

// ApplyResult classifies the result event:
//   - risk.payment-authorized.v1 → success; Output carries the authorization_id.
//   - risk.payment-rejected.v1   → BusinessRejected (terminal; the domain
//     refused the payment, retrying the same command cannot change the outcome).
//
// Any other message type is defensive — the engine pre-filters by
// AcceptedResultTypes — and yields InvalidMessage so the instance stays running
// rather than spuriously compensating.
func (*AuthorizeRisk) ApplyResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultRiskAuthorized:
		var res riskAuthorizeResultPayload
		if err := json.Unmarshal(env.Payload, &res); err != nil {
			return workflow.Outcome{}, fmt.Errorf("authorize-risk: decode authorized payload: %w", err)
		}
		out, err := json.Marshal(riskAuthorizeOutput{AuthorizationID: res.AuthorizationID})
		if err != nil {
			return workflow.Outcome{}, fmt.Errorf("authorize-risk: marshal output: %w", err)
		}
		return workflow.Outcome{Succeeded: true, Output: out}, nil
	case resultRiskRejected:
		return workflow.Outcome{
			Succeeded: false,
			Class:     workflow.BusinessRejected,
			Message:   "risk authorization rejected",
		}, nil
	default:
		return errUnexpectedMessageType(env, true), nil
	}
}

// Compensate builds the void-authorization dispatch from the forward Output
// (the authorization_id recorded when the forward pass succeeded). The
// compensation idempotency key is stable across compensation retries of this
// action so the risk service deduplicates a redelivered void.
func (*AuthorizeRisk) Compensate(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	var out riskAuthorizeOutput
	if len(view.Action.Output) == 0 {
		return workflow.Dispatch{}, fmt.Errorf("authorize-risk: no forward output to compensate")
	}
	if err := json.Unmarshal(view.Action.Output, &out); err != nil {
		return workflow.Dispatch{}, fmt.Errorf("authorize-risk: decode forward output for void: %w", err)
	}
	body, err := json.Marshal(riskVoidAuthorizationPayload{AuthorizationID: out.AuthorizationID})
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("authorize-risk: marshal void payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routeVoidAuthorization,
		Payload:             body,
		AcceptedResultTypes: authorizeRiskCompensationResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      compensateKey(view.Instance.ID, "authorize-risk"),
	}, nil
}

// ApplyCompensationResult treats a voided result as a successful undo. Any
// other message type is defensive and yields UnknownOutcome so the engine
// retries the compensation (up to CompensationMaxAttempts).
func (*AuthorizeRisk) ApplyCompensationResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultRiskVoided:
		return workflow.Outcome{Succeeded: true}, nil
	default:
		return errUnexpectedMessageType(env, false), nil
	}
}

// authorizationID derives a deterministic risk authorization identifier from the
// workflow instance id. It is stable across re-dispatches, so:
//   - The risk service deduplicates a redelivered authorize command by
//     IdempotencyKey and returns the same authorization record.
//   - The compensation step can reference the same authorization_id when
//     voiding, because it reads the id from the forward Output (not by
//     re-deriving), and the forward Output captured this same value.
func authorizationID(workflowID string) string {
	return "authz:" + workflowID
}

// forwardKey builds the semantic forward idempotency key for an action. It is
// stable across re-dispatches of the same forward action so downstream
// consumers deduplicate redelivered commands.
func forwardKey(workflowID, action string) string {
	return strings.Join([]string{"wf", workflowID, action}, ":")
}

// compensateKey builds the semantic compensation idempotency key for an action.
// It is stable across compensation retries of the same action (distinct from
// the forward key) so undo commands are deduplicated independently.
func compensateKey(workflowID, action string) string {
	return strings.Join([]string{"wf", workflowID, "compensate", action}, ":")
}
