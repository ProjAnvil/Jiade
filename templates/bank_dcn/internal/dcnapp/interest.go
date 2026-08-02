package dcnapp

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/shopspring/decimal"

	"bank_dcn/internal/platform/httpx"
)

var bizDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// InterestFor computes end-of-day interest for a single account: balance × daily rate,
// rounded half-even to 2 decimals (shopspring/decimal RoundBank: an exact 5 rounds
// toward even, e.g. 0.005→0.00, 0.015→0.02).
func InterestFor(balance, rate decimal.Decimal) decimal.Decimal {
	return balance.Mul(rate).RoundBank(2)
}

// interestTxID returns the journal idempotency key for interest posting (backstopped by
// uk_tx_acct, safe to re-run).
func interestTxID(bizDate string, accountID int) string {
	return fmt.Sprintf("interest-%s-%d", bizDate, accountID)
}

type interestBatchRequest struct {
	BizDate string `json:"bizDate"`
}

// handleInterestBatch performs end-of-day interest posting: it iterates the accounts of
// this unit, posting each account in its own local transaction (simulating production
// batches that commit per entry), and reports each entry to the ADM global mirror via
// publishEvent.
func (s *Server) handleInterestBatch(w http.ResponseWriter, r *http.Request) {
	var req interestBatchRequest
	if err := httpx.Decode(r, &req); err != nil || !bizDateRe.MatchString(req.BizDate) {
		httpx.Error(w, 400, "bizDate required in YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", req.BizDate); err != nil {
		httpx.Error(w, 400, "invalid bizDate")
		return
	}
	rows, err := s.db.Query(`SELECT account_id, balance FROM account ORDER BY account_id`)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	type acct struct {
		id  int
		bal string
	}
	var list []acct
	for rows.Next() {
		var a acct
		if err := rows.Scan(&a.id, &a.bal); err != nil {
			rows.Close()
			httpx.Error(w, 500, err.Error())
			return
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}

	total := decimal.Zero
	count := 0
	for _, a := range list {
		bal, err := decimal.NewFromString(a.bal)
		if err != nil {
			continue
		}
		interest := InterestFor(bal, s.rate)
		if !interest.GreaterThan(decimal.Zero) {
			continue
		}
		txID := interestTxID(req.BizDate, a.id)
		tx, err := s.db.Begin()
		if err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		moveErr := applyMovement(tx, txID, a.id, "CREDIT", interest)
		if moveErr != nil {
			tx.Rollback()
			if errors.Is(moveErr, errDuplicate) {
				continue // idempotent re-run: skip entries already posted
			}
			httpx.Error(w, 500, moveErr.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		s.publishEvent(txID, a.id, "CREDIT", interest)
		total = total.Add(interest)
		count++
	}
	httpx.JSON(w, 200, map[string]any{
		"dcn": s.dcn, "accounts": count, "totalInterest": total.String(),
	})
}
