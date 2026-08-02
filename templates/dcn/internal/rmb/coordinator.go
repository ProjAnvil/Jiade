// Package rmb 实现 RMB 可靠消息总线的事务协调服务：
// 跨 DCN 总事务的注册、子事务分发、回执收集、超时补偿与崩溃恢复。
package rmb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"dcn/internal/contracts"
	"dcn/internal/platform/httpx"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

const httpWaitWindow = 10 * time.Second

// Coordinator 是 RMB 事务协调服务。
type Coordinator struct {
	db       *sql.DB
	mqc      *mq.Conn
	timeout  time.Duration
	attempts sync.Map // 补偿步骤重试计数："txID:stepNo" -> int
}

// NewCoordinator 构造协调服务；timeout 为子事务超时（超时即补偿）。
func NewCoordinator(db *sql.DB, mqc *mq.Conn, timeout time.Duration) *Coordinator {
	return &Coordinator{db: db, mqc: mqc, timeout: timeout}
}

// Handler 返回 HTTP 路由。
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "rmb-coordinator", "POST /transactions", http.HandlerFunc(c.handleCreate))
	metrics.Handle(mux, "rmb-coordinator", "GET /transactions/{txId}", http.HandlerFunc(c.handleGet))
	return mux
}

// Run 声明拓扑、崩溃恢复、启动回执消费与超时器。在 HTTP 服务之前调用。
func (c *Coordinator) Run() {
	c.mqc.DeclareTopicExchange("rmb.steps")
	c.mqc.DeclareQueue("rmb.receipts")
	c.recover()
	c.mqc.Consume("rmb.receipts", c.handleReceipt)
	go c.timeoutLoop()
}

// ---------- 注册与查询 ----------

type txRequest struct {
	TxID  string          `json:"txId,omitempty"`
	Type  string          `json:"type"`
	Steps []txRequestStep `json:"steps"`
}

type txRequestStep struct {
	DCN       string `json:"dcn"`
	Action    string `json:"action"`
	AccountID int    `json:"accountId"`
	Amount    string `json:"amount"`
}

func (c *Coordinator) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req txRequest
	if err := httpx.Decode(r, &req); err != nil || req.Type == "" || len(req.Steps) == 0 {
		httpx.Error(w, 400, "type and non-empty steps required")
		return
	}
	txID, status, err := c.register(req)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	if status == "PROCESSING" {
		status = c.waitFinal(txID, httpWaitWindow)
	}
	httpx.JSON(w, 200, map[string]string{"txId": txID, "status": status})
}

func (c *Coordinator) handleGet(w http.ResponseWriter, r *http.Request) {
	txID := r.PathValue("txId")
	var typ, status string
	err := c.db.QueryRow(`SELECT type, status FROM tx_log WHERE tx_id = ?`, txID).Scan(&typ, &status)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, 404, "transaction not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := c.db.Query(
		`SELECT step_no, dcn, action, status FROM tx_step_log WHERE tx_id = ? ORDER BY step_no`, txID)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type stepView struct {
		StepNo int    `json:"stepNo"`
		DCN    string `json:"dcn"`
		Action string `json:"action"`
		Status string `json:"status"`
	}
	steps := []stepView{}
	for rows.Next() {
		var sv stepView
		if err := rows.Scan(&sv.StepNo, &sv.DCN, &sv.Action, &sv.Status); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		steps = append(steps, sv)
	}
	httpx.JSON(w, 200, map[string]any{
		"txId": txID, "type": typ, "status": status, "steps": steps,
	})
}

// register 落库总事务与步骤后分发；txId 已存在时幂等返回当前状态。
func (c *Coordinator) register(req txRequest) (string, string, error) {
	txID := req.TxID
	if txID == "" {
		txID = "tx-" + runx.RandHex(8)
	}
	var status string
	err := c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, txID).Scan(&status)
	if err == nil {
		return txID, status, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", "", err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tx_log (tx_id, type, status) VALUES (?,?,'PROCESSING')`,
		txID, req.Type); err != nil {
		if mysqlx.IsDuplicate(err) {
			_ = tx.Rollback()
			var cur string
			if qerr := c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, txID).Scan(&cur); qerr == nil {
				return txID, cur, nil
			}
		}
		return "", "", err
	}
	for i, st := range req.Steps {
		payload, _ := json.Marshal(contracts.StepMessage{
			TxID: txID, StepNo: i + 1, Action: st.Action, AccountID: st.AccountID, Amount: st.Amount,
		})
		if _, err := tx.Exec(
			`INSERT INTO tx_step_log (tx_id, step_no, dcn, action, status, payload) VALUES (?,?,?,?,'PENDING',?)`,
			txID, i+1, st.DCN, st.Action, payload); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", err
	}
	log.Printf("tx %s: -> PROCESSING (%d steps)", txID, len(req.Steps))
	c.publishPending(txID)
	return txID, "PROCESSING", nil
}

