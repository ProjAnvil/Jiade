// Package sqltest 提供可脚本化的 database/sql/driver 测试替身。
// 测试专用：供各服务包的单测共享，避免每个包各自手搓 fake driver。
// 零外部依赖；行为按 Rule 列表顺序匹配（Contains 为 SQL 子串）。
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

// Rule 是一条 SQL 匹配规则；Max>0 时最多命中 Max 次，随后落入后续规则。
type Rule struct {
	Contains string
	Max      int
	Columns  []string
	Rows     [][]any
	Err      error
	Result   driver.Result
}

// Call 记录一次 Exec/Query 调用。
type Call struct {
	Query string
	Args  []driver.Value
}

// Recorder 线程安全地记录全部调用。
type Recorder struct {
	mu      sync.Mutex
	execs   []Call
	queries []Call
}

// ExecCount 返回 Query 文本含 substr 的 Exec 次数。
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

// QueryCount 返回 Query 文本含 substr 的 Query 次数。
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

// LastExec 返回最后一条匹配的 Exec，未命中返回 nil。
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

// NewDB 注册唯一驱动名并返回 *sql.DB 与 Recorder。
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
