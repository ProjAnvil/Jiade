package rmb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"bank-dcn/lib/sqltest"
)

// aff is a driver.Result whose RowsAffected is fixed at construction.
type aff int64

func (a aff) LastInsertId() (int64, error) { return 0, nil }
func (a aff) RowsAffected() (int64, error) { return int64(a), nil }

const txSelect = "SELECT status FROM tx_log WHERE tx_id = ?"

// NewCoordinator wires the DB handle, timeout and publish function (a nil mq.Conn is fine:
// binding its Publish method value does not dereference the connection).
func TestNewCoordinator(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	c := NewCoordinator(db, nil, 3*time.Second)
	if c.db != db || c.timeout != 3*time.Second || c.publishFn == nil {
		t.Fatalf("NewCoordinator fields not wired: %+v", c)
	}
}

func createBody(txID string) string {
	if txID == "" {
		return `{"type":"TRANSFER","steps":[{"dcn":"dcn01","action":"DEBIT","accountId":1001,"amount":"50.00"}]}`
	}
	return `{"txId":"` + txID + `","type":"TRANSFER","steps":[{"dcn":"dcn01","action":"DEBIT","accountId":1001,"amount":"50.00"}]}`
}

// Handler wires /healthz, POST /transactions and GET /transactions/{txId}.
func TestHandlerRouting(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		// GET /transactions/{txId}: tx lookup returns no rows -> 404
		sqltest.Rule{Contains: "SELECT type, status FROM tx_log", Columns: []string{"type", "status"}},
	)
	c := &Coordinator{db: db}
	h := c.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/transactions", strings.NewReader(`{}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "type and non-empty steps") {
		t.Fatalf("create = %d %s, want 400 validation error", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/transactions/tx-404", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "transaction not found") {
		t.Fatalf("get = %d %s, want 404", rec.Code, rec.Body)
	}
}

