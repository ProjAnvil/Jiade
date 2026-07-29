package messaging

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
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRabbitPublisherUsesPersistentMandatoryConfirmedMessages(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t)
	result := make(chan error, 1)
	go func() {
		result <- publisher.Publish(context.Background(), "payment.initiated", envelope)
	}()

	select {
	case <-channel.published:
	case <-time.After(time.Second):
		t.Fatal("publisher did not send message")
	}
	select {
	case err := <-result:
		t.Fatalf("Publish returned before confirmation: %v", err)
	default:
	}

	channel.confirmations <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	channel.mu.Lock()
	defer channel.mu.Unlock()
	if channel.exchange != "bank.events" || channel.key != "payment.initiated" {
		t.Fatalf("route=%s/%s", channel.exchange, channel.key)
	}
	if !channel.mandatory || channel.message.DeliveryMode != amqp.Persistent {
		t.Fatalf("mandatory=%v delivery_mode=%d", channel.mandatory, channel.message.DeliveryMode)
	}
	if channel.message.MessageId != envelope.MessageID || channel.message.ContentType != "application/json" {
		t.Fatalf("publishing=%#v", channel.message)
	}
}

func TestRabbitPublisherTreatsNegativeConfirmationAsFailure(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.onPublish = func() {
		channel.confirmations <- amqp.Confirmation{DeliveryTag: 1, Ack: false}
	}

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if err == nil || !strings.Contains(err.Error(), "negatively confirmed") {
		t.Fatalf("Publish error=%v, want negative-confirmation failure", err)
	}
}

func TestProcessDeliveryAcksOnlyAfterSuccessfulCommit(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		recorder.add("handler")
		return nil
	}, RetryPolicy{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	assertEvents(t, recorder, "insert", "handler", "commit", "ack")
}

func TestProcessDeliveryNeverAcksWhenCommitFails(t *testing.T) {
	commitErr := errors.New("commit unavailable")
	recorder := &txRecorder{rowsAffected: 1, commitErr: commitErr}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		recorder.add("handler")
		return nil
	}, RetryPolicy{MaxAttempts: 3})
	if !errors.Is(err, commitErr) {
		t.Fatalf("ProcessDelivery error=%v, want %v", err, commitErr)
	}

	if recorder.contains("ack") {
		t.Fatalf("events=%v, commit failure must not ack", recorder.snapshot())
	}
	if !recorder.contains("nack:false") {
		t.Fatalf("events=%v, commit failure must enter retry route", recorder.snapshot())
	}
}

func TestProcessDeliveryDuplicateCommitsAndAcksWithoutHandler(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 0}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)
	handlerCalled := false

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		handlerCalled = true
		return nil
	}, RetryPolicy{MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}

	if handlerCalled {
		t.Fatal("handler called for duplicate Inbox message")
	}
	assertEvents(t, recorder, "insert", "commit", "ack")
}

func TestProcessDeliveryHandlerFailureRollsBackAndNeverDirectlyRequeues(t *testing.T) {
	handlerErr := errors.New("dependency unavailable")
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		recorder.add("handler")
		return handlerErr
	}, RetryPolicy{MaxAttempts: 3})
	if !errors.Is(err, handlerErr) {
		t.Fatalf("ProcessDelivery error=%v, want %v", err, handlerErr)
	}

	assertEvents(t, recorder, "insert", "handler", "rollback", "nack:false")
}

func TestProcessDeliveryMalformedEnvelopeRollsBackAndRejectsToDLQ(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, []byte(`{"message_id":"not-a-uuid"}`))

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		t.Fatal("handler called for malformed envelope")
		return nil
	}, RetryPolicy{MaxAttempts: 3})
	if err == nil {
		t.Fatal("ProcessDelivery error=nil, want malformed-envelope error")
	}

	assertEvents(t, recorder, "rollback", "reject:false")
}

func TestProcessDeliveryRejectsAfterBoundedXDeathRetries(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	headers := amqp.Table{"x-death": []interface{}{
		amqp.Table{"count": int64(3), "queue": "payment-workflow.retry", "reason": "expired"},
		amqp.Table{"count": int64(50), "queue": "unrelated", "reason": "expired"},
	}}
	delivery := recordingDelivery(recorder, nil)
	delivery.Headers = headers

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		return errors.New("still unavailable")
	}, RetryPolicy{MaxAttempts: 3})
	if err == nil {
		t.Fatal("ProcessDelivery error=nil, want handler error")
	}

	if !recorder.contains("reject:false") || recorder.contains("nack:false") {
		t.Fatalf("events=%v, want terminal rejection after retry limit", recorder.snapshot())
	}
}

func testEnvelope(t *testing.T) Envelope {
	t.Helper()
	envelope := NewEnvelope(
		"payment.initiated.v1",
		"correlation-1",
		json.RawMessage(`{"amount_minor":1200}`),
		func() time.Time { return time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC) },
	)
	return envelope
}

