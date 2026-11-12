// Package sqltest provides a scriptable database/sql/driver test double.
// Test-only: shared by unit tests across service packages, so each package
// avoids hand-rolling its own fake driver.
// Zero external dependencies; behavior matches the Rule list in order
// (Contains is an SQL substring).
package sqltest

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Rule is a single SQL matching rule; when Max>0 it matches at most Max times, then later rules take over.
type Rule struct {
	Contains string
	Max      int
	Columns  []string
	Rows     [][]any
	Err      error
	Result   driver.Result
}

// Call records a single Exec/Query invocation.
type Call struct {
	Query string
	Args  []driver.Value
}

// Recorder records all calls in a thread-safe manner.
type Recorder struct {
	mu      sync.Mutex
	execs   []Call
	queries []Call
}

// ExecCount returns the number of Exec calls whose query text contains substr.
func (r *Recorder) ExecCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.execs {
		if strings.Contains(c.Query, substr) {
			n++
		}
	}
	return n
}

// QueryCount returns the number of Query calls whose query text contains substr.
func (r *Recorder) QueryCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.queries {
		if strings.Contains(c.Query, substr) {
			n++
		}
	}
	return n
}

// LastExec returns the last matching Exec call, or nil if none matched.
func (r *Recorder) LastExec(substr string) *Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.execs) - 1; i >= 0; i-- {
		if strings.Contains(r.execs[i].Query, substr) {
			c := r.execs[i]
			return &c
		}
	}
	return nil
}

type ruleState struct {
	rule Rule
	hits int
}

type fakeDriver struct {
	rules *[]*ruleState
	rec   *Recorder
}

func (d fakeDriver) Open(string) (driver.Conn, error) {
	return &conn{rules: d.rules, rec: d.rec}, nil
}

type conn struct {
	rules *[]*ruleState
	rec   *Recorder
}

func (c *conn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("sqltest: Prepare unsupported")
}
func (c *conn) Close() error { return nil }

func (c *conn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

func (c *conn) match(query string) (*ruleState, error) {
	for _, rs := range *c.rules {
		if rs.rule.Max > 0 && rs.hits >= rs.rule.Max {
			continue
		}
		if strings.Contains(query, rs.rule.Contains) {
			rs.hits++
			return rs, nil
		}
	}
	prefix := query
	if len(prefix) > 60 {
		prefix = prefix[:60]
	}
	return nil, fmt.Errorf("sqltest: no rule for: %s", prefix)
}

func (c *conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	rs, err := c.match(query)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.execs = append(c.rec.execs, Call{query, args})
	c.rec.mu.Unlock()
	if rs.rule.Err != nil {
		return nil, rs.rule.Err
	}
	if rs.rule.Result != nil {
		return rs.rule.Result, nil
	}
	return result(1), nil
}

func (c *conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	rs, err := c.match(query)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.queries = append(c.rec.queries, Call{query, args})
	c.rec.mu.Unlock()
	if rs.rule.Err != nil {
		return nil, rs.rule.Err
	}
	return &rows{cols: rs.rule.Columns, data: rs.rule.Rows}, nil
}

type result int64

func (r result) LastInsertId() (int64, error) { return 0, nil }
func (r result) RowsAffected() (int64, error) { return int64(r), nil }

type rows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.pos]
	r.pos++
	for i := range dest {
		if i < len(row) {
			dest[i] = driver.Value(row[i])
		}
	}
	return nil
}

var seq atomic.Int64

// NewDB registers a unique driver name and returns a *sql.DB and a Recorder.
func NewDB(t *testing.T, rules ...Rule) (*sql.DB, *Recorder) {
	t.Helper()
	rec := &Recorder{}
	states := make([]*ruleState, len(rules))
	for i, r := range rules {
		states[i] = &ruleState{rule: r}
	}
	name := fmt.Sprintf("sqltest-%d", seq.Add(1))
	sql.Register(name, fakeDriver{rules: &states, rec: rec})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, rec
}
