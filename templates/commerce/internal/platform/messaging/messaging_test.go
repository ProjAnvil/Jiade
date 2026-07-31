package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func fixedClock() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) }

func testEvent() Event {
	return NewEvent("order.placed.v1", "ORD-1", "corr-1", "cause-1", json.RawMessage(`{"total_minor":1200}`), fixedClock)
}

func TestEventRoundTripPreservesTracingFields(t *testing.T) {
	want := testEvent()
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Event
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestNewEventUsesCurrentSchemaAndClock(t *testing.T) {
	event := testEvent()
	if event.SchemaVersion != CurrentSchemaVersion || !event.OccurredAt.Equal(fixedClock()) || !validEventID(event.ID) {
		t.Fatalf("event = %#v", event)
	}
}

func TestNewEventPanicsInsteadOfReturningInvalidIDWhenEntropyFails(t *testing.T) {
	original := randomRead
	randomRead = func([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
	t.Cleanup(func() { randomRead = original })
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("NewEvent() did not panic when UUID entropy failed")
		}
	}()
	_ = testEvent()
}

func TestHandleOnceSkipsDuplicateEventForSameConsumer(t *testing.T) {
	tx := &inboxTx{}
	event := testEvent()
	calls := 0
	for range 2 {
		if err := HandleOnce(context.Background(), tx, "inventory-projection", event, func() error {
			calls++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
}

func TestHandleOnceDoesNotCrossDeduplicateConsumers(t *testing.T) {
	tx := &inboxTx{}
	event := testEvent()
	calls := 0
	for _, consumer := range []string{"inventory-projection", "analytics"} {
		if err := HandleOnce(context.Background(), tx, consumer, event, func() error {
			calls++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d, want 2", calls)
	}
}

func TestRelayMarksPublishedOnlyAfterPublisherConfirms(t *testing.T) {
	event := testEvent()
	store := &relayStore{claims: []outboxClaim{{Event: event}}}
	publisher := publisherFunc(func(context.Context, Event) error { return nil })
	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	if store.published != 1 || store.failed != 0 {
		t.Fatalf("published=%d failed=%d, want published=1 failed=0", store.published, store.failed)
	}
	if store.batch != 1 || store.lease != defaultClaimTTL {
		t.Fatalf("claim batch=%d lease=%s, want batch=1 lease=%s", store.batch, store.lease, defaultClaimTTL)
	}
}

func TestRelayRecordsPublishFailureForRetry(t *testing.T) {
	store := &relayStore{claims: []outboxClaim{{Event: testEvent()}}}
	publisher := publisherFunc(func(context.Context, Event) error { return errors.New("broker unavailable") })
	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	if store.published != 0 || store.failed != 1 {
		t.Fatalf("published=%d failed=%d, want published=0 failed=1", store.published, store.failed)
	}
}

func TestInsertOutboxPersistsPropagationCarrierInDomainTransaction(t *testing.T) {
	original := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(original) })

	wantTraceID := trace.TraceID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	spanContext := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    wantTraceID,
		SpanID:     trace.SpanID{0, 1, 2, 3, 4, 5, 6, 7},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), spanContext)
	tx := &outboxTx{}

	if err := InsertOutbox(ctx, tx, testEvent()); err != nil {
		t.Fatal(err)
	}
	if len(tx.args) != 9 {
		t.Fatalf("outbox insert arguments=%d, want propagation carrier in the same insert", len(tx.args))
	}
	raw, ok := tx.args[8].([]byte)
	if !ok {
		t.Fatalf("propagation carrier type=%T, want []byte JSON", tx.args[8])
	}
	var carrier map[string]string
	if err := json.Unmarshal(raw, &carrier); err != nil {
		t.Fatalf("decode propagation carrier: %v", err)
	}
	if got := carrier["traceparent"]; got != "00-000102030405060708090a0b0c0d0e0f-0001020304050607-01" {
		t.Fatalf("traceparent=%q, want persisted W3C carrier", got)
	}
}

func TestRelayRestoresPersistedTraceContextBeforePublishing(t *testing.T) {
	original := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(original) })

	store := &relayStore{claims: []outboxClaim{{
		Event: testEvent(),
		Propagation: propagation.MapCarrier{
			"traceparent": "00-000102030405060708090a0b0c0d0e0f-0001020304050607-01",
		},
	}}}
	var publishedTraceID trace.TraceID
	publisher := publisherFunc(func(ctx context.Context, _ Event) error {
		publishedTraceID = trace.SpanContextFromContext(ctx).TraceID()
		return nil
	})

	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{BatchSize: 1}); err != nil {
		t.Fatal(err)
	}
	if want := (trace.TraceID{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}); publishedTraceID != want {
		t.Fatalf("published trace ID=%s, want %s from durable outbox carrier", publishedTraceID, want)
	}
}

