package messaging

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEnvelopeRoundTripPreservesWorkflowMetadata(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 30, 0, 0, time.FixedZone("test", 8*60*60))
	want := NewEnvelope("payment.initiated.v1", "correlation-1", json.RawMessage(`{"amount_minor":1200}`), func() time.Time {
		return now
	})
	want.WorkflowID = "workflow-1"
	want.ActionName = "book-transfer"
	want.CommandID = "command-1"
	want.IdempotencyKey = "payment-1"
	want.CausationID = "cause-1"

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Envelope
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip=%#v, want %#v", got, want)
	}
}

func TestNewEnvelopeUsesSchemaVersionGeneratedIDAndUTCClock(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 30, 0, 0, time.FixedZone("test", 8*60*60))
	envelope := NewEnvelope("payment.initiated.v1", "correlation-1", json.RawMessage(`{"ok":true}`), func() time.Time {
		return now
	})

	if envelope.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion=%d, want 1", envelope.SchemaVersion)
	}
	if !validMessageID(envelope.MessageID) {
		t.Fatalf("MessageID=%q, want UUID", envelope.MessageID)
	}
	if !envelope.OccurredAt.Equal(now.UTC()) || envelope.OccurredAt.Location() != time.UTC {
		t.Fatalf("OccurredAt=%v, want %v in UTC", envelope.OccurredAt, now.UTC())
	}
	if envelope.MessageType != "payment.initiated.v1" || envelope.CorrelationID != "correlation-1" {
		t.Fatalf("envelope=%#v", envelope)
	}
}