func recordingDelivery(recorder *txRecorder, body []byte) amqp.Delivery {
	if body == nil {
		envelope := testEnvelopeWithoutT()
		body, _ = json.Marshal(envelope)
	}
	return amqp.Delivery{
		Acknowledger: recordingAcknowledger{recorder: recorder},
		DeliveryTag:  1,
		Body:         body,
	}
}

func testEnvelopeWithoutT() Envelope {
	return NewEnvelope(
		"payment.initiated.v1",
		"correlation-1",
		json.RawMessage(`{"amount_minor":1200}`),
		func() time.Time { return time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC) },
	)
}

type recordingAcknowledger struct {
	recorder *txRecorder
}

func (ack recordingAcknowledger) Ack(uint64, bool) error {
	ack.recorder.add("ack")
	return nil
}

func (ack recordingAcknowledger) Nack(_ uint64, _ bool, requeue bool) error {
	ack.recorder.add(fmt.Sprintf("nack:%v", requeue))
	return nil
}

func (ack recordingAcknowledger) Reject(_ uint64, requeue bool) error {
	ack.recorder.add(fmt.Sprintf("reject:%v", requeue))
	return nil
}

type txRecorder struct {
	mu           sync.Mutex
	events       []string
	rowsAffected int64
	execErr      error
	commitErr    error
}

func (recorder *txRecorder) add(event string) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, event)
}

func (recorder *txRecorder) snapshot() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]string(nil), recorder.events...)
}

func (recorder *txRecorder) contains(want string) bool {
	for _, event := range recorder.snapshot() {
		if event == want {
			return true
		}
	}
	return false
}

func assertEvents(t *testing.T, recorder *txRecorder, want ...string) {
	t.Helper()
	got := recorder.snapshot()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events=%v, want %v", got, want)
	}
}

var recordingDriverID atomic.Uint64

func beginRecordingTx(t *testing.T, recorder *txRecorder) *sql.Tx {
	t.Helper()
	name := fmt.Sprintf("bank-messaging-test-%d", recordingDriverID.Add(1))
	sql.Register(name, recordingDriver{recorder: recorder})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

type recordingDriver struct {
	recorder *txRecorder
}

func (driver recordingDriver) Open(string) (driver.Conn, error) {
	return &recordingConn{recorder: driver.recorder}, nil
}

type recordingConn struct {
	recorder *txRecorder
}

func (conn *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare is not supported")
}

func (conn *recordingConn) Close() error { return nil }

func (conn *recordingConn) Begin() (driver.Tx, error) {
	return &recordingTx{recorder: conn.recorder}, nil
}

func (conn *recordingConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return conn.Begin()
}

func (conn *recordingConn) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	conn.recorder.add("insert")
	if conn.recorder.execErr != nil {
		return nil, conn.recorder.execErr
	}
	return driver.RowsAffected(conn.recorder.rowsAffected), nil
}

type recordingTx struct {
	recorder *txRecorder
}

func (tx *recordingTx) Commit() error {
	tx.recorder.add("commit")
	return tx.recorder.commitErr
}

func (tx *recordingTx) Rollback() error {
	tx.recorder.add("rollback")
	return nil
}

type fakeRabbitChannel struct {
	mu            sync.Mutex
	confirmations chan amqp.Confirmation
	returns       chan amqp.Return
	published     chan struct{}
	exchange      string
	key           string
	mandatory     bool
	message       amqp.Publishing
	onPublish     func()
}

func newFakeRabbitChannel() *fakeRabbitChannel {
	return &fakeRabbitChannel{published: make(chan struct{}, 1)}
}

func (channel *fakeRabbitChannel) Confirm(bool) error { return nil }

func (channel *fakeRabbitChannel) NotifyPublish(confirmations chan amqp.Confirmation) chan amqp.Confirmation {
	channel.confirmations = confirmations
	return confirmations
}

func (channel *fakeRabbitChannel) NotifyReturn(returns chan amqp.Return) chan amqp.Return {
	channel.returns = returns
	return returns
}

func (channel *fakeRabbitChannel) GetNextPublishSeqNo() uint64 { return 1 }

func (channel *fakeRabbitChannel) PublishWithContext(_ context.Context, exchange, key string, mandatory, _ bool, message amqp.Publishing) error {
	channel.mu.Lock()
	channel.exchange = exchange
	channel.key = key
	channel.mandatory = mandatory
	channel.message = message
	onPublish := channel.onPublish
	channel.mu.Unlock()
	channel.published <- struct{}{}
	if onPublish != nil {
		onPublish()
	}
	return nil
}

func (channel *fakeRabbitChannel) Close() error {
	close(channel.confirmations)
	close(channel.returns)
	return nil
}

var _ driver.Driver = recordingDriver{}
var _ driver.ConnBeginTx = (*recordingConn)(nil)
var _ driver.ExecerContext = (*recordingConn)(nil)
var _ io.Closer = (*recordingConn)(nil)
