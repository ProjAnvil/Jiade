package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	if event.SchemaVersion != CurrentSchemaVersion || !event.OccurredAt.Equal(fixedClock()) || event.ID == "" {
		t.Fatalf("event = %#v", event)
	}
}

func TestHandleOnceSkipsDuplicateEventForSameConsumer(t *testing.T) {
	tx := &inboxTx{rows: []int64{1, 0}}
	calls := 0
	for range 2 {
		if err := HandleOnce(context.Background(), tx, "inventory-projection", testEvent(), func() error {
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
	tx := &inboxTx{rows: []int64{1, 1}}
	calls := 0
	for _, consumer := range []string{"inventory-projection", "analytics"} {
		if err := HandleOnce(context.Background(), tx, consumer, testEvent(), func() error {
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

func TestPublishOutcomeRejectsReturnedOrNegativelyConfirmedMessage(t *testing.T) {
	tests := []struct {
		name     string
		confirm  bool
		returned bool
		wantErr  bool
	}{
		{name: "confirmed", confirm: true},
		{name: "nack", confirm: false, wantErr: true},
		{name: "returned", confirm: true, returned: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			confirms := make(chan bool, 1)
			returns := make(chan error, 1)
			confirms <- test.confirm
			if test.returned {
				returns <- errors.New("unroutable")
			}
			err := awaitPublishOutcome(context.Background(), confirms, returns)
			if (err != nil) != test.wantErr {
				t.Fatalf("awaitPublishOutcome() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestProcessDeliveryAcksOnlyAfterCommit(t *testing.T) {
	tx := &inboxTx{rows: []int64{1}}
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
			tx := &inboxTx{rows: []int64{1}}
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

type inboxTx struct {
	pgx.Tx
	rows       []int64
	committed  bool
	rolledBack bool
}

func (tx *inboxTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	rows := tx.rows[0]
	tx.rows = tx.rows[1:]
	return pgconn.NewCommandTag("INSERT 0 " + string(rune('0'+rows))), nil
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
