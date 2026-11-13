package workflow

// Minimal stdlib-only fake SQL driver used to unit-test repo query/scan
// logic without a live PostgreSQL. The repo layer takes *sql.DB directly
// (no other seam exists), so tests build a *sql.DB over scripted query
// plans via sql.OpenDB + database/sql/driver. Live-DB behaviour stays
// covered by the integration-gated tests in this package.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
)

// queryPlan is one scripted outcome, routed by a case-insensitive marker
// that must appear in the SQL text (e.g. "from loan_product").
type queryPlan struct {
	marker   string
	cols     []string
	rows     [][]driver.Value
	err      error // returned before producing any rows
	iterErr  error // returned by Next after data is exhausted (surfaces as rows.Err)
	affected int64 // ExecContext result
}

// fakeDB collects query plans and records every executed statement.
type fakeDB struct {
	mu      sync.Mutex
	plans   []queryPlan
	calls   []fakeCall
	begins  int
	commits int
	rolls   int
}

type fakeCall struct {
	query string
	args  []driver.Value
}

func (d *fakeDB) add(p queryPlan) { d.plans = append(d.plans, p) }

// plan returns the first plan whose marker matches the query.
func (d *fakeDB) plan(query string) *queryPlan {
	lower := strings.ToLower(query)
	for i := range d.plans {
		if strings.Contains(lower, d.plans[i].marker) {
			return &d.plans[i]
		}
	}
	return nil
}

func (d *fakeDB) record(query string, args []driver.NamedValue) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	d.mu.Lock()
	d.calls = append(d.calls, fakeCall{query: query, args: vals})
	d.mu.Unlock()
}

// lastCallWith returns the recorded call whose SQL contains marker.
func (d *fakeDB) lastCallWith(marker string) *fakeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.calls) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(d.calls[i].query), marker) {
			c := d.calls[i]
			return &c
		}
	}
	return nil
}

func (d *fakeDB) counters() (begins, commits, rolls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.begins, d.commits, d.rolls
}

// open returns a *sql.DB backed by this fake driver.
func (d *fakeDB) open() *sql.DB {
	return sql.OpenDB(fakeConnector{db: d})
}

type fakeConnector struct{ db *fakeDB }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeConn{db: c.db}, nil
}
func (c fakeConnector) Driver() driver.Driver { return fakeDriver{c.db} }

type fakeDriver struct{ db *fakeDB }

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("fakesql: Open not supported, use sql.OpenDB")
}

type fakeConn struct{ db *fakeDB }

var (
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.ConnBeginTx    = (*fakeConn)(nil)
)

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("fakesql: Prepare not supported")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }
func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.db.mu.Lock()
	c.db.begins++
	c.db.mu.Unlock()
	return fakeTx{db: c.db}, nil
}

func (c *fakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.db.record(query, args)
	p := c.db.plan(query)
	if p == nil {
		return nil, fmt.Errorf("fakesql: no plan for %q", query)
	}
	if p.err != nil {
		return nil, p.err
	}
	return driver.RowsAffected(p.affected), nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.record(query, args)
	p := c.db.plan(query)
	if p == nil {
		return nil, fmt.Errorf("fakesql: no plan for %q", query)
	}
	if p.err != nil {
		return nil, p.err
	}
	rows := make([][]driver.Value, len(p.rows))
	copy(rows, p.rows)
	iterErr := p.iterErr
	return &fakeRows{cols: p.cols, rows: rows, iterErr: iterErr}, nil
}

type fakeTx struct{ db *fakeDB }

func (t fakeTx) Commit() error {
	t.db.mu.Lock()
	t.db.commits++
	t.db.mu.Unlock()
	return nil
}
func (t fakeTx) Rollback() error {
	t.db.mu.Lock()
	t.db.rolls++
	t.db.mu.Unlock()
	return nil
}

type fakeRows struct {
	cols   []string
	rows   [][]driver.Value
	idx    int
	iterErr error // one-shot error surfaced after the data is exhausted
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		if r.iterErr != nil {
			err := r.iterErr
			r.iterErr = nil
			return err
		}
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	copy(dest, row)
	return nil
}
