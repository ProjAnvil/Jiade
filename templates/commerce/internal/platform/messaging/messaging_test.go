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
	amqp "github.com/rabbitmq/amqp091-go"
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
			amqp.Table{"count": int64(2)},
			amqp.Table{"count": int32(1)},
		},
	}}}
	if got := delivery.RetryCount(); got != 3 {
		t.Fatalf("RetryCount()=%d, want 3", got)
	}
	if retryable(errors.New("temporary"), delivery.RetryCount(), RetryPolicy{MaxAttempts: 3}) {
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

type relayStore struct {
	claims    []outboxClaim
	published int
	failed    int
	batch     int
	lease     time.Duration
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
func (store *relayStore) MarkFailed(context.Context, outboxClaim, error) error {
	store.failed++
	return nil
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
