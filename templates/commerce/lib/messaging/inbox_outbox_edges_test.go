package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestHandleOnceValidatesInputs(t *testing.T) {
	event := testEvent()
	handler := func() error { return nil }
	tests := []struct {
		name     string
		tx       pgx.Tx
		consumer string
		event    Event
		handler  func() error
		wantMsg  string
	}{
		{name: "nil transaction", consumer: "c", event: event, handler: handler, wantMsg: "transaction is nil"},
		{name: "empty consumer", tx: &inboxTx{}, consumer: "", event: event, handler: handler, wantMsg: "consumer is required"},
		{name: "empty event id", tx: &inboxTx{}, consumer: "c", event: Event{}, handler: handler, wantMsg: "event ID is required"},
		{
			name: "non-uuid event id", tx: &inboxTx{}, consumer: "c",
			event:   Event{ID: "not-a-uuid", Type: "t", Subject: "s", Data: []byte(`{}`)},
			handler: handler, wantMsg: "must be a UUID",
		},
		{name: "nil handler", tx: &inboxTx{}, consumer: "c", event: event, wantMsg: "handler is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := HandleOnce(context.Background(), test.tx, test.consumer, test.event, test.handler)
			if err == nil || !strings.Contains(err.Error(), test.wantMsg) {
				t.Fatalf("HandleOnce error=%v, want %q", err, test.wantMsg)
			}
		})
	}
}

func TestHandleOncePropagatesExecFailure(t *testing.T) {
	execErr := errors.New("connection reset")
	tx := &execErrorTx{err: execErr}
	err := HandleOnce(context.Background(), tx, "c", testEvent(), func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "record inbox event") {
		t.Fatalf("HandleOnce error=%v, want wrapped exec failure", err)
	}
}

func TestHandleOnceWrapsHandlerError(t *testing.T) {
	tx := &inboxTx{}
	cause := errors.New("domain rejected")
	err := HandleOnce(context.Background(), tx, "c", testEvent(), func() error { return cause })
	if err == nil || !strings.Contains(err.Error(), "handle inbox event") || !errors.Is(err, cause) {
		t.Fatalf("HandleOnce error=%v, want wrapped handler failure", err)
	}
}

func TestNonRetryableWrapsAndUnwraps(t *testing.T) {
	if NonRetryable(nil) != nil {
		t.Fatal("NonRetryable(nil) must be nil")
	}
	cause := errors.New("bad payload")
	wrapped := NonRetryable(cause)
	if wrapped == nil || wrapped.Error() != "bad payload" {
		t.Fatalf("NonRetryable error=%v", wrapped)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is must see through the wrapper")
	}
	if !retryable(wrapped, 0, RetryPolicy{MaxAttempts: 3}) == false {
		t.Fatal("non-retryable errors must not be retryable")
	}
}

func TestRetryableDefaultsMaxAttempts(t *testing.T) {
	if !retryable(errors.New("transient"), 2, RetryPolicy{}) {
		t.Fatal("default max attempts is 3, attempt 3 of a 0-based count 2 is retryable")
	}
	if retryable(errors.New("transient"), 3, RetryPolicy{}) {
		t.Fatal("retry count at the default budget must stop retrying")
	}
}

func TestProcessDeliveryRequiresDelivery(t *testing.T) {
	if err := ProcessDelivery(context.Background(), &inboxTx{}, "c", testEvent(),
		func() error { return nil }, nil, RetryPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "delivery is nil") {
		t.Fatalf("ProcessDelivery error=%v, want nil delivery guard", err)
	}
}

func TestSettleFailureSurfacesSettlementErrors(t *testing.T) {
	cause := errors.New("handler exploded")
	// A retryable failure whose Nack fails surfaces both.
	nackFailure := &settlementErrorDelivery{nackErr: errors.New("channel closed")}
	err := settleFailure(cause, nackFailure, RetryPolicy{MaxAttempts: 3})
	if err == nil || !strings.Contains(err.Error(), "retry delivery after") {
		t.Fatalf("settleFailure(nack error)=%v", err)
	}
	// A terminal failure whose Reject fails surfaces both.
	rejectFailure := &settlementErrorDelivery{rejectErr: errors.New("channel closed")}
	err = settleFailure(NonRetryable(cause), rejectFailure, RetryPolicy{MaxAttempts: 3})
	if err == nil || !strings.Contains(err.Error(), "reject delivery after") {
		t.Fatalf("settleFailure(reject error)=%v", err)
	}
}

