package workflows

import (
	"context"
	"encoding/json"
	"fmt"

	"bank/internal/platform/messaging"
	"bank/internal/platform/workflow"
)

// ledgerTransferPostPayload is the wire format for the
// core.post-held-transfer.v1 command. The hold_id references the hold placed by
// the PlaceFundsHold action; the consumer converts the held amount into a
// two-legged ledger posting.
type ledgerTransferPostPayload struct {
	HoldID      string `json:"hold_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// ledgerTransferReversePayload is the wire format for the
// core.reverse-transfer.v1 compensation command. The original_voucher_no
// identifies the posting to reverse.
type ledgerTransferReversePayload struct {
	OriginalVoucherNo string `json:"original_voucher_no"`
}

// ledgerTransferPostedResultPayload is the subset of the transfer-posted result
// envelope this action needs: the voucher_no, used by the reverse-transfer
// compensation.
type ledgerTransferPostedResultPayload struct {
	VoucherNo string `json:"voucher_no"`
}

// ledgerTransferOutput is the forward Output recorded on the action record after
// a successful posting. Compensate reads it to build the reverse command.
type ledgerTransferOutput struct {
	VoucherNo string `json:"voucher_no"`
}

// PostLedgerTransfer is the third (final) payment-transfer saga action. It
// dispatches a core.post-held-transfer.v1 command that consumes the hold placed
// by PlaceFundsHold and posts the two-legged ledger transfer. On compensation
// it dispatches core.reverse-transfer.v1 to reverse the posting.
//
// Because it is the LAST forward action, a terminal failure here triggers
// compensation of BOTH prior actions (PlaceFundsHold then AuthorizeRisk) in
// reverse order, plus its own reverse if it had succeeded before a later step
// failed.
//
// Stateless: all per-instance state lives on the Instance/ActionRecord.
type PostLedgerTransfer struct{}

// Name returns the action identifier.
func (*PostLedgerTransfer) Name() string { return postLedgerTransferName }

// Execute builds the post-held-transfer dispatch. The hold_id is read from the
// prior PlaceFundsHold action's Output (not from the TransferContext, which
// predates the hold). Reading by semantic Name — not positional index — keeps
// the dependency robust to action reordering.
func (*PostLedgerTransfer) Execute(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	tc, err := decodeContext(view)
	if err != nil {
		return workflow.Dispatch{}, err
	}
	holdOutput, err := priorActionOutput(view.Instance.Actions, placeFundsHoldName)
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: %w", err)
	}
	var hold fundsHoldOutput
	if err := json.Unmarshal(holdOutput, &hold); err != nil {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: decode prior hold output: %w", err)
	}
	wid := view.Instance.ID
	payload := ledgerTransferPostPayload{
		HoldID:      hold.HoldID,
		FromAccount: tc.PayerAccountNo,
		ToAccount:   tc.PayeeAccountNo,
		AmountCents: tc.AmountMinor,
		Currency:    tc.Currency,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: marshal payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routePostHeldTransfer,
		Payload:             body,
		AcceptedResultTypes: ledgerTransferAcceptedResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      forwardKey(wid, "post-ledger-transfer"),
	}, nil
}

// ApplyResult classifies the result event:
//   - core.transfer-posted.v1 → success; Output carries the voucher_no. This is
//     the terminal success of the forward saga — the engine marks the instance
//     StatusSucceeded after the last action.
//   - core.transfer-failed.v1 → failure; the embedded error_class drives the
//     classification per the brief.
func (*PostLedgerTransfer) ApplyResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultTransferPosted:
		var res ledgerTransferPostedResultPayload
		if err := json.Unmarshal(env.Payload, &res); err != nil {
			return workflow.Outcome{}, fmt.Errorf("post-ledger-transfer: decode posted payload: %w", err)
		}
		out, err := json.Marshal(ledgerTransferOutput{VoucherNo: res.VoucherNo})
		if err != nil {
			return workflow.Outcome{}, fmt.Errorf("post-ledger-transfer: marshal output: %w", err)
		}
		return workflow.Outcome{Succeeded: true, Output: out}, nil
	case resultTransferFailed:
		class, msg := classifyFailure(env.Payload)
		return workflow.Outcome{Succeeded: false, Class: class, Message: msg}, nil
	default:
		return errUnexpectedMessageType(env, true), nil
	}
}

// Compensate builds the reverse-transfer dispatch from the forward Output (the
// voucher_no recorded when the posting succeeded). This action's own
// compensation runs BEFORE the prior actions' compensations only when a LATER
// step failed after the posting succeeded; within the v1 saga PostLedgerTransfer
// is last, so its compensation is dispatched first only if a future action is
// appended after it.
func (*PostLedgerTransfer) Compensate(_ context.Context, view workflow.View) (workflow.Dispatch, error) {
	var out ledgerTransferOutput
	if len(view.Action.Output) == 0 {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: no forward output to compensate")
	}
	if err := json.Unmarshal(view.Action.Output, &out); err != nil {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: decode forward output for reverse: %w", err)
	}
	body, err := json.Marshal(ledgerTransferReversePayload{OriginalVoucherNo: out.VoucherNo})
	if err != nil {
		return workflow.Dispatch{}, fmt.Errorf("post-ledger-transfer: marshal reverse payload: %w", err)
	}
	return workflow.Dispatch{
		RoutingKey:          routeReverseTransfer,
		Payload:             body,
		AcceptedResultTypes: ledgerTransferCompensationResults,
		Deadline:            actionDispatchDeadline,
		IdempotencyKey:      compensateKey(view.Instance.ID, "post-ledger-transfer"),
	}, nil
}

// ApplyCompensationResult treats a transfer-reversed result as a successful
// undo. A transfer-reverse-failed result is classified from its embedded
// error_class so transient reverse failures are retried.
func (*PostLedgerTransfer) ApplyCompensationResult(_ context.Context, _ workflow.View, env messaging.Envelope) (workflow.Outcome, error) {
	switch env.MessageType {
	case resultTransferReversed:
		return workflow.Outcome{Succeeded: true}, nil
	case resultTransferReverseFailed:
		class, msg := classifyFailure(env.Payload)
		return workflow.Outcome{Succeeded: false, Class: class, Message: msg}, nil
	default:
		return errUnexpectedMessageType(env, false), nil
	}
}