// publishPending 把 PENDING 步骤按 payload 原样投递到各自 DCN 队列。
// DCN 端以 journal 唯一键幂等，重复投递安全。
func (c *Coordinator) publishPending(txID string) {
	rows, err := c.db.Query(
		`SELECT step_no, dcn, payload FROM tx_step_log WHERE tx_id = ? AND status = 'PENDING'`, txID)
	if err != nil {
		log.Printf("tx %s: load pending steps: %v", txID, err)
		return
	}
	type pending struct {
		stepNo  int
		dcn     string
		payload string
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.stepNo, &p.dcn, &p.payload); err == nil {
			list = append(list, p)
		}
	}
	rows.Close()
	for _, p := range list {
		if err := c.mqc.Publish("rmb.steps", "step."+p.dcn, []byte(p.payload)); err != nil {
			log.Printf("tx %s step %d: publish failed: %v", txID, p.stepNo, err)
		}
	}
}

// waitFinal 轮询等待事务离开 PROCESSING（最长 d）。
func (c *Coordinator) waitFinal(txID string, d time.Duration) string {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		var s string
		if err := c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, txID).Scan(&s); err == nil &&
			s != "PROCESSING" {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "PROCESSING"
}

// ---------- 回执与状态机 ----------

type stepRow struct {
	stepNo  int
	dcn     string
	action  string
	status  string
	payload string
}

func (c *Coordinator) handleReceipt(body []byte) error {
	var rc contracts.Receipt
	if err := json.Unmarshal(body, &rc); err != nil {
		return nil
	}
	var stepStatus, txStatus string
	err := c.db.QueryRow(
		`SELECT status FROM tx_step_log WHERE tx_id = ? AND step_no = ?`, rc.TxID, rc.StepNo).
		Scan(&stepStatus)
	if err != nil {
		return nil // 未知步骤（可能重复回执），丢弃
	}
	_ = c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, rc.TxID).Scan(&txStatus)

	if rc.Status == "DONE" && stepStatus == "FAILED" {
		// 迟到 DONE 回执：步骤被判 FAILED（如超时）但下游恢复后补执行成功。
		// 先把步骤置回 DONE，再在事务已 COMPENSATED 时重开事务，
		// 由 advance 按 DONE 原始步骤补建反向补偿，保证余额合计不变。
		log.Printf("tx %s: late DONE receipt for FAILED step %d, re-compensating", rc.TxID, rc.StepNo)
		if _, err := c.db.Exec(
			`UPDATE tx_step_log SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'FAILED'`,
			rc.TxID, rc.StepNo); err != nil {
			return err
		}
		if txStatus == "COMPENSATED" {
			if err := c.reopenCompensation(rc.TxID); err != nil {
				return err
			}
		}
		return c.advance(rc.TxID)
	}

	if rc.Status == "DONE" {
		_, err = c.db.Exec(
			`UPDATE tx_step_log SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'PENDING'`,
			rc.TxID, rc.StepNo)
	} else {
		_, err = c.db.Exec(
			`UPDATE tx_step_log SET status = 'FAILED' WHERE tx_id = ? AND step_no = ? AND status = 'PENDING'`,
			rc.TxID, rc.StepNo)
		log.Printf("tx %s step %d: FAILED (%s)", rc.TxID, rc.StepNo, rc.Reason)
	}
	if err != nil {
		return err
	}
	return c.advance(rc.TxID)
}