func TestProcessDeliveryCommitFailureSettlesAndRollsBack(t *testing.T) {
	tx := &commitErrorTx{inboxTx: inboxTx{}}
	delivery := &testDelivery{}
	err := ProcessDelivery(context.Background(), tx, "c", testEvent(),
		func() error { return nil }, delivery, RetryPolicy{MaxAttempts: 3})
	if err == nil || !strings.Contains(err.Error(), "commit inbox transaction") {
		t.Fatalf("ProcessDelivery error=%v, want commit failure", err)
	}
	if !tx.rolledBack {
		t.Fatal("commit failure must roll the transaction back")
	}
	if delivery.nacks != 1 {
		t.Fatalf("nacks=%d, want retryable settlement", delivery.nacks)
	}
}

func TestProcessDeliveryAckFailureSurfaces(t *testing.T) {
	tx := &inboxTx{}
	delivery := &ackErrorDelivery{}
	err := ProcessDelivery(context.Background(), tx, "c", testEvent(),
		func() error { return nil }, delivery, RetryPolicy{})
	if err == nil || !strings.Contains(err.Error(), "ack delivery") {
		t.Fatalf("ProcessDelivery error=%v, want ack failure", err)
	}
	if !tx.committed {
		t.Fatal("transaction must commit before the ack")
	}
}

func TestInsertOutboxValidatesInputs(t *testing.T) {
	valid := testEvent()
	if err := InsertOutbox(context.Background(), nil, valid); err == nil ||
		!strings.Contains(err.Error(), "transaction is nil") {
		t.Fatalf("InsertOutbox(nil tx) error=%v", err)
	}
	badID := valid
	badID.ID = "not-a-uuid"
	if err := InsertOutbox(context.Background(), &outboxTx{}, badID); err == nil ||
		!strings.Contains(err.Error(), "must be a UUID") {
		t.Fatalf("InsertOutbox(bad ID) error=%v", err)
	}
	for name, mutate := range map[string]func(*Event){
		"missing type":       func(event *Event) { event.Type = "" },
		"missing subject":    func(event *Event) { event.Subject = "" },
		"missing data":       func(event *Event) { event.Data = nil },
		"zero schema version": func(event *Event) { event.SchemaVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			event := testEvent()
			event.ID = validUUIDForTests()
			mutate(&event)
			if err := InsertOutbox(context.Background(), &outboxTx{}, event); err == nil ||
				!strings.Contains(err.Error(), "incomplete") {
				t.Fatalf("InsertOutbox error=%v, want incomplete event", err)
			}
		})
	}
}

func TestInsertOutboxPropagatesExecFailure(t *testing.T) {
	event := testEvent()
	event.ID = validUUIDForTests()
	err := InsertOutbox(context.Background(), &execErrorTx{err: errors.New("pool exhausted")}, event)
	if err == nil || !strings.Contains(err.Error(), "insert outbox event") {
		t.Fatalf("InsertOutbox error=%v, want wrapped exec failure", err)
	}
}

func TestRunRelayRequiresPoolAndPublisher(t *testing.T) {
	publisher := publisherFunc(func(context.Context, Event) error { return nil })
	if err := RunRelay(context.Background(), nil, publisher, RelayConfig{}); err == nil ||
		!strings.Contains(err.Error(), "pool is nil") {
		t.Fatalf("RunRelay(nil pool) error=%v", err)
	}
	if err := RunRelay(context.Background(), nil, nil, RelayConfig{}); err == nil {
		t.Fatal("nil pool with nil publisher must still fail on the pool first")
	}
}

func TestRelayOnceValidatesDependencies(t *testing.T) {
	publisher := publisherFunc(func(context.Context, Event) error { return nil })
	store := &relayStore{}
	if _, err := relayOnce(context.Background(), nil, publisher, RelayConfig{}); err == nil ||
		!strings.Contains(err.Error(), "dependency is nil") {
		t.Fatalf("relayOnce(nil store) error=%v", err)
	}
	if _, err := relayOnce(context.Background(), store, nil, RelayConfig{}); err == nil {
		t.Fatal("relayOnce(nil publisher) must fail")
	}
}

func TestRelayOnceClaimFailureStopsBatch(t *testing.T) {
	store := &claimErrorStore{}
	publisher := publisherFunc(func(context.Context, Event) error { return nil })
	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{}); err == nil ||
		!strings.Contains(err.Error(), "claim outbox events") {
		t.Fatalf("relayOnce error=%v, want claim failure", err)
	}
}

func TestRelayOnceMarkPublishedFailureSurfaces(t *testing.T) {
	event := testEvent()
	event.ID = validUUIDForTests()
	store := &markResultStore{claims: []outboxClaim{{Event: event}}, markPublishedErr: errors.New("mark failed")}
	publisher := publisherFunc(func(context.Context, Event) error { return nil })
	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{}); err == nil ||
		!strings.Contains(err.Error(), "mark outbox event published") {
		t.Fatalf("relayOnce error=%v, want mark-published failure", err)
	}
}

