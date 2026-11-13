package messaging

import (
	"context"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestAMQPDeliveryRetryCountDefaultsReason(t *testing.T) {
	// With no x-death header the count is zero regardless of reason defaults.
	delivery := AMQPDelivery{Delivery: amqp.Delivery{}, RetryQueue: "svc.retry"}
	if got := delivery.RetryCount(); got != 0 {
		t.Fatalf("RetryCount=%d, want 0 without x-death", got)
	}

	// The default reason ("expired") filters out x-death entries recorded for
	// other reasons such as rejected.
	delivery.Headers = amqp.Table{"x-death": []any{
		amqp.Table{"queue": "svc.retry", "reason": "rejected", "count": int64(2)},
	}}
	if got := delivery.RetryCount(); got != 0 {
		t.Fatalf("RetryCount=%d, want 0 for non-expired reason", got)
	}
	delivery.Headers = amqp.Table{"x-death": []any{
		amqp.Table{"queue": "svc.retry", "reason": "expired", "count": int64(2)},
	}}
	if got := delivery.RetryCount(); got != 2 {
		t.Fatalf("RetryCount=%d, want 2", got)
	}
}

func TestAMQPDeliveryRetryCountForEdgeCases(t *testing.T) {
	delivery := AMQPDelivery{}
	if got := delivery.RetryCountFor("q", "expired"); got != 0 {
		t.Fatalf("no header count=%d", got)
	}
	if got := (&AMQPDelivery{Delivery: amqp.Delivery{
		Headers: amqp.Table{"x-death": "not-a-list"},
	}}).RetryCountFor("q", "expired"); got != 0 {
		t.Fatalf("non-list x-death count=%d", got)
	}
	// Non-table entries are skipped; int/int32/int64 counts all accumulate.
	delivery = AMQPDelivery{Delivery: amqp.Delivery{
		Headers: amqp.Table{"x-death": []any{
			"bogus",
			amqp.Table{"queue": "q", "reason": "expired", "count": 1},
			amqp.Table{"queue": "q", "reason": "expired", "count": int32(2)},
			amqp.Table{"queue": "q", "reason": "expired", "count": int64(3)},
			amqp.Table{"queue": "q", "reason": "expired", "count": "4"},
		}},
	}}
	if got := delivery.RetryCountFor("q", "expired"); got != 6 {
		t.Fatalf("mixed counts=%d, want 6 (int+int32+int64)", got)
	}
	// An explicit queue filter skips other queues.
	if got := delivery.RetryCountFor("other", "expired"); got != 0 {
		t.Fatalf("other queue count=%d, want 0", got)
	}
	// Without a queue only .retry-suffixed queues count.
	noQueue := AMQPDelivery{Delivery: amqp.Delivery{
		Headers: amqp.Table{"x-death": []any{
			amqp.Table{"queue": "svc.main", "reason": "expired", "count": 9},
			amqp.Table{"queue": "svc.retry", "reason": "expired", "count": 4},
			amqp.Table{"reason": "expired", "count": 8},
		}},
	}}
	if got := noQueue.RetryCountFor("", "expired"); got != 4 {
		t.Fatalf("retry-suffix inference=%d, want 4", got)
	}
	// An empty reason matches every reason.
	all := AMQPDelivery{Delivery: amqp.Delivery{
		Headers: amqp.Table{"x-death": []any{
			amqp.Table{"queue": "q", "reason": "expired", "count": 2},
			amqp.Table{"queue": "q", "reason": "rejected", "count": 3},
		}},
	}}
	if got := all.RetryCountFor("q", ""); got != 5 {
		t.Fatalf("empty reason count=%d, want 5", got)
	}
}

func TestProcessRabbitDeliveryForRetryQueueRejectsBadInput(t *testing.T) {
	tx := &inboxTx{}
	acknowledger := &amqpRejectStub{}
	delivery := amqp.Delivery{Acknowledger: acknowledger, Body: []byte(`{`)}
	if err := ProcessRabbitDeliveryForRetryQueue(context.Background(), tx, "c",
		delivery, "", func(context.Context, Event) error { return nil }, RetryPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "decode event envelope") {
		t.Fatalf("malformed body error=%v", err)
	}
	if acknowledger.rejects != 1 {
		t.Fatalf("rejects=%d, want malformed deliveries rejected to the DLQ", acknowledger.rejects)
	}

	// A nil handler is rejected before the inbox write.
	delivery = amqp.Delivery{Acknowledger: acknowledger, Body: []byte(
		`{"id":"` + validUUIDForTests() + `","type":"t","subject":"s","schema_version":1,"data":{}}`)}
	if err := ProcessRabbitDeliveryForRetryQueue(context.Background(), tx, "c",
		delivery, "", nil, RetryPolicy{}); err == nil ||
		!strings.Contains(err.Error(), "handler is nil") {
		t.Fatalf("nil handler error=%v", err)
	}

	// A nil context is tolerated (defaults to Background).
	if err := ProcessRabbitDeliveryForRetryQueue(nil, tx, "c",
		amqp.Delivery{Acknowledger: acknowledger, Body: []byte(`not json`)},
		"", func(context.Context, Event) error { return nil }, RetryPolicy{}); err == nil {
		t.Fatal("nil ctx with malformed body must still fail")
	}
}

// amqpRejectStub adapts the AMQP acknowledger interface for delivery tests.
type amqpRejectStub struct{ rejects int }

func (acknowledger *amqpRejectStub) Ack(uint64, bool) error   { return nil }
func (acknowledger *amqpRejectStub) Nack(uint64, bool, bool) error { return nil }
func (acknowledger *amqpRejectStub) Reject(uint64, bool) error {
	acknowledger.rejects++
	return nil
}
