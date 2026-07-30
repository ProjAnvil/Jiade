// Package workflows implements the payment-transfer saga definition for the
// bank payment workflow engine.
//
// This file defines TransferContext — the IMMUTABLE artifact produced by the
// Preparation phase. The workflow engine stores it once (on
// Instance.PreparedContext) and every subsequent action reads it as read-only
// input. The context is never mutated after Preparation.
package workflows

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// TransferContext is the immutable prepared context for a payment transfer.
// Once produced by Preparation and stored on the workflow instance, it is never
// mutated by any downstream action.
//
// The PayerLedgerSnapshot and PayerAvailableSnapshot fields capture the payer's
// observed balance at Preparation time. They are OBSERVATIONS, not
// authorizations: recording the snapshot does NOT reserve funds. Funds
// authorization is the responsibility of the risk and hold actions (Tasks 1-2)
// which run later in the saga and record their own state.
type TransferContext struct {
	PaymentID              string `json:"payment_id"`
	PayerCustomerID        string `json:"payer_customer_id"`
	PayerAccountNo         string `json:"payer_account_no"`
	PayeeAccountNo         string `json:"payee_account_no"`
	Currency               string `json:"currency"`
	AmountMinor            int64  `json:"amount_minor"`
	PayerLedgerSnapshot    int64  `json:"payer_ledger_snapshot"`
	PayerAvailableSnapshot int64  `json:"payer_available_snapshot"`
	CustomerKYC            string `json:"customer_kyc"`
	ContextDigest          string `json:"context_digest"`
}

// ComputeDigest returns the SHA-256 hex digest over the canonical (sorted-key)
// JSON encoding of the TransferContext, excluding the ContextDigest field
// itself. Canonicalisation removes struct-declaration-order sensitivity so the
// digest is stable across builds; excluding the digest field avoids a
// self-referential hash. Downstream actions can recompute the digest to verify
// the context they read is exactly what Preparation produced.
func (tc TransferContext) ComputeDigest() (string, error) {
	raw, err := json.Marshal(tc)
	if err != nil {
		return "", fmt.Errorf("workflow: marshal transfer context: %w", err)
	}
	// Re-encode through a map so json.Marshal sorts keys alphabetically,
	// yielding a canonical form independent of struct field order.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", fmt.Errorf("workflow: unmarshal transfer context: %w", err)
	}
	delete(m, "context_digest")
	canonical, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("workflow: marshal canonical context: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
