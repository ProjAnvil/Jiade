package messaging

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after negative confirmation")
	}
	if err := publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t)); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Publish after negative confirmation error=%v, want retired failure", err)
	}
}

func TestRabbitPublisherMandatoryReturnRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.onPublish = func() {
		channel.returns <- amqp.Return{
			ReplyCode: 312,
			ReplyText: "NO_ROUTE",
			MessageId: channel.lastMessageID(),
		}
		channel.confirmations <- amqp.Confirmation{DeliveryTag: 1, Ack: true}
	}

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if err == nil || !strings.Contains(err.Error(), "returned mandatory") {
		t.Fatalf("Publish error=%v, want mandatory-return failure", err)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after mandatory return")
	}
	assertPublisherRetired(t, publisher)
}

func TestRabbitPublisherCancellationRetiresChannelAndPreventsReuse(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	channel.onPublish = cancel

	err = publisher.Publish(ctx, "payment.initiated", testEnvelope(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish error=%v, want context.Canceled", err)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after cancellation")
	}
	if err := publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t)); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Publish after cancellation error=%v, want retired failure", err)
	}
}

func TestRabbitPublisherConfirmationClosureRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.closeConfirmationOnPublish = true

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if err == nil || !strings.Contains(err.Error(), "confirmation notification closed") {
		t.Fatalf("Publish error=%v, want confirmation-closure failure", err)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after confirmation closure")
	}
	assertPublisherRetired(t, publisher)
}

func TestRabbitPublisherReturnClosureRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.closeReturnOnPublish = true

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if err == nil || !strings.Contains(err.Error(), "return notification closed") {
		t.Fatalf("Publish error=%v, want return-closure failure", err)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after return closure")
	}
	assertPublisherRetired(t, publisher)
}

func TestRabbitPublisherWrongConfirmationSequenceRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.onPublish = func() {
		channel.confirmations <- amqp.Confirmation{DeliveryTag: 99, Ack: true}
	}

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if err == nil || !strings.Contains(err.Error(), "correlation lost") {
		t.Fatalf("Publish error=%v, want sequence-correlation failure", err)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after sequence mismatch")
	}
	assertPublisherRetired(t, publisher)
}

func TestRabbitPublisherPublishErrorRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.publishErr = errors.New("socket write failed")
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}

	err = publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t))
	if !errors.Is(err, channel.publishErr) {
		t.Fatalf("Publish error=%v, want %v", err, channel.publishErr)
	}
	if !channel.isClosed() {
		t.Fatal("publisher did not retire channel after ambiguous publish error")
	}
}

func TestRabbitPublisherObservesIdleChannelClosureAndCloseIsIdempotent(t *testing.T) {
	channel := newFakeRabbitChannel()
	publisher, err := newRabbitPublisher(channel, "bank.events")
	if err != nil {
		t.Fatal(err)
	}
	channel.asyncClose()
	select {
	case <-publisher.watcherDone:
	case <-time.After(time.Second):
		t.Fatal("publisher watcher did not observe channel closure")
	}
	if err := publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t)); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Publish after channel closure error=%v, want retired failure", err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatal(err)
	}
	if channel.closeCount() != 1 {
		t.Fatalf("channel close calls=%d, want 1", channel.closeCount())
	}
}

func assertPublisherRetired(t *testing.T, publisher *RabbitPublisher) {
	t.Helper()
	if err := publisher.Publish(context.Background(), "payment.initiated", testEnvelope(t)); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Publish after retirement error=%v, want fast retired failure", err)
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

func TestProcessDeliveryRoutesCommitFailureBeforeAckingSource(t *testing.T) {
	commitErr := errors.New("commit unavailable")
	recorder := &txRecorder{rowsAffected: 1, commitErr: commitErr}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)
	router := &recordingRouter{recorder: recorder}

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		recorder.add("handler")
		return nil
	}, retryPolicy(router))
	if !errors.Is(err, commitErr) {
		t.Fatalf("ProcessDelivery error=%v, want %v", err, commitErr)
	}

	assertEvents(t, recorder,
		"insert", "handler", "commit",
		"route:bank.retry:payment-workflow.retry", "ack")
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
	router := &recordingRouter{recorder: recorder}

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		recorder.add("handler")
		return handlerErr
	}, retryPolicy(router))
	if !errors.Is(err, handlerErr) {
		t.Fatalf("ProcessDelivery error=%v, want %v", err, handlerErr)
	}

	assertEvents(t, recorder, "insert", "handler", "rollback", "route:bank.retry:payment-workflow.retry", "ack")
	if recorder.contains("nack:false") || recorder.contains("nack:true") || recorder.contains("reject:false") {
		t.Fatalf("events=%v, delivery used broker nack/reject instead of explicit route", recorder.snapshot())
	}
	assertRepublishedDelivery(t, router, delivery)
}

