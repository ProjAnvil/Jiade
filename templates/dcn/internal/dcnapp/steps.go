package dcnapp

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/shopspring/decimal"

	"dcn/internal/contracts"
)

// classifyResult 把 applyMovement 的结果映射为回执语义：
// 幂等重复按 DONE 处理；余额不足是业务 FAILED；其余视为基础设施错误（requeue 重试）。
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

// handleStep 消费一条 RMB 子事务消息。返回非 nil 表示基础设施错误（nack 重投）。
func (s *Server) handleStep(body []byte) error {
	var msg contracts.StepMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		log.Printf("bad step message, drop: %v", err)
		return nil // 毒消息：ack 丢弃
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
		return nil // 单测未注入时不发布，视为成功避免 requeue 语义干扰测试
	}
	if err := s.publishFn("", "rmb.receipts", receipt); err != nil {
		return err // 回执失败也重投（applyStep 幂等，重投安全）
	}
	return nil
}

// applyStep 在本地事务内执行一次资金变动。
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