// handleCreate rejects malformed bodies, missing type and empty step lists before touching the DB.
func TestHandleCreateValidation(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	c := &Coordinator{db: db}
	cases := []struct{ name, body string }{
		{"malformed json", "not json"},
		{"missing type", `{"steps":[{"dcn":"dcn01","action":"DEBIT","accountId":1,"amount":"1.00"}]}`},
		{"empty steps", `{"type":"TRANSFER","steps":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c.handleCreate(w, httptest.NewRequest(http.MethodPost, "/transactions", strings.NewReader(tc.body)))
			if w.Code != 400 {
				t.Fatalf("status = %d (%s), want 400", w.Code, w.Body)
			}
		})
	}
}

// handleCreate on a new transaction: steps are persisted, dispatched to the DCN queue,
// and the response waits for the final status via waitFinal.
func TestHandleCreateRegistersAndWaitsForFinal(t *testing.T) {
	p1 := stepPayload(t, "tx-1", 1, "DEBIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Max: 1, Columns: []string{"status"}}, // first lookup: absent
		sqltest.Rule{Contains: "INSERT INTO tx_log"},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
			Rows: [][]any{{int64(1), "dcn01", p1}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"COMMITTED"}}}, // waitFinal poll
	)
	var gotKey string
	c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
		if exchange != "rmb.steps" {
			t.Errorf("publish exchange = %s, want rmb.steps", exchange)
		}
		gotKey = key
		return nil
	}}
	w := httptest.NewRecorder()
	c.handleCreate(w, httptest.NewRequest(http.MethodPost, "/transactions", strings.NewReader(createBody("tx-1"))))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "COMMITTED") {
		t.Fatalf("status = %d body = %s, want 200 COMMITTED", w.Code, w.Body)
	}
	if gotKey != "step.dcn01" {
		t.Fatalf("step routing key = %q, want step.dcn01", gotKey)
	}
	if rec.ExecCount("INSERT INTO tx_step_log") != 1 {
		t.Fatalf("step inserts = %d, want 1", rec.ExecCount("INSERT INTO tx_step_log"))
	}
}

// handleCreate with an already terminal transaction returns the stored status without waiting.
func TestHandleCreateExistingTxReturnsStatusWithoutWaiting(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"COMMITTED"}}},
	)
	c := &Coordinator{db: db}
	w := httptest.NewRecorder()
	c.handleCreate(w, httptest.NewRequest(http.MethodPost, "/transactions", strings.NewReader(createBody("tx-1"))))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "COMMITTED") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if rec.QueryCount(txSelect) != 1 {
		t.Fatal("waitFinal must not poll for a non-PROCESSING tx")
	}
	if rec.ExecCount("INSERT INTO tx_log") != 0 {
		t.Fatal("idempotent create must not insert")
	}
}

// handleCreate surfaces register failures as 500.
func TestHandleCreateRegisterError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Err: errors.New("db down")},
	)
	c := &Coordinator{db: db}
	w := httptest.NewRecorder()
	c.handleCreate(w, httptest.NewRequest(http.MethodPost, "/transactions", strings.NewReader(createBody("tx-1"))))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "db down") {
		t.Fatalf("status = %d body = %s, want 500 db down", w.Code, w.Body)
	}
}

// handleGet renders the transaction plus ordered steps; every failure mode is a 4xx/5xx.
func TestHandleGet(t *testing.T) {
	cases := []struct {
		name    string
		rules   []sqltest.Rule
		code    int
		bodyHas []string
	}{
		{"found", []sqltest.Rule{
			{Contains: "SELECT type, status FROM tx_log", Columns: []string{"type", "status"},
				Rows: [][]any{{"TRANSFER", "COMMITTED"}}},
			{Contains: "FROM tx_step_log WHERE tx_id = ? ORDER BY step_no",
				Columns: []string{"step_no", "dcn", "action", "status"},
				Rows: [][]any{
					{int64(1), "dcn01", "DEBIT", "DONE"},
					{int64(2), "dcn02", "CREDIT", "DONE"},
				}},
		}, 200, []string{"COMMITTED", "dcn01", "dcn02"}},
		{"not found", []sqltest.Rule{
			{Contains: "SELECT type, status FROM tx_log", Columns: []string{"type", "status"}},
		}, 404, []string{"transaction not found"}},
		{"tx lookup error", []sqltest.Rule{
			{Contains: "SELECT type, status FROM tx_log", Err: errors.New("lookup boom")},
		}, 500, []string{"lookup boom"}},
		{"steps query error", []sqltest.Rule{
			{Contains: "SELECT type, status FROM tx_log", Columns: []string{"type", "status"},
				Rows: [][]any{{"TRANSFER", "PROCESSING"}}},
			{Contains: "FROM tx_step_log WHERE tx_id = ? ORDER BY step_no", Err: errors.New("steps boom")},
		}, 500, []string{"steps boom"}},
		{"step scan error", []sqltest.Rule{
			{Contains: "SELECT type, status FROM tx_log", Columns: []string{"type", "status"},
				Rows: [][]any{{"TRANSFER", "PROCESSING"}}},
			{Contains: "FROM tx_step_log WHERE tx_id = ? ORDER BY step_no",
				Columns: []string{"step_no", "dcn", "action", "status"},
				Rows:    [][]any{{"not-a-number", "dcn01", "DEBIT", "DONE"}}},
		}, 500, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := sqltest.NewDB(t, tc.rules...)
			c := &Coordinator{db: db}
			r := httptest.NewRequest(http.MethodGet, "/transactions/tx-1", nil)
			r.SetPathValue("txId", "tx-1")
			w := httptest.NewRecorder()
			c.handleGet(w, r)
			if w.Code != tc.code {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body, tc.code)
			}
			for _, want := range tc.bodyHas {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body %s missing %q", w.Body, want)
				}
			}
		})
	}
}

// register: a failing status lookup surfaces the error.
func TestRegisterLookupError(t *testing.T) {
	db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: txSelect, Err: errors.New("db down")})
	c := &Coordinator{db: db}
	_, _, err := c.register(txRequest{TxID: "tx-1", Type: "TRANSFER"})
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want db down", err)
	}
}

// register: a concurrent insert of the same tx_id (MySQL 1062) recovers the current status
// instead of failing the request.
func TestRegisterDuplicateInsertRecoversCurrentStatus(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Max: 1, Columns: []string{"status"}}, // absent on first lookup
		sqltest.Rule{Contains: "INSERT INTO tx_log",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
	)
	c := &Coordinator{db: db}
	txID, status, err := c.register(txRequest{TxID: "tx-dup", Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1, Amount: "1.00"}}})
	if err != nil || txID != "tx-dup" || status != "PROCESSING" {
		t.Fatalf("register = (%s, %s, %v)", txID, status, err)
	}
	if rec.ExecCount("INSERT INTO tx_step_log") != 0 {
		t.Fatal("no steps may be inserted for a tx owned by another request")
	}
}

// register: when the post-duplicate status re-query also fails, the original error is returned.
func TestRegisterDuplicateInsertRecoveryFails(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Max: 1, Columns: []string{"status"}},
		sqltest.Rule{Contains: "INSERT INTO tx_log",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
		sqltest.Rule{Contains: txSelect, Err: errors.New("re-query boom")},
	)
	c := &Coordinator{db: db}
	_, _, err := c.register(txRequest{TxID: "tx-dup", Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1, Amount: "1.00"}}})
	if err == nil {
		t.Fatal("duplicate insert with failing re-query must return an error")
	}
}

// register: a failing step insert aborts the transaction.
func TestRegisterStepInsertFailureAborts(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}}, // absent
		sqltest.Rule{Contains: "INSERT INTO tx_log"},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log", Err: errors.New("step insert boom")},
	)
	c := &Coordinator{db: db}
	_, _, err := c.register(txRequest{TxID: "tx-1", Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1, Amount: "1.00"}}})
	if err == nil || !strings.Contains(err.Error(), "step insert boom") {
		t.Fatalf("err = %v, want step insert boom", err)
	}
}

// register: a request without txId gets a generated id.
func TestRegisterGeneratesTxID(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}}, // absent
		sqltest.Rule{Contains: "INSERT INTO tx_log"},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
	)
	c := &Coordinator{db: db} // publishFn nil: publishPending is a no-op
	txID, status, err := c.register(txRequest{Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1, Amount: "1.00"}}})
	if err != nil || status != "PROCESSING" || !strings.HasPrefix(txID, "tx-") {
		t.Fatalf("register = (%s, %s, %v), want generated tx-* PROCESSING", txID, status, err)
	}
}

// waitFinal returns PROCESSING when the deadline elapses without a final status.
func TestWaitFinalTimesOutWhileProcessing(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
	)
	c := &Coordinator{db: db}
	if got := c.waitFinal("tx-1", 150*time.Millisecond); got != "PROCESSING" {
		t.Fatalf("waitFinal = %q, want PROCESSING", got)
	}
}

// waitFinal returns immediately once the status leaves PROCESSING (also on query errors).
func TestWaitFinalReturnsNonProcessing(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
	)
	c := &Coordinator{db: db}
	if got := c.waitFinal("tx-1", 5*time.Second); got != "FAILED" {
		t.Fatalf("waitFinal = %q, want FAILED", got)
	}
}

// publishPending: query failures and unscannable rows are tolerated; each pending step is
// published verbatim to its DCN queue.
func TestPublishPending(t *testing.T) {
	t.Run("query error is tolerated", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: "status = 'PENDING'", Err: errors.New("pending boom")})
		c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
			t.Error("must not publish on a load failure")
			return nil
		}}
		c.publishPending("tx-1")
	})
	t.Run("publishes each pending step to its DCN queue", func(t *testing.T) {
		p1 := stepPayload(t, "tx-1", 1, "DEBIT")
		p2 := stepPayload(t, "tx-1", 2, "CREDIT")
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
				Rows: [][]any{{int64(1), "dcn01", p1}, {int64(2), "dcn02", p2}}},
		)
		var keys []string
		c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
			if exchange != "rmb.steps" {
				t.Errorf("exchange = %s, want rmb.steps", exchange)
			}
			keys = append(keys, key)
			return nil
		}}
		c.publishPending("tx-1")
		if len(keys) != 2 || keys[0] != "step.dcn01" || keys[1] != "step.dcn02" {
			t.Fatalf("routing keys = %v, want [step.dcn01 step.dcn02]", keys)
		}
	})
	t.Run("unscannable rows are skipped", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
				Rows: [][]any{{"not-a-number", "dcn01", "p"}}},
		)
		c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
			t.Error("an unscannable row must not be published")
			return nil
		}}
		c.publishPending("tx-1")
	})
}
