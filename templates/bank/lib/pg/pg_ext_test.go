package pg

// Tests for Open (lazy, no live connection) and RunInTx commit/rollback/panic
// semantics using the fakesql harness in fakesql_test.go.

import (
	"context"
	"errors"
	"testing"
)

func TestOpen_ReturnsPoolWithoutConnecting(t *testing.T) {
	// sql.Open is lazy: a well-formed DSN opens without touching the network.
	db, err := Open("core_db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned a nil pool")
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRunInTx_CommitsOnSuccess(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into t", affected: 1})
	err := RunInTx(context.Background(), db.open(), func(q DBTX) error {
		_, err := q.ExecContext(context.Background(), "INSERT INTO t VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	begins, commits, rolls := db.counters()
	if begins != 1 || commits != 1 || rolls != 0 {
		t.Errorf("tx counters = begin:%d commit:%d rollback:%d, want 1/1/0", begins, commits, rolls)
	}
}

func TestRunInTx_RollsBackOnError(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into t", affected: 1})
	boom := errors.New("boom")
	err := RunInTx(context.Background(), db.open(), func(q DBTX) error {
		if _, err := q.ExecContext(context.Background(), "INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("RunInTx must surface the callback error, got %v", err)
	}
	_, commits, rolls := db.counters()
	if commits != 0 || rolls != 1 {
		t.Errorf("tx counters commit:%d rollback:%d, want 0/1", commits, rolls)
	}
}

func TestRunInTx_RollsBackAndRepanics(t *testing.T) {
	db := &fakeDB{}
	defer func() {
		if r := recover(); r != "kaboom" {
			t.Fatalf("panic must be re-raised, got %v", r)
		}
		_, commits, rolls := db.counters()
		if commits != 0 || rolls != 1 {
			t.Errorf("tx counters commit:%d rollback:%d, want 0/1 after panic", commits, rolls)
		}
	}()
	_ = RunInTx(context.Background(), db.open(), func(DBTX) error {
		panic("kaboom")
	})
}

func TestRunInTx_BeginFailurePropagates(t *testing.T) {
	// A fake whose connection cannot begin a transaction: force it by
	// using a *sql.DB that is already closed.
	db := &fakeDB{}
	pool := db.open()
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	err := RunInTx(context.Background(), pool, func(DBTX) error { return nil })
	if err == nil {
		t.Fatal("begin failure must surface")
	}
}