func TestProcessDeliveryMalformedEnvelopeRoutesToDLQThenAcks(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, []byte(`{"message_id":"not-a-uuid"}`))
	router := &recordingRouter{recorder: recorder}

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		t.Fatal("handler called for malformed envelope")
		return nil
	}, retryPolicy(router))
	if err == nil {
		t.Fatal("ProcessDelivery error=nil, want malformed-envelope error")
	}

	assertEvents(t, recorder, "rollback", "route:bank.dlx:payment-workflow.dead", "ack")
	assertRepublishedDelivery(t, router, delivery)
}

func TestProcessDeliveryRoutesExhaustedDeliveryToDLQ(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	headers := amqp.Table{"x-death": []interface{}{
		amqp.Table{"count": int64(3), "queue": "payment-workflow.retry", "reason": "expired"},
		amqp.Table{"count": int64(50), "queue": "unrelated", "reason": "expired"},
	}}
	delivery := recordingDelivery(recorder, nil)
	delivery.Headers = headers
	router := &recordingRouter{recorder: recorder}

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		return errors.New("still unavailable")
	}, retryPolicy(router))
	if err == nil {
		t.Fatal("ProcessDelivery error=nil, want handler error")
	}

	assertEvents(t, recorder, "insert", "rollback", "route:bank.dlx:payment-workflow.dead", "ack")
	assertRepublishedDelivery(t, router, delivery)
}

func TestProcessDeliveryAcksSourceOnlyAfterConfirmedRetryRoute(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)
	confirm := make(chan struct{})
	router := &recordingRouter{recorder: recorder, confirm: confirm, routed: make(chan struct{})}
	result := make(chan error, 1)

	go func() {
		result <- ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
			return errors.New("temporary")
		}, retryPolicy(router))
	}()
	select {
	case <-router.routed:
	case <-time.After(time.Second):
		t.Fatal("delivery was not routed")
	}
	if recorder.contains("ack") {
		t.Fatalf("events=%v, source acknowledged before route confirmation", recorder.snapshot())
	}
	close(confirm)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("ProcessDelivery error=%v, want handler error", err)
	}
	if !recorder.contains("ack") {
		t.Fatalf("events=%v, source not acknowledged after route confirmation", recorder.snapshot())
	}
}

func TestProcessDeliveryRouteFailureNeverAcksSource(t *testing.T) {
	recorder := &txRecorder{rowsAffected: 1}
	tx := beginRecordingTx(t, recorder)
	delivery := recordingDelivery(recorder, nil)
	routeErr := errors.New("confirm channel closed")
	router := &recordingRouter{recorder: recorder, err: routeErr}

	err := ProcessDelivery(context.Background(), tx, "payment-workflow", delivery, func(context.Context, Envelope) error {
		return errors.New("temporary")
	}, retryPolicy(router))
	if !errors.Is(err, routeErr) || !strings.Contains(err.Error(), "route delivery") {
		t.Fatalf("ProcessDelivery error=%v, want clear route failure", err)
	}
	if recorder.contains("ack") {
		t.Fatalf("events=%v, source acknowledged after failed route", recorder.snapshot())
	}
}

func retryPolicy(router ConfirmedRouter) RetryPolicy {
	return RetryPolicy{
		MaxAttempts:          3,
		Router:               router,
		RetryQueue:           "payment-workflow.retry",
		RetryExchange:        "bank.retry",
		RetryRoutingKey:      "payment-workflow.retry",
		DeadLetterExchange:   "bank.dlx",
		DeadLetterRoutingKey: "payment-workflow.dead",
	}
}