func TestRelayEmitsOldestQueuedEventAgeAndReusesCollector(t *testing.T) {
	registry := prometheus.NewRegistry()
	store := &relayStore{oldestAge: 12.5}
	config := RelayConfig{Service: "order", Registry: registry}
	publisher := publisherFunc(func(context.Context, Event) error { return nil })

	if _, err := relayOnce(context.Background(), store, publisher, config); err != nil {
		t.Fatal(err)
	}
	store.oldestAge = 3.25
	if _, err := relayOnce(context.Background(), store, publisher, config); err != nil {
		t.Fatal(err)
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var family *dto.MetricFamily
	for _, candidate := range families {
		if candidate.GetName() == "outbox_oldest_age_seconds" {
			family = candidate
			break
		}
	}
	if family == nil || len(family.Metric) != 1 {
		t.Fatalf("outbox_oldest_age_seconds family=%v, want one series", family)
	}
	if got := family.Metric[0].GetGauge().GetValue(); got != 3.25 {
		t.Fatalf("outbox_oldest_age_seconds=%v, want updated value 3.25", got)
	}
	var hasService bool
	for _, label := range family.Metric[0].Label {
		hasService = hasService || label.GetName() == "service" && label.GetValue() == "order"
	}
	if !hasService {
		t.Fatal("outbox_oldest_age_seconds missing service=order")
	}
}

func TestProcessDeliveryAcksOnlyAfterCommit(t *testing.T) {
	tx := &inboxTx{}
	delivery := &testDelivery{}
	if err := ProcessDelivery(context.Background(), tx, "projection", testEvent(), func() error { return nil }, delivery, RetryPolicy{MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	if !tx.committed || delivery.acks != 1 || delivery.nacks != 0 || delivery.rejects != 0 {
		t.Fatalf("committed=%v acks=%d nacks=%d rejects=%d", tx.committed, delivery.acks, delivery.nacks, delivery.rejects)
	}
}

func TestProcessDeliveryRejectsNilTransactionWithoutPanicking(t *testing.T) {
	delivery := &testDelivery{}
	err := ProcessDelivery(context.Background(), nil, "projection", testEvent(), func() error { return nil }, delivery, RetryPolicy{MaxAttempts: 3})
	if err == nil {
		t.Fatal("ProcessDelivery() error = nil, want error")
	}
	if delivery.nacks != 1 || delivery.requeue {
		t.Fatalf("nacks=%d requeue=%v, want one non-requeue retry", delivery.nacks, delivery.requeue)
	}
}

func TestProcessDeliveryNeverRequeuesAndClassifiesTerminalFailures(t *testing.T) {
	tests := []struct {
		name       string
		handlerErr error
		attempts   int
		wantNack   int
		wantReject int
	}{
		{name: "retryable", handlerErr: errors.New("temporary"), attempts: 0, wantNack: 1},
		{name: "retry limit", handlerErr: errors.New("temporary"), attempts: 3, wantReject: 1},
		{name: "non retryable", handlerErr: NonRetryable(errors.New("invalid event")), attempts: 0, wantReject: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx := &inboxTx{}
			delivery := &testDelivery{attempts: test.attempts}
			err := ProcessDelivery(context.Background(), tx, "projection", testEvent(), func() error { return test.handlerErr }, delivery, RetryPolicy{MaxAttempts: 3})
			if !errors.Is(err, test.handlerErr) {
				t.Fatalf("ProcessDelivery() error = %v, want %v", err, test.handlerErr)
			}
			if delivery.nacks != test.wantNack || delivery.rejects != test.wantReject || delivery.requeue {
				t.Fatalf("nacks=%d rejects=%d requeue=%v", delivery.nacks, delivery.rejects, delivery.requeue)
			}
		})
	}
}

func TestProcessDeliveryRejectsMalformedEventIDWithoutInboxWrite(t *testing.T) {
	tx := &inboxTx{}
	delivery := &testDelivery{}
	event := testEvent()
	event.ID = "not-a-uuid"

	err := ProcessDelivery(context.Background(), tx, "projection", event, func() error {
		t.Fatal("handler called for malformed event ID")
		return nil
	}, delivery, RetryPolicy{MaxAttempts: 3})
	if err == nil {
		t.Fatal("ProcessDelivery() error = nil, want malformed-ID error")
	}
	if tx.execCalls != 0 || !tx.rolledBack || delivery.rejects != 1 || delivery.nacks != 0 {
		t.Fatalf("execCalls=%d rolledBack=%v rejects=%d nacks=%d", tx.execCalls, tx.rolledBack, delivery.rejects, delivery.nacks)
	}
}

func TestAMQPDeliveryXDeathCountBoundsRetry(t *testing.T) {
	delivery := AMQPDelivery{Delivery: amqp.Delivery{Headers: amqp.Table{
		"x-death": []interface{}{
			amqp.Table{"count": int64(2), "queue": "order.saga.retry", "reason": "expired"},
			amqp.Table{"count": int32(1), "queue": "order.saga.retry", "reason": "expired"},
			amqp.Table{"count": int64(99), "queue": "other.retry", "reason": "rejected"},
			amqp.Table{"count": int64(50), "queue": "other.retry", "reason": "expired"},
		},
	}}}
	if got := delivery.RetryCountFor("order.saga.retry", "expired"); got != 3 {
		t.Fatalf("RetryCountFor()=%d, want 3", got)
	}
	if retryable(errors.New("temporary"), delivery.RetryCountFor("order.saga.retry", "expired"), RetryPolicy{MaxAttempts: 3}) {
		t.Fatal("retryable()=true at x-death retry limit")
	}
}

type inboxTx struct {
	pgx.Tx
	seen       map[string]struct{}
	execCalls  int
	committed  bool
	rolledBack bool
}

func (tx *inboxTx) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	tx.execCalls++
	if tx.seen == nil {
		tx.seen = make(map[string]struct{})
	}
	key := fmt.Sprint(args[0], "/", args[1])
	rows := int64(1)
	if _, exists := tx.seen[key]; exists {
		rows = 0
	} else {
		tx.seen[key] = struct{}{}
	}
	return pgconn.NewCommandTag(fmt.Sprintf("INSERT 0 %d", rows)), nil
}

func (tx *inboxTx) Commit(context.Context) error   { tx.committed = true; return nil }
func (tx *inboxTx) Rollback(context.Context) error { tx.rolledBack = true; return nil }

type outboxTx struct {
	pgx.Tx
	query string
	args  []any
}

func (tx *outboxTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	tx.query = query
	tx.args = append([]any(nil), args...)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

type relayStore struct {
	claims        []outboxClaim
	published     int
	failed        int
	failureErrors []error
	batch         int
	lease         time.Duration
	oldestAge     float64
}

func (store *relayStore) Claim(_ context.Context, batch int, lease time.Duration) ([]outboxClaim, error) {
	store.batch = batch
	store.lease = lease
	return store.claims, nil
}
func (store *relayStore) MarkPublished(context.Context, outboxClaim) error {
	store.published++
	return nil
}
func (store *relayStore) MarkFailed(_ context.Context, _ outboxClaim, err error) error {
	store.failed++
	store.failureErrors = append(store.failureErrors, err)
	return nil
}
func (store *relayStore) OldestAge(context.Context) (float64, error) {
	return store.oldestAge, nil
}

type publisherFunc func(context.Context, Event) error

func (publish publisherFunc) Publish(ctx context.Context, event Event) error {
	return publish(ctx, event)
}

type testDelivery struct {
	attempts int
	acks     int
	nacks    int
	rejects  int
	requeue  bool
}

func (delivery *testDelivery) RetryCount() int { return delivery.attempts }
func (delivery *testDelivery) Ack(bool) error  { delivery.acks++; return nil }
func (delivery *testDelivery) Nack(_ bool, requeue bool) error {
	delivery.nacks++
	delivery.requeue = requeue
	return nil
}
func (delivery *testDelivery) Reject(_ bool) error { delivery.rejects++; return nil }