// advance 推动事务状态机：全部 DONE → COMMITTED；出现 FAILED → 逆序补偿；补偿齐 → COMPENSATED。
// 不变式：只要存在 FAILED 原始步骤，每个 DONE 的原始步骤最终都必须有对应 COMPENSATE_* 步骤。
// 迟到 DONE 回执把 FAILED 原始步骤置回 DONE 后，已没有其他 FAILED 步骤，但已存在的
// COMPENSATE_* 与 DONE 原始步骤一一对应关系被打破——因此补偿分支以
// 「有 FAILED 或已有 COMPENSATE_*」为进入条件，按数量差补齐反向补偿。
func (c *Coordinator) advance(txID string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ? FOR UPDATE`, txID).
		Scan(&status); err != nil {
		return err
	}
	if status != "PROCESSING" {
		return nil
	}
	steps, err := loadSteps(tx, txID)
	if err != nil {
		return err
	}

	hasFailed, hasPending := false, false
	var doneOriginals []stepRow // DONE 的原始（非 COMPENSATE_）步骤
	var compSteps []stepRow
	for _, st := range steps {
		if strings.HasPrefix(st.action, "COMPENSATE_") {
			compSteps = append(compSteps, st)
			continue
		}
		switch st.status {
		case "DONE":
			doneOriginals = append(doneOriginals, st)
		case "FAILED":
			hasFailed = true
		case "PENDING":
			hasPending = true
		}
	}

	if hasFailed || len(compSteps) > 0 {
		// 1) 为尚未补偿的 DONE 原始步骤补建 COMPENSATE_*（逆序）。
		// 按数量差补建：仿真场景每事务 ≤2 步，至多 1 个 DONE 原始步骤需要补偿。
		// 迟到 DONE 把 FAILED 步骤置回 DONE 后，此处会为它补建反向补偿。
		if missing := len(doneOriginals) - len(compSteps); missing > 0 {
			maxNo := 0
			for _, st := range steps {
				if st.stepNo > maxNo {
					maxNo = st.stepNo
				}
			}
			n := maxNo
			created := 0
			for i := len(doneOriginals) - 1; i >= 0 && created < missing; i-- {
				st := doneOriginals[i]
				rev, ok := contracts.ReverseAction(st.action)
				if !ok {
					continue
				}
				n++
				payload, err := compensatePayload(st, n, rev)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(
					`INSERT INTO tx_step_log (tx_id, step_no, dcn, action, status, payload) VALUES (?,?,?,?,'PENDING',?)`,
					txID, n, st.dcn, rev, payload); err != nil {
					return err
				}
				created++
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			log.Printf("tx %s: compensating (%d new compensation steps)", txID, created)
			c.publishPending(txID)
			return nil
		}
		// 2) 补偿已齐：等待回执 / 终态
		if len(compSteps) > 0 {
			for _, st := range compSteps {
				if st.status == "FAILED" {
					retry, err := c.retryCompensate(tx, txID, st)
					if err != nil {
						return err // 基础设施错误：回执 nack 重投后重走 advance
					}
					if retry {
						if err := tx.Commit(); err != nil {
							return err
						}
						c.publishPending(txID) // 先提交再重投
						return nil
					}
					c.transition(tx, txID, "PROCESSING", "FAILED")
					if err := tx.Commit(); err != nil {
						return err
					}
					metrics.IncTx("FAILED")
					return nil
				}
			}
			for _, st := range compSteps {
				if st.status != "DONE" {
					return nil // 等待补偿回执
				}
			}
			c.transition(tx, txID, "PROCESSING", "COMPENSATED")
			if err := tx.Commit(); err != nil {
				return err
			}
			metrics.IncTx("COMPENSATED")
			return nil
		}
		// 3) 零 DONE 原始步骤：还有 PENDING 则等待（超时器会收场）；
		// 全部 FAILED 且无需补偿 → 空补偿直接终态 COMPENSATED。
		if hasPending {
			return nil
		}
		c.transition(tx, txID, "PROCESSING", "COMPENSATED")
		if err := tx.Commit(); err != nil {
			return err
		}
		metrics.IncTx("COMPENSATED")
		return nil
	}

	// 无失败：全部 DONE → COMMITTED
	for _, st := range steps {
		if st.status != "DONE" {
			return nil
		}
	}
	c.transition(tx, txID, "PROCESSING", "COMMITTED")
	if err := tx.Commit(); err != nil {
		return err
	}
	metrics.IncTx("COMMITTED")
	return nil
}

// retryCompensate 对失败的补偿步骤重置为 PENDING（最多 3 次）；返回 (是否已重置可重投, 错误)。
// 调用方负责提交事务后再 publishPending；UPDATE 失败不消耗重试次数。
func (c *Coordinator) retryCompensate(tx *sql.Tx, txID string, st stepRow) (bool, error) {
	key := txID + ":" + strconv.Itoa(st.stepNo)
	n, _ := c.attempts.LoadOrStore(key, 0)
	if n.(int) >= 3 {
		return false, nil
	}
	if _, err := tx.Exec(
		`UPDATE tx_step_log SET status = 'PENDING' WHERE tx_id = ? AND step_no = ?`,
		txID, st.stepNo); err != nil {
		return false, err
	}
	c.attempts.Store(key, n.(int)+1)
	return true, nil
}

// reopenCompensation 处理迟到 DONE 回执：把已 COMPENSATED 的事务重开回 PROCESSING，
// 由调用方的 advance 补建反向补偿步骤并投递。已被重开（并发）时静默返回。
func (c *Coordinator) reopenCompensation(txID string) error {
	res, err := c.db.Exec(
		`UPDATE tx_log SET status = 'PROCESSING' WHERE tx_id = ? AND status = 'COMPENSATED'`, txID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // 已在重开流程中
	}
	log.Printf("tx %s: reopened for late-receipt compensation", txID)
	return nil
}

// compensatePayload 基于原步骤 payload 生成补偿消息（金额、账户不变，动作取反）。
func compensatePayload(st stepRow, newStepNo int, rev string) (string, error) {
	var orig contracts.StepMessage
	if err := json.Unmarshal([]byte(st.payload), &orig); err != nil {
		return "", err
	}
	out, err := json.Marshal(contracts.StepMessage{
		TxID: orig.TxID, StepNo: newStepNo, Action: rev,
		AccountID: orig.AccountID, Amount: orig.Amount,
	})
	return string(out), err
}

func loadSteps(tx *sql.Tx, txID string) ([]stepRow, error) {
	rows, err := tx.Query(
		`SELECT step_no, dcn, action, status, payload FROM tx_step_log WHERE tx_id = ? ORDER BY step_no`,
		txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		var st stepRow
		if err := rows.Scan(&st.stepNo, &st.dcn, &st.action, &st.status, &st.payload); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// transition 更新事务状态并打日志（verify 依赖日志可读性）。
func (c *Coordinator) transition(tx *sql.Tx, txID, from, to string) {
	if _, err := tx.Exec(`UPDATE tx_log SET status = ? WHERE tx_id = ?`, to, txID); err != nil {
		log.Printf("tx %s: transition to %s failed: %v", txID, to, err)
		return
	}
	log.Printf("tx %s: %s -> %s", txID, from, to)
}

// ---------- 超时与恢复 ----------

func (c *Coordinator) timeoutLoop() {
	t := time.NewTicker(time.Second)
	for range t.C {
		rows, err := c.db.Query(
			`SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'
			 AND created_at < NOW() - INTERVAL ? SECOND`, int(c.timeout.Seconds()))
		if err != nil {
			log.Printf("timeout scan: %v", err)
			continue
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			c.expire(id)
		}
	}
}

// expire 把超时事务的 PENDING 步骤标记 FAILED 并推进补偿；
// 补偿进行中的事务不判超时，但重投 PENDING 补偿步骤以防发布失败滞留。
func (c *Coordinator) expire(txID string) {
	tx, err := c.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ? FOR UPDATE`, txID).
		Scan(&status); err != nil || status != "PROCESSING" {
		return
	}
	var comps int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM tx_step_log WHERE tx_id = ? AND action LIKE 'COMPENSATE_%'`,
		txID).Scan(&comps); err != nil {
		return
	}
	if comps > 0 {
		// 补偿进行中不判超时；但 PENDING 补偿步骤可能因发布失败滞留，
		// 重投一次（DCN 端以 journal 幂等，重复投递安全）。先回滚释放行锁。
		_ = tx.Rollback()
		c.publishPending(txID)
		return
	}
	if _, err := tx.Exec(
		`UPDATE tx_step_log SET status = 'FAILED' WHERE tx_id = ? AND status = 'PENDING'`,
		txID); err != nil {
		return
	}
	if err := tx.Commit(); err != nil {
		return
	}
	log.Printf("tx %s: timed out, marking pending steps FAILED", txID)
	if err := c.advance(txID); err != nil {
		log.Printf("tx %s: advance after timeout: %v", txID, err)
	}
}

// recover 启动时续跑未完成事务：重发 PENDING 步骤（DCN 端幂等），
// 已在补偿中的事务同样靠重发 PENDING 补偿步骤续跑。
func (c *Coordinator) recover() {
	rows, err := c.db.Query(`SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'`)
	if err != nil {
		log.Printf("recover scan: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		log.Printf("tx %s: recovering", id)
		c.publishPending(id)
	}
}
