package corebanking

// Tests for the consumer's transactional AMQP entry point (ConsumeDelivery)
// and the OutboxRelay drain loop. Uses the fakesql harness (bottom of this
// file) so the Inbox tx lifecycle runs without PostgreSQL, a stub
// acknowledger instead of a broker, and the stubs already defined in
// consumer_test.go.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"bank/core-banking/corebanking/domain"
	"bank/lib/messaging"
)

// ackingAcknowledger satisfies amqp.Acknowledger.
type ackingAcknowledger struct {
	mu     sync.Mutex
	acked  int
	nacked int
}

func (a *ackingAcknowledger) Ack(uint64, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acked++
	return nil
}
func (a *ackingAcknowledger) Nack(uint64, bool, bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nacked++
	return nil
}
func (a *ackingAcknowledger) Reject(uint64, bool) error { return nil }

func placeHoldDelivery(t *testing.T, workflowID string) amqp.Delivery {
	t.Helper()
	env := makeEnvelope(CmdPlaceHold, placeHoldPayload{
		AccountNo:      "D1",
		IdempotencyKey: "idem-1",
		AmountCents:    5000,
		Currency:       "CNY",
	})
	env.MessageID = testUUID()
	env.WorkflowID = workflowID
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return amqp.Delivery{Body: raw, Acknowledger: &ackingAcknowledger{}}
}

func TestNewConsumer_DefaultsNow(t *testing.T) {
	c := NewConsumer(nil, nil, nil, nil, messaging.RetryPolicy{}, nil)
	if c.now == nil {
		t.Error("NewConsumer must default now to time.Now")
	}
}

func TestConsumeDelivery_PlaceHoldCommitsAndAcks(t *testing.T) {
	holds := &stubHoldCommander{
		placeHoldResult: domain.Hold{HoldID: "H1", AccountNo: "D1", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY", Status: domain.HoldStatusActive},
	}
	outbox := &capturingOutbox{}
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into inbox_message", affected: 1})
	c := NewConsumer(db.open(), holds, &stubTransferCommander{}, outbox, messaging.RetryPolicy{}, fixedNow)

	if err := c.ConsumeDelivery(context.Background(), placeHoldDelivery(t, "wf-tx-1")); err != nil {
		t.Fatalf("ConsumeDelivery: %v", err)
	}
	if len(holds.placeHoldCalls) != 1 {
		t.Fatalf("PlaceHold calls = %d, want 1", len(holds.placeHoldCalls))
	}
	if holds.placeHoldCalls[0].AccountNo != "D1" || holds.placeHoldCalls[0].Amount.Cents() != 5000 {
		t.Errorf("place-hold payload wrong: %+v", holds.placeHoldCalls[0])
	}
	if msgs := outbox.msgs; len(msgs) != 1 {
		t.Errorf("outbox msgs = %d, want 1 (hold-placed result event)", len(msgs))
	}
	begins, commits, rolls := db.counters()
	if begins != 1 || commits != 1 || rolls != 0 {
		t.Errorf("tx counters = begin:%d commit:%d rollback:%d, want 1/1/0", begins, commits, rolls)
	}
}

func TestConsumeDelivery_DuplicateInboxSkipsHandlerButAcks(t *testing.T) {
	holds := &stubHoldCommander{}
	outbox := &capturingOutbox{}
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into inbox_message", affected: 0}) // ON CONFLICT
	c := NewConsumer(db.open(), holds, &stubTransferCommander{}, outbox, messaging.RetryPolicy{}, fixedNow)

	if err := c.ConsumeDelivery(context.Background(), placeHoldDelivery(t, "wf-dup")); err != nil {
		t.Fatalf("ConsumeDelivery: %v", err)
	}
	if len(holds.placeHoldCalls) != 0 {
		t.Errorf("PlaceHold calls = %d, want 0 for a duplicate delivery", len(holds.placeHoldCalls))
	}
	if len(outbox.msgs) != 0 {
		t.Errorf("outbox msgs = %d, want 0", len(outbox.msgs))
	}
	_, commits, rolls := db.counters()
	if commits != 1 || rolls != 0 {
		t.Errorf("tx counters commit:%d rollback:%d, want 1/0", commits, rolls)
	}
}

// --- OutboxRelay ---

func TestNewOutboxRelay_Defaults(t *testing.T) {
	r := NewOutboxRelay(nil, nil)
	if r.Interval != 500*time.Millisecond || r.BatchSize != 100 {
		t.Errorf("defaults wrong: interval=%v batch=%d", r.Interval, r.BatchSize)
	}
}

func TestOutboxRelay_Run_GuardsAndCancel(t *testing.T) {
	if err := (&OutboxRelay{}).Run(context.Background()); err == nil {
		t.Error("nil db must fail fast")
	}
	db := &fakeDB{}
	if err := NewOutboxRelay(db.open(), nil).Run(context.Background()); err == nil {
		t.Error("nil publisher must fail fast")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewOutboxRelay(db.open(), &relayPublisherStub{}).Run(ctx); err != nil {
		t.Errorf("Run on cancelled ctx = %v, want nil", err)
	}
}

type relayPublisherStub struct {
	err   error
	route string
}

func (p *relayPublisherStub) PublishTo(_ context.Context, _, routingKey string, _ messaging.Envelope) error {
	p.route = routingKey
	return p.err
}

func TestOutboxRelay_DrainPublishesAndMarksDispatched(t *testing.T) {
	env := makeEnvelope("core.hold-placed.v1", map[string]string{})
	raw, _ := json.Marshal(env)
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from outbox_message",
		cols:   []string{"message_id", "routing_key", "envelope"},
		rows:   [][]driver.Value{{env.MessageID, "core.hold.placed", []byte(raw)}},
	})
	db.add(queryPlan{marker: "update outbox_message", affected: 1})
	pub := &relayPublisherStub{}
	relay := NewOutboxRelay(db.open(), pub)
	if err := relay.drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if pub.route != "core.hold.placed" {
		t.Errorf("published routing key = %q", pub.route)
	}
	if call := db.lastCallWith("update outbox_message"); call == nil || call.args[0] != env.MessageID {
		t.Errorf("dispatched update missing: %+v", call)
	}
}

