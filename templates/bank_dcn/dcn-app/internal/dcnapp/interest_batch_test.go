package dcnapp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank-dcn/lib/sqltest"
)

func batchRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/internal/batch/interest", strings.NewReader(body))
}

// handleInterestBatch rejects malformed bodies, wrong formats and impossible calendar dates.
func TestHandleInterestBatchValidation(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	cases := []struct{ name, body string }{
		{"malformed json", "not json"},
		{"wrong format", `{"bizDate":"2026/08/02"}`},
		{"impossible date", `{"bizDate":"2026-02-30"}`},
		{"missing bizDate", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleInterestBatch(w, batchRequest(tc.body))
			if w.Code != 400 {
				t.Fatalf("status = %d (%s), want 400", w.Code, w.Body)
			}
		})
	}
}

// handleInterestBatch posts one journal entry per account with positive interest and
// reports each posting to the ADM mirror.
func TestHandleInterestBatchPostsPerAccount(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
			Columns: []string{"account_id", "balance"},
			Rows:    [][]any{{int64(1001), "10000.00"}, {int64(1002), "20000.00"}}},
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance + ?"},
	)
	var pubs []pubCall
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&pubs)
	w := httptest.NewRecorder()
	s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"accounts":2`) ||
		!strings.Contains(w.Body.String(), `"totalInterest":"3"`) {
		t.Fatalf("body = %s, want 2 accounts and total 3", w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 2 {
		t.Fatalf("journal entries = %d, want 2", rec.ExecCount("INSERT INTO journal"))
	}
	ins := rec.LastExec("INSERT INTO journal")
	if ins == nil || ins.Args[0] != "interest-2026-08-02-1002" {
		t.Fatalf("journal key = %+v, want interest-2026-08-02-1002", ins)
	}
	if len(pubs) != 2 || pubs[0].exchange != "adm.events" {
		t.Fatalf("adm events = %+v, want one per posting", pubs)
	}
}

// handleInterestBatch on a unit without accounts succeeds with a zero total.
func TestHandleInterestBatchNoAccounts(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
			Columns: []string{"account_id", "balance"}},
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	w := httptest.NewRecorder()
	s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accounts":0`) {
		t.Fatalf("status = %d body = %s, want 200 with 0 accounts", w.Code, w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 0 {
		t.Fatal("no accounts, no postings")
	}
}

// Accounts whose rounded interest is zero are skipped without a journal entry.
func TestHandleInterestBatchSkipsZeroInterest(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
			Columns: []string{"account_id", "balance"},
			Rows:    [][]any{{int64(1001), "10.00"}}}, // 10 * 0.0001 -> 0.00
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	w := httptest.NewRecorder()
	s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accounts":0`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 0 {
		t.Fatal("zero interest must not be posted")
	}
}

// An idempotent rerun of the same bizDate skips entries already posted (journal 1062).
func TestHandleInterestBatchIdempotentRerun(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
			Columns: []string{"account_id", "balance"},
			Rows:    [][]any{{int64(1001), "10000.00"}}},
		sqltest.Rule{Contains: "INSERT INTO journal", Err: dupErr()},
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	w := httptest.NewRecorder()
	s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accounts":0`) {
		t.Fatalf("status = %d body = %s, want 200 with duplicates skipped", w.Code, w.Body)
	}
}

// Infrastructure failures during posting abort the batch with 500.
func TestHandleInterestBatchFailures(t *testing.T) {
	t.Run("account query error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT account_id, balance FROM account", Err: errors.New("acct boom")},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "acct boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("row scan error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
				Columns: []string{"account_id", "balance"},
				Rows:    [][]any{{"not-a-number", "10000.00"}}},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 {
			t.Fatalf("status = %d body = %s, want 500 for unscannable row", w.Code, w.Body)
		}
	})
	t.Run("movement failure", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
				Columns: []string{"account_id", "balance"},
				Rows:    [][]any{{int64(1001), "10000.00"}}},
			sqltest.Rule{Contains: "INSERT INTO journal", Err: errors.New("move boom")},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		w := httptest.NewRecorder()
		s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "move boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
}

// A stored balance that is not a decimal is skipped (no posting, no batch failure).
func TestHandleInterestBatchSkipsBadBalance(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT account_id, balance FROM account",
			Columns: []string{"account_id", "balance"},
			Rows:    [][]any{{int64(1001), "not-a-decimal"}}},
		sqltest.Rule{Contains: "INSERT INTO journal"},
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	w := httptest.NewRecorder()
	s.handleInterestBatch(w, batchRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"accounts":0`) {
		t.Fatalf("status = %d body = %s, want 200 with the account skipped", w.Code, w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 0 {
		t.Fatal("a non-decimal balance must not be posted")
	}
}
