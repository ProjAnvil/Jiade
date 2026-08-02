package dcnapp

import (
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/shopspring/decimal"

	"dcn/internal/platform/httpx"
)

var bizDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// InterestFor 计算单账户日终利息：余额×日利率，2 位小数 half-even 取舍
// （shopspring/decimal RoundBank：恰好 5 时向偶数位取舍，如 0.0015→0.00、0.0025→0.00）。
func InterestFor(balance, rate decimal.Decimal) decimal.Decimal {
	return balance.Mul(rate).RoundBank(2)
}

// interestTxID 返回结息的 journal 幂等键（uk_tx_acct 兜底，重跑安全）。
func interestTxID(bizDate string, accountID int) string {
	return fmt.Sprintf("interest-%s-%d", bizDate, accountID)
}

type interestBatchRequest struct {
	BizDate string `json:"bizDate"`
}

// handleInterestBatch 日终结息：遍历本单元账户，逐账户独立本地事务入账
// （仿真生产批量按笔提交），每笔经 publishEvent 上报 ADM 全局镜像。
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
			if moveErr == errDuplicate {
				continue // 重跑幂等：已入账的跳过
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