func TestOutboxRelay_DrainPoisonMessageMarked(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from outbox_message",
		cols:   []string{"message_id", "routing_key", "envelope"},
		rows:   [][]driver.Value{{"poison-1", "core.hold.placed", []byte("{not-json")}},
	})
	db.add(queryPlan{marker: "update outbox_message", affected: 1})
	pub := &relayPublisherStub{}
	relay := NewOutboxRelay(db.open(), pub)
	if err := relay.drain(context.Background()); err != nil {
		t.Fatalf("drain must tolerate poison messages: %v", err)
	}
	if pub.route != "" {
		t.Error("poison message must not be published")
	}
	if call := db.lastCallWith("update outbox_message"); call == nil || call.args[0] != "poison-1" {
		t.Errorf("poison message must be marked: %+v", call)
	}
}

func TestOutboxRelay_DrainPublishFailurePropagates(t *testing.T) {
	env := makeEnvelope("core.hold-placed.v1", map[string]string{})
	raw, _ := json.Marshal(env)
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from outbox_message",
		cols:   []string{"message_id", "routing_key", "envelope"},
		rows:   [][]driver.Value{{env.MessageID, "core.hold.placed", []byte(raw)}},
	})
	relay := NewOutboxRelay(db.open(), &relayPublisherStub{err: errors.New("broker down")})
	err := relay.drain(context.Background())
	if err == nil || !strings.Contains(err.Error(), "broker down") {
		t.Fatalf("want publish failure surfaced, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// fakesql harness (same design as corebanking/repo/fakesql_test.go;
// duplicated here because this package's tests need their own *sql.DB).
// ---------------------------------------------------------------------------

type queryPlan struct {
	marker   string
	cols     []string
	rows     [][]driver.Value
	err      error
	iterErr  error
	affected int64
}

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
func (d *fakeDB) open() *sql.DB { return sql.OpenDB(fakeConnector{db: d}) }

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

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fakesql: Prepare not supported")
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
		return nil, errors.New("fakesql: no plan for " + query)
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
		return nil, errors.New("fakesql: no plan for " + query)
	}
	if p.err != nil {
		return nil, p.err
	}
	rows := make([][]driver.Value, len(p.rows))
	copy(rows, p.rows)
	return &fakeRows{cols: p.cols, rows: rows, iterErr: p.iterErr}, nil
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
	cols    []string
	rows    [][]driver.Value
	idx     int
	iterErr error
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
