package dcnapp

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank-dcn/lib/sqltest"
)

func postTransfer(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/transfer", strings.NewReader(body))
}

// gnsLocater returns a GNS double that routes accounts 1001/1002 -> dcn01 (local pairs)
// and anything else -> dcn02 (cross-unit), with an overridable dcn01 endpoint.
func gnsLocater(dcn01Endpoint string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
		dcn, ep := "dcn02", "http://dcn02-app:8080"
		switch r.URL.Query().Get("accountId") {
		case "1001", "1002":
			dcn, ep = "dcn01", dcn01Endpoint
		}
		_ = jsonEncode(w, map[string]any{"accountId": 1, "dcn": dcn, "endpoint": ep})
	})
	return httptest.NewServer(mux)
}

// handleTransfer rejects malformed bodies and non-positive amounts before locating anything.
func TestHandleTransferValidation(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn01", db, "http://gns-unused", "http://rmb-unused")
	cases := []struct{ name, body string }{
		{"malformed json", "not json"},
		{"missing ids", `{}`},
		{"bad amount", `{"fromId":1001,"toId":1002,"amount":"abc"}`},
		{"zero amount", `{"fromId":1001,"toId":1002,"amount":"0"}`},
		{"negative amount", `{"fromId":1001,"toId":1002,"amount":"-1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleTransfer(w, postTransfer(tc.body))
			if w.Code != 400 {
				t.Fatalf("status = %d (%s), want 400", w.Code, w.Body)
			}
		})
	}
}

// A local transfer that would overdraw is rejected with 422 (conditional balance update).
func TestTransferLocalInsufficientFunds(t *testing.T) {
	gns := gnsLocater("http://dcn01-app:8080")
	defer gns.Close()
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance - ?", Result: affResult(0)}, // no row updated
	)
	s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":1002,"amount":"10.00"}`))
	if w.Code != 422 || !strings.Contains(w.Body.String(), "insufficient funds") {
		t.Fatalf("status = %d body = %s, want 422", w.Code, w.Body)
	}
}

// A journal write failure aborts a local transfer with 500.
func TestTransferLocalJournalFailure(t *testing.T) {
	gns := gnsLocater("http://dcn01-app:8080")
	defer gns.Close()
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal", Err: errors.New("journal boom")},
	)
	s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":1002,"amount":"10.00"}`))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "journal boom") {
		t.Fatalf("status = %d body = %s, want 500", w.Code, w.Body)
	}
}

// A missing credit target aborts the local transfer after the debit succeeded (500;
// the surrounding local transaction rolls back).
func TestTransferLocalCreditTargetMissing(t *testing.T) {
	gns := gnsLocater("http://dcn01-app:8080")
	defer gns.Close()
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance - ?"},
		sqltest.Rule{Contains: "balance = balance + ?", Result: affResult(0)}, // target row absent
	)
	s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":1002,"amount":"10.00"}`))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "account not found") {
		t.Fatalf("status = %d body = %s, want 500 account not found", w.Code, w.Body)
	}
}

// A client-supplied txId is passed through to the RMB registration payload.
func TestTransferCrossUnitWithClientTxID(t *testing.T) {
	var gotBody string
	rmb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_ = jsonEncode(w, map[string]string{"txId": "client-1", "status": "COMMITTED"})
	}))
	defer rmb.Close()
	gns := gnsLocater("http://dcn01-app:8080")
	defer gns.Close()
	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn01", db, gns.URL, rmb.URL)
	w := httptest.NewRecorder()
	s.handleTransfer(w, postTransfer(`{"txId":"client-1","fromId":1001,"toId":2001,"amount":"10.00"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if !strings.Contains(gotBody, `"txId":"client-1"`) {
		t.Fatalf("rmb payload %s missing client txId", gotBody)
	}
}

// Unknown source or target accounts (GNS unreachable / 404 / garbage payload) are 404s.
func TestTransferUnknownAccounts(t *testing.T) {
	t.Run("gns unreachable", func(t *testing.T) {
		gns := httptest.NewServer(http.NotFoundHandler())
		gns.Close() // closed immediately: connection failure
		db, _ := sqltest.NewDB(t)
		s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":1002,"amount":"10.00"}`))
		if w.Code != 404 || !strings.Contains(w.Body.String(), "unknown from account") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("gns garbage payload", func(t *testing.T) {
		gns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer gns.Close()
		db, _ := sqltest.NewDB(t)
		s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":1002,"amount":"10.00"}`))
		if w.Code != 404 {
			t.Fatalf("status = %d body = %s, want 404 for undecodable locate", w.Code, w.Body)
		}
	})
	t.Run("target not routed", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("accountId") == "2001" {
				w.WriteHeader(404)
				return
			}
			_ = jsonEncode(w, map[string]any{"accountId": 1, "dcn": "dcn01", "endpoint": "http://dcn01:8080"})
		})
		gns := httptest.NewServer(mux)
		defer gns.Close()
		db, _ := sqltest.NewDB(t)
		s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":2001,"amount":"10.00"}`))
		if w.Code != 404 || !strings.Contains(w.Body.String(), "unknown to account") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
}

// submitRMB: unreachable coordinator, undecodable response, and non-COMMITTED outcomes.
func TestSubmitRMBBranches(t *testing.T) {
	setup := func(rmb *httptest.Server) (*Server, func()) {
		gns := gnsLocater("http://dcn01-app:8080") // from -> dcn01 (local), to -> dcn02 (cross-unit)
		db, _ := sqltest.NewDB(t)
		return newSrv("dcn01", db, gns.URL, rmb.URL), gns.Close
	}
	body := `{"fromId":1001,"toId":2001,"amount":"10.00"}`

	t.Run("rmb unreachable", func(t *testing.T) {
		rmb := httptest.NewServer(http.NotFoundHandler())
		rmb.Close()
		s, done := setup(rmb)
		defer done()
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(body))
		if w.Code != 502 || !strings.Contains(w.Body.String(), "rmb unreachable") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("rmb returns garbage", func(t *testing.T) {
		rmb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer rmb.Close()
		s, done := setup(rmb)
		defer done()
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(body))
		if w.Code != 502 || !strings.Contains(w.Body.String(), "invalid rmb response") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("rmb not committed", func(t *testing.T) {
		rmb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = jsonEncode(w, map[string]string{"txId": "tx-x", "status": "COMPENSATED"})
		}))
		defer rmb.Close()
		s, done := setup(rmb)
		defer done()
		w := httptest.NewRecorder()
		s.handleTransfer(w, postTransfer(body))
		if w.Code != 502 || !strings.Contains(w.Body.String(), "COMPENSATED") {
			t.Fatalf("status = %d body = %s, want 502 with COMPENSATED", w.Code, w.Body)
		}
	})
}

// forward: an unreachable source-unit endpoint surfaces as 502.
func TestTransferForwardFailure(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	gns := gnsLocater(dead.URL) // source account 1001 lives on dcn01, whose endpoint is dead
	defer gns.Close()
	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn02", db, gns.URL, "http://rmb-unused") // request lands on dcn02 -> forward
	w := httptest.NewRecorder()
	s.handleTransfer(w, postTransfer(`{"fromId":1001,"toId":2002,"amount":"10.00"}`))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "forward failed") {
		t.Fatalf("status = %d body = %s, want 502 forward failed", w.Code, w.Body)
	}
}
