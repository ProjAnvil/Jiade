// Package rmb implements the transaction coordination service of the RMB
// reliable message bus: registration of cross-DCN global transactions,
// step dispatch, receipt collection, timeout compensation, and crash recovery.
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

	"bank_dcn/internal/contracts"
	"bank_dcn/internal/platform/httpx"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mq"
	"bank_dcn/internal/platform/mysqlx"
	"bank_dcn/internal/platform/runx"
)

const httpWaitWindow = 10 * time.Second

// Coordinator is the RMB transaction coordination service.
type Coordinator struct {
	db        *sql.DB
	mqc       *mq.Conn
	timeout   time.Duration
	attempts  sync.Map                                      // compensation step retry count: "txID:stepNo" -> int
	publishFn func(exchange, key string, body []byte) error // injectable; test double in unit tests
}

// NewCoordinator builds the coordination service; timeout is the step timeout (a timeout triggers compensation).
func NewCoordinator(db *sql.DB, mqc *mq.Conn, timeout time.Duration) *Coordinator {
	coord := &Coordinator{db: db, mqc: mqc, timeout: timeout}
	coord.publishFn = mqc.Publish
	return coord
}

// Handler returns the HTTP routes.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "rmb-coordinator", "POST /transactions", http.HandlerFunc(c.handleCreate))
	metrics.Handle(mux, "rmb-coordinator", "GET /transactions/{txId}", http.HandlerFunc(c.handleGet))
	return mux
}

// Run declares the topology, recovers after crashes, and starts receipt consumption and the timeout loop. Call before the HTTP server.
func (c *Coordinator) Run() {
	c.mqc.DeclareTopicExchange("rmb.steps")
	c.mqc.DeclareQueue("rmb.receipts")
	c.recover()
	c.mqc.Consume("rmb.receipts", c.handleReceipt)
	go c.timeoutLoop()
}

// ---------- Registration and query ----------

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

// register persists the global transaction and its steps, then dispatches; when txId already exists it idempotently returns the current status.
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

// publishPending delivers PENDING steps verbatim by payload to their respective DCN queues.
// The DCN side deduplicates via the journal unique key, so repeated delivery is safe.
func (c *Coordinator) publishPending(txID string) {
	if c.publishFn == nil {
		return // no publishing when not injected (unit tests)
	}
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
		if err := c.publishFn("rmb.steps", "step."+p.dcn, []byte(p.payload)); err != nil {
			log.Printf("tx %s step %d: publish failed: %v", txID, p.stepNo, err)
		}
	}
}

// waitFinal polls until the transaction leaves PROCESSING (at most d).
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