func TestRelayOnceTerminalFailureWithRecordFailureJoins(t *testing.T) {
	event := testEvent()
	event.ID = validUUIDForTests()
	store := &markResultStore{claims: []outboxClaim{{Event: event}}, markFailedErr: errors.New("db down")}
	publisher := publisherFunc(func(context.Context, Event) error {
		return ErrPublisherUnavailable
	})
	_, err := relayOnce(context.Background(), store, publisher, RelayConfig{})
	if err == nil ||
		!strings.Contains(err.Error(), ErrPublisherUnavailable.Error()) ||
		!strings.Contains(err.Error(), "record outbox publish failure") {
		t.Fatalf("relayOnce error=%v, want joined publish+record failures", err)
	}
}

func TestRelayOnceTransientFailureContinuesBatch(t *testing.T) {
	first := testEvent()
	first.ID = validUUIDForTests()
	second := first
	second.ID = validUUIDForTests()
	store := &markResultStore{claims: []outboxClaim{{Event: first}, {Event: second}}}
	published := 0
	publisher := publisherFunc(func(context.Context, Event) error {
		published++
		if published == 1 {
			return errors.New("transient broker hiccup")
		}
		return nil
	})
	claimed, err := relayOnce(context.Background(), store, publisher, RelayConfig{})
	if err != nil {
		t.Fatalf("relayOnce error: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("claimed=%d, want both claims processed", claimed)
	}
	if store.failed != 1 || store.published != 1 {
		t.Fatalf("failed=%d published=%d, want one failure and one publish", store.failed, store.published)
	}
}

func TestRelayOnceTransientFailureRecordFailureStopsBatch(t *testing.T) {
	event := testEvent()
	event.ID = validUUIDForTests()
	store := &markResultStore{claims: []outboxClaim{{Event: event}}, markFailedErr: errors.New("db down")}
	publisher := publisherFunc(func(context.Context, Event) error {
		return errors.New("transient")
	})
	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{}); err == nil ||
		!strings.Contains(err.Error(), "record outbox publish failure") {
		t.Fatalf("relayOnce error=%v, want record failure to stop the batch", err)
	}
}

// validUUIDForTests returns a fixed valid UUID so event-ID guards pass.
func validUUIDForTests() string {
	return "11111111-2222-4333-8444-555555555555"
}

// --- error-injecting fakes ---

// execErrorTx fails every Exec.
type execErrorTx struct {
	pgx.Tx
	err error
}

func (tx *execErrorTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, tx.err
}

// commitErrorTx fails Commit.
type commitErrorTx struct {
	inboxTx
}

func (tx *commitErrorTx) Commit(context.Context) error {
	return errors.New("commit failed")
}

// settlementErrorDelivery fails settlement calls.
type settlementErrorDelivery struct {
	nackErr   error
	rejectErr error
}

func (delivery *settlementErrorDelivery) RetryCount() int { return 0 }
func (delivery *settlementErrorDelivery) Ack(bool) error  { return nil }
func (delivery *settlementErrorDelivery) Nack(bool, bool) error {
	return delivery.nackErr
}
func (delivery *settlementErrorDelivery) Reject(bool) error { return delivery.rejectErr }

// ackErrorDelivery fails the final Ack.
type ackErrorDelivery struct{}

func (delivery *ackErrorDelivery) RetryCount() int { return 0 }
func (delivery *ackErrorDelivery) Ack(bool) error  { return errors.New("ack lost") }
func (delivery *ackErrorDelivery) Nack(bool, bool) error { return nil }
func (delivery *ackErrorDelivery) Reject(bool) error     { return nil }

// claimErrorStore fails Claim.
type claimErrorStore struct{}

func (store *claimErrorStore) Claim(context.Context, int, time.Duration) ([]outboxClaim, error) {
	return nil, errors.New("claim exploded")
}
func (store *claimErrorStore) MarkPublished(context.Context, outboxClaim) error { return nil }
func (store *claimErrorStore) MarkFailed(context.Context, outboxClaim, error) error {
	return nil
}
func (store *claimErrorStore) OldestAge(context.Context) (float64, error) { return 0, nil }

// markResultStore is a relayStore with injectable mark errors.
type markResultStore struct {
	claims         []outboxClaim
	published      int
	failed         int
	markPublishedErr error
	markFailedErr  error
}

func (store *markResultStore) Claim(context.Context, int, time.Duration) ([]outboxClaim, error) {
	return store.claims, nil
}
func (store *markResultStore) MarkPublished(context.Context, outboxClaim) error {
	store.published++
	return store.markPublishedErr
}
func (store *markResultStore) MarkFailed(_ context.Context, _ outboxClaim, err error) error {
	store.failed++
	return store.markFailedErr
}
func (store *markResultStore) OldestAge(context.Context) (float64, error) { return 0, nil }
