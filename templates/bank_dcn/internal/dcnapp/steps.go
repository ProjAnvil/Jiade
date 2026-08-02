package dcnapp

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/shopspring/decimal"

	"bank_dcn/internal/contracts"
)

// classifyResult maps the result of applyMovement to receipt semantics:
// idempotent duplicates are treated as DONE; insufficient funds is a business FAILED;
// anything else is treated as an infrastructure error (retried via requeue).
func classifyResult(err error) (status, reason string, infraError bool) {
	switch {
	case err == nil:
		return "DONE", "", false
	case errors.Is(err, errDuplicate):
		return "DONE", "duplicate ignored", false
	case errors.Is(err, errInsufficient):
		return "FAILED", "insufficient funds", false
	case errors.Is(err, errNotFound):
		return "FAILED", "account not found", false
	default:
		return "", "", true
	}
}

// handleStep consumes one RMB sub-transaction message. Returning non-nil indicates an
// infrastructure error (nack + redelivery).
func (s *Server) handleStep(body []byte) error {
	var msg contracts.StepMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("bad step message, drop: %v", err)
		return nil // poison message: ack and drop
	}
	suffix, dir, ok := contracts.StepDirection(msg.Action)
	if !ok {
		log.Printf("unknown action %q, drop", msg.Action)
		return nil
	}
	amt, err := decimal.NewFromString(msg.Amount)
	if err != nil {
		log.Printf("bad amount %q, drop", msg.Amount)
		return nil
	}
	status, reason, infraErr := s.applyStep(msg.TxID+suffix, msg.AccountID, dir, amt)
	if infraErr != nil {
		return infraErr // nack + requeue
	}
	receipt, _ := json.Marshal(contracts.Receipt{
		TxID: msg.TxID, StepNo: msg.StepNo, DCN: s.dcn, Status: status, Reason: reason,
	})
	if s.publishFn == nil {
		return nil // not injected in unit tests: treat as success so requeue semantics don't disturb the tests
	}
	if err := s.publishFn("", "rmb.receipts", receipt); err != nil {
		return err // receipt failures are redelivered too (applyStep is idempotent, redelivery-safe)
	}
	return nil
}

// applyStep executes one fund movement inside a local transaction.
func (s *Server) applyStep(journalTxID string, accountID int, dir string, amt decimal.Decimal) (string, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	moveErr := applyMovement(tx, journalTxID, accountID, dir, amt)
	status, reason, isInfra := classifyResult(moveErr)
	if isInfra {
		return "", "", moveErr
	}
	if status == "DONE" && reason == "" {
		if err := tx.Commit(); err != nil {
			return "", "", err
		}
		s.publishEvent(journalTxID, accountID, dir, amt)
	}
	return status, reason, nil
}