func assertRepublishedDelivery(t *testing.T, router *recordingRouter, delivery amqp.Delivery) {
	t.Helper()
	if router.message.MessageId != delivery.MessageId {
		t.Fatalf("republished MessageId=%q, want %q", router.message.MessageId, delivery.MessageId)
	}
	if !reflect.DeepEqual(router.message.Headers, delivery.Headers) {
		t.Fatalf("republished headers=%#v, want %#v", router.message.Headers, delivery.Headers)
	}
	if !reflect.DeepEqual(router.message.Body, delivery.Body) {
		t.Fatalf("republished body=%q, want %q", router.message.Body, delivery.Body)
	}
	if router.message.DeliveryMode != amqp.Persistent {
		t.Fatalf("republished DeliveryMode=%d, want persistent", router.message.DeliveryMode)
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
		Acknowledger:  recordingAcknowledger{recorder: recorder},
		DeliveryTag:   1,
		Headers:       amqp.Table{"tenant": "bank-1"},
		ContentType:   "application/json",
		MessageId:     testEnvelopeMessageID(body),
		Type:          "payment.initiated.v1",
		CorrelationId: "correlation-1",
		Body:          body,
	}
}

func testEnvelopeMessageID(body []byte) string {
	var envelope Envelope
	if json.Unmarshal(body, &envelope) == nil && envelope.MessageID != "" {
		return envelope.MessageID
	}
	return "source-message-1"
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

type recordingRouter struct {
	recorder *txRecorder
	confirm  <-chan struct{}
	err      error
	routed   chan struct{}
	exchange string
	key      string
	message  amqp.Publishing
}

func (router *recordingRouter) Route(_ context.Context, exchange, key string, message amqp.Publishing) error {
	router.exchange = exchange
	router.key = key
	router.message = message
	router.recorder.add("route:" + exchange + ":" + key)
	if router.routed == nil {
		router.routed = make(chan struct{})
	}
	close(router.routed)
	if router.confirm != nil {
		<-router.confirm
	}
	return router.err
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
	mu                         sync.Mutex
	confirmations              chan amqp.Confirmation
	returns                    chan amqp.Return
	published                  chan struct{}
	exchange                   string
	key                        string
	mandatory                  bool
	message                    amqp.Publishing
	onPublish                  func()
	nextSequence               uint64
	publishErr                 error
	closeConfirmationOnPublish bool
	closeReturnOnPublish       bool
	closes                     chan *amqp.Error
	closed                     bool
	closeCalls                 int
	closeOnce                  sync.Once
}

func newFakeRabbitChannel() *fakeRabbitChannel {
	return &fakeRabbitChannel{published: make(chan struct{}, 1), nextSequence: 1}
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

func (channel *fakeRabbitChannel) NotifyClose(closes chan *amqp.Error) chan *amqp.Error {
	channel.closes = closes
	return closes
}

func (channel *fakeRabbitChannel) GetNextPublishSeqNo() uint64 {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.nextSequence
}

func (channel *fakeRabbitChannel) PublishWithContext(_ context.Context, exchange, key string, mandatory, _ bool, message amqp.Publishing) error {
	channel.mu.Lock()
	channel.exchange = exchange
	channel.key = key
	channel.mandatory = mandatory
	channel.message = message
	channel.nextSequence++
	onPublish := channel.onPublish
	publishErr := channel.publishErr
	closeConfirmation := channel.closeConfirmationOnPublish
	channel.closeConfirmationOnPublish = false
	closeReturn := channel.closeReturnOnPublish
	channel.closeReturnOnPublish = false
	channel.mu.Unlock()
	channel.published <- struct{}{}
	if closeConfirmation {
		close(channel.confirmations)
	}
	if closeReturn {
		close(channel.returns)
	}
	if onPublish != nil {
		onPublish()
	}
	return publishErr
}

func (channel *fakeRabbitChannel) Close() error {
	channel.closeOnce.Do(func() {
		channel.mu.Lock()
		channel.closed = true
		channel.closeCalls++
		closes := channel.closes
		channel.mu.Unlock()
		if closes != nil {
			select {
			case closes <- amqp.ErrClosed:
			default:
			}
		}
	})
	return nil
}

func (channel *fakeRabbitChannel) asyncClose() { _ = channel.Close() }

func (channel *fakeRabbitChannel) isClosed() bool {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.closed
}

func (channel *fakeRabbitChannel) closeCount() int {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.closeCalls
}

func (channel *fakeRabbitChannel) lastMessageID() string {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.message.MessageId
}

var _ driver.Driver = recordingDriver{}
var _ driver.ConnBeginTx = (*recordingConn)(nil)
var _ driver.ExecerContext = (*recordingConn)(nil)
var _ io.Closer = (*recordingConn)(nil)