// ---------- Receipts and state machine ----------

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
		return nil // unknown step (possibly a duplicate receipt); drop it
	}
	_ = c.db.QueryRow(`SELECT status FROM tx_log WHERE tx_id = ?`, rc.TxID).Scan(&txStatus)

	if rc.Status == "DONE" && stepStatus == "FAILED" {
		// Late DONE receipt: the step was marked FAILED (e.g. timeout) but the downstream recovered and completed it.
		// First set the step back to DONE, then reopen the transaction if it is already COMPENSATED;
		// advance builds reverse compensations for the original DONE steps, keeping the balance totals unchanged.
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

// advance drives the transaction state machine: all DONE -> COMMITTED; any FAILED -> reverse-order compensation; compensation complete -> COMPENSATED.
// Invariant: as long as a FAILED original step exists, every DONE original step must eventually have a matching COMPENSATE_* step.
// After a late DONE receipt sets a FAILED original step back to DONE, no other FAILED step remains, but the existing
// one-to-one mapping between COMPENSATE_* and DONE original steps is broken — therefore the compensation branch
// uses "any FAILED or any existing COMPENSATE_*" as its entry condition and fills in reverse compensations by the count difference.
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
	var doneOriginals []stepRow // DONE original (non-COMPENSATE_) steps
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
		// 1) Create COMPENSATE_* steps (in reverse order) for DONE original steps not yet compensated.
		// Built by count difference: the simulation has at most 2 steps per transaction, so at most 1 DONE original step needs compensation.
		// After a late DONE sets a FAILED step back to DONE, a reverse compensation is created for it here.
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
		// 2) Compensation complete: wait for receipts / terminal state
		if len(compSteps) > 0 {
			for _, st := range compSteps {
				if st.status == "FAILED" {
					retry, err := c.retryCompensate(tx, txID, st)
					if err != nil {
						return err // infrastructure error: receipt is nacked and redelivered, then advance runs again
					}
					if retry {
						if err := tx.Commit(); err != nil {
							return err
						}
						c.publishPending(txID) // commit first, then redeliver
						return nil
					}
					c.transition(tx, txID, "PROCESSING", "FAILED")
					if err := tx.Commit(); err != nil {
						return err
					}
					// The metric counts terminal-state arrivals: the late-receipt re-compensation path counts COMPENSATED twice for the same tx
					metrics.IncTx("FAILED")
					return nil
				}
			}
			for _, st := range compSteps {
				if st.status != "DONE" {
					return nil // waiting for compensation receipts
				}
			}
			c.transition(tx, txID, "PROCESSING", "COMPENSATED")
			if err := tx.Commit(); err != nil {
				return err
			}
			// The metric counts terminal-state arrivals: the late-receipt re-compensation path counts COMPENSATED twice for the same tx
			metrics.IncTx("COMPENSATED")
			return nil
		}
		// 3) Zero DONE original steps: wait while PENDING steps remain (the timeout loop will settle them);
		// all FAILED with nothing to compensate -> empty compensation goes straight to terminal COMPENSATED.
		if hasPending {
			return nil
		}
		c.transition(tx, txID, "PROCESSING", "COMPENSATED")
		if err := tx.Commit(); err != nil {
			return err
		}
		// The metric counts terminal-state arrivals: the late-receipt re-compensation path counts COMPENSATED twice for the same tx
		metrics.IncTx("COMPENSATED")
		return nil
	}

	// No failures: all DONE -> COMMITTED
	for _, st := range steps {
		if st.status != "DONE" {
			return nil
		}
	}
	c.transition(tx, txID, "PROCESSING", "COMMITTED")
	if err := tx.Commit(); err != nil {
		return err
	}
	// The metric counts terminal-state arrivals: the late-receipt re-compensation path counts COMPENSATED twice for the same tx
	metrics.IncTx("COMMITTED")
	return nil
}

// retryCompensate resets a failed compensation step to PENDING (at most 3 times); returns (whether it was reset for redelivery, error).
// The caller must commit the transaction before calling publishPending; a failed UPDATE does not consume a retry.
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

// reopenCompensation handles a late DONE receipt: reopens a COMPENSATED transaction back to PROCESSING,
// and the caller's advance builds and publishes reverse compensation steps. Silently returns if already reopened (concurrent).
func (c *Coordinator) reopenCompensation(txID string) error {
	res, err := c.db.Exec(
		`UPDATE tx_log SET status = 'PROCESSING' WHERE tx_id = ? AND status = 'COMPENSATED'`, txID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // already in the reopen flow
	}
	log.Printf("tx %s: reopened for late-receipt compensation", txID)
	return nil
}

// compensatePayload builds a compensation message from the original step payload (amount and account unchanged, action reversed).
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

// transition updates the transaction status and logs it (verify depends on log readability).
func (c *Coordinator) transition(tx *sql.Tx, txID, from, to string) {
	if _, err := tx.Exec(`UPDATE tx_log SET status = ? WHERE tx_id = ?`, to, txID); err != nil {
		log.Printf("tx %s: transition to %s failed: %v", txID, to, err)
		return
	}
	log.Printf("tx %s: %s -> %s", txID, from, to)
}

// ---------- Timeout and recovery ----------

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

// expire marks PENDING steps of a timed-out transaction FAILED and advances compensation;
// a transaction already compensating is not considered timed out, but its PENDING compensation steps are redelivered in case a publish failure stranded them.
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
		// No timeout while compensation is in progress; but PENDING compensation steps may be stranded by a publish failure,
		// so redeliver once (the DCN side deduplicates via the journal, repeated delivery is safe). Roll back first to release row locks.
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

// recover resumes unfinished transactions at startup by redelivering PENDING steps (the DCN side is idempotent);
// transactions already compensating likewise resume by redelivering PENDING compensation steps.
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
