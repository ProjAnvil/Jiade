package migrate

// Tests for Run (statement execution + error wrapping) using the fakesql
// harness in fakesql_test.go, plus table tests for firstLine.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRun_ExecutesEachStatement(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "create table t1", affected: 0})
	db.add(queryPlan{marker: "create table t2", affected: 0})
	if err := Run(context.Background(), db.open(), "CREATE TABLE t1 (id int);\nCREATE TABLE t2 (id int);"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(db.callsFor("create table")); got != 2 {
		t.Errorf("statements executed = %d, want 2", got)
	}
}

func TestRun_FailureCarriesFirstLine(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "create table ok", affected: 0})
	db.add(queryPlan{marker: "create index bad", err: errors.New("syntax error")})
	ddl := "CREATE INDEX bad ON t;\nthis line is never reached;"
	err := Run(context.Background(), db.open(), "CREATE TABLE ok (id int);\n"+ddl)
	if err == nil {
		t.Fatal("failing statement must surface")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("cause missing: %v", err)
	}
	if !strings.Contains(err.Error(), "CREATE INDEX bad ON t") {
		t.Errorf("error must carry the failing statement's first line: %v", err)
	}
}

func TestRun_EmptyDDLIstNoop(t *testing.T) {
	db := &fakeDB{}
	if err := Run(context.Background(), db.open(), "  \n;; \n"); err != nil {
		t.Fatalf("Run on empty DDL: %v", err)
	}
	if got := len(db.callsFor("")); got != 0 {
		t.Errorf("no statement should execute, got %d calls", got)
	}
}

func TestFirstLine(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"single line", "single line"},
		{"first\nsecond\nthird", "first"},
		{"", ""},
		{"\nonly newline", ""},
	}
	for _, tc := range tests {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
