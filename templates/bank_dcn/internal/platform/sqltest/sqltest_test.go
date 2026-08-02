package sqltest

import (
	"database/sql"
	"errors"
	"testing"
)

func TestRuleMatchingAndRecorder(t *testing.T) {
	db, rec := NewDB(t,
		Rule{Contains: "SELECT a", Max: 1, Columns: []string{"a"}},                    // empty set → ErrNoRows
		Rule{Contains: "SELECT a", Columns: []string{"a"}, Rows: [][]any{{int64(7)}}}, // second call returns 7
		Rule{Contains: "UPDATE t", Result: nil},
	)
	var v int
	if err := db.QueryRow("SELECT a FROM t").Scan(&v); !errors.Is(err, sql.ErrNoRows) && err == nil {
		t.Fatal("first query should be ErrNoRows")
	}
	if err := db.QueryRow("SELECT a FROM t").Scan(&v); err != nil || v != 7 {
		t.Fatalf("second query: v=%d err=%v", v, err)
	}
	if _, err := db.Exec("UPDATE t SET x=?", 1); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE t") != 1 || rec.QueryCount("SELECT a") != 2 {
		t.Fatalf("recorder: %+v", rec)
	}
	if rec.LastExec("UPDATE t").Args[0] != int64(1) {
		t.Fatalf("args not recorded: %+v", rec.LastExec("UPDATE t"))
	}
}

func TestRuleErrorAndTx(t *testing.T) {
	boom := errors.New("boom")
	db, _ := NewDB(t, Rule{Contains: "INSERT INTO j", Err: boom})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO j VALUES (1)"); !errors.Is(err, boom) {
		t.Fatalf("tx exec err = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNoRulePanics(t *testing.T) {
	db, _ := NewDB(t)
	if _, err := db.Exec("SELECT 1"); err == nil {
		t.Fatal("unmatched SQL should error")
	}
}
