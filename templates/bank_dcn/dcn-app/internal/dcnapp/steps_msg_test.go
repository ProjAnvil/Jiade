package dcnapp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bank-dcn/lib/contracts"
	"bank-dcn/lib/sqltest"
)

func stepMsg(txID string, stepNo int, action, amount string) []byte {
	b, _ := json.Marshal(contracts.StepMessage{
		TxID: txID, StepNo: stepNo, Action: action, AccountID: 1001, Amount: amount,
	})
	return b
}

// handleStep drops poison messages (bad JSON, unknown action, bad amount) without DB access.
func TestHandleStepDropsInvalidMessages(t *testing.T) {
	db, rec := sqltest.NewDB(t)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	msgs := []string{
		"not json",
		`{"txId":"t","stepNo":1,"action":"BOGUS","accountId":1,"amount":"1.00"}`,
		`{"txId":"t","stepNo":1,"action":"DEBIT","accountId":1,"amount":"abc"}`,
	}
	for _, m := range msgs {
		if err := s.handleStep([]byte(m)); err != nil {
			t.Fatalf("handleStep(%s) = %v, want nil (drop)", m, err)
		}
	}
	if rec.ExecCount("") != 0 || rec.QueryCount("") != 0 {
		t.Fatal("poison messages must not touch the database")
	}
}

// handleStep DEBIT: journal entry + conditional balance update, ADM event, DONE receipt.
func TestHandleStepDebitSuccess(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance - ?"},
	)
	var pubs []pubCall
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&pubs)
	if err := s.handleStep(stepMsg("ts1", 1, "DEBIT", "10.00")); err != nil {
		t.Fatal(err)
	}
	if len(pubs) != 2 {
		t.Fatalf("want adm event + receipt, got %v", pubs)
	}
	if pubs[0].exchange != "adm.events" || !strings.Contains(pubs[0].body, `"direction":"DEBIT"`) {
		t.Errorf("adm event = %+v", pubs[0])
	}
	if pubs[1].exchange != "" || pubs[1].key != "rmb.receipts" ||
		!strings.Contains(pubs[1].body, `"status":"DONE"`) {
		t.Errorf("receipt = %+v", pubs[1])
	}
	if rec.ExecCount("balance = balance - ?") != 1 {
		t.Fatal("DEBIT must move the balance")
	}
}

// handleStep COMPENSATE_CREDIT: compensation debits under a ":comp"-suffixed journal key.
func TestHandleStepCompensateCreditDebits(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance - ?"},
	)
	var pubs []pubCall
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&pubs)
	if err := s.handleStep(stepMsg("ts2", 3, "COMPENSATE_CREDIT", "10.00")); err != nil {
		t.Fatal(err)
	}
	ins := rec.LastExec("INSERT INTO journal")
	if ins == nil || ins.Args[0] != "ts2:comp" {
		t.Fatalf("journal idempotency key = %+v, want ts2:comp", ins)
	}
	if rec.ExecCount("balance = balance - ?") != 1 {
		t.Fatal("COMPENSATE_CREDIT must debit the account")
	}
	if len(pubs) != 2 || !strings.Contains(pubs[1].body, `"status":"DONE"`) {
		t.Fatalf("publications = %+v", pubs)
	}
}

// handleStep business failures produce FAILED receipts (nack-free) instead of requeue.
func TestHandleStepBusinessFailures(t *testing.T) {
	cases := []struct {
		name   string
		action string
		rules  []sqltest.Rule
		reason string
	}{
		{"insufficient funds on DEBIT", "DEBIT", []sqltest.Rule{
			{Contains: "INSERT INTO journal"},
			{Contains: "balance = balance - ?", Result: affResult(0)}, // no row updated
		}, "insufficient funds"},
		{"unknown account on CREDIT", "CREDIT", []sqltest.Rule{
			{Contains: "INSERT INTO journal"},
			{Contains: "balance = balance + ?", Result: affResult(0)},
		}, "account not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pubs []pubCall
			db, _ := sqltest.NewDB(t, tc.rules...)
			s := newSrv("dcn01", db, "http://gns", "http://rmb")
			s.publishFn = collectingPublish(&pubs)
			if err := s.handleStep(stepMsg("ts3", 1, tc.action, "10.00")); err != nil {
				t.Fatalf("business failure must not requeue, got %v", err)
			}
			if len(pubs) != 1 || !strings.Contains(pubs[0].body, `"status":"FAILED"`) ||
				!strings.Contains(pubs[0].body, tc.reason) {
				t.Fatalf("receipt = %+v, want FAILED %q", pubs, tc.reason)
			}
		})
	}
}

// handleStep: a duplicate journal entry is an idempotent DONE; the balance is not touched again.
func TestHandleStepDuplicateJournalIsIdempotent(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal", Err: dupErr()},
	)
	var pubs []pubCall
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&pubs)
	if err := s.handleStep(stepMsg("ts4", 1, "DEBIT", "10.00")); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE account") != 0 {
		t.Fatal("a duplicate journal entry must not move the balance")
	}
	if len(pubs) != 1 || !strings.Contains(pubs[0].body, "duplicate ignored") {
		t.Fatalf("receipt = %+v, want DONE duplicate ignored", pubs)
	}
}

// handleStep: infrastructure failures propagate (nack + redelivery).
func TestHandleStepInfraErrorPropagates(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal", Err: errors.New("journal boom")},
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&[]pubCall{})
	err := s.handleStep(stepMsg("ts5", 1, "DEBIT", "10.00"))
	if err == nil || !strings.Contains(err.Error(), "journal boom") {
		t.Fatalf("err = %v, want journal boom", err)
	}
}

// handleStep: a failing balance UPDATE is an infrastructure error, not a business FAILED.
func TestHandleStepUpdateErrorPropagates(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance = balance - ?", Err: errors.New("update boom")},
	)
	s := newSrv("dcn01", db, "http://gns", "http://rmb")
	s.publishFn = collectingPublish(&[]pubCall{})
	if err := s.handleStep(stepMsg("ts5b", 1, "DEBIT", "10.00")); err == nil ||
		!strings.Contains(err.Error(), "update boom") {
		t.Fatalf("err = %v, want update boom", err)
	}
}

// handleStep: a failing receipt publish redelivers the step (applyStep is idempotent),
// while a failing ADM event publish is tolerated.
func TestHandleStepPublishFailures(t *testing.T) {
	t.Run("receipt publish failure redelivers", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO journal"},
			sqltest.Rule{Contains: "balance = balance - ?"},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		s.publishFn = func(exchange, key string, body []byte) error {
			if key == "rmb.receipts" {
				return errors.New("mq down")
			}
			return nil
		}
		err := s.handleStep(stepMsg("ts6", 1, "DEBIT", "10.00"))
		if err == nil || !strings.Contains(err.Error(), "mq down") {
			t.Fatalf("err = %v, want mq down", err)
		}
	})
	t.Run("adm event publish failure is tolerated", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "INSERT INTO journal"},
			sqltest.Rule{Contains: "balance = balance - ?"},
		)
		s := newSrv("dcn01", db, "http://gns", "http://rmb")
		s.publishFn = func(exchange, key string, body []byte) error {
			if exchange == "adm.events" {
				return errors.New("adm down")
			}
			return nil
		}
		if err := s.handleStep(stepMsg("ts7", 1, "DEBIT", "10.00")); err != nil {
			t.Fatalf("adm failure must be tolerated, got %v", err)
		}
	})
}
