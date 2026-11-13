package messaging

// Table tests for validateEnvelope's rejection branches. The happy path is
// covered indirectly by every ProcessDelivery test; these cases pin the
// exact validation order and error messages.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validEnvelope() Envelope {
	return Envelope{
		MessageID:     "12345678-1234-1234-1234-123456789abc",
		MessageType:   "risk.payment-authorized.v1",
		SchemaVersion: CurrentSchemaVersion,
		CorrelationID: "corr-1",
		OccurredAt:    time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
		Payload:       json.RawMessage(`{}`),
	}
}

func TestValidateEnvelope_Table(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Envelope)
		wantErr string
	}{
		{"valid", func(*Envelope) {}, ""},
		{"bad message id", func(e *Envelope) { e.MessageID = "not-a-uuid" }, "message_id must be a UUID"},
		{"empty message type", func(e *Envelope) { e.MessageType = "" }, "message_type is required"},
		{"wrong schema version", func(e *Envelope) { e.SchemaVersion = CurrentSchemaVersion + 1 }, "unsupported schema_version"},
		{"missing correlation id", func(e *Envelope) { e.CorrelationID = "" }, "correlation_id is required"},
		{"zero occurred at", func(e *Envelope) { e.OccurredAt = time.Time{} }, "occurred_at is required"},
		{"invalid payload json", func(e *Envelope) { e.Payload = json.RawMessage("{not-json") }, "payload must be valid JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnvelope()
			tc.mutate(&env)
			err := validateEnvelope(env)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidMessageID_MalformedVariants(t *testing.T) {
	for _, id := range []string{
		"",                                   // too short
		"12345678123412341234123456789abc",   // missing dashes
		"12345678-1234-1234-1234-123456789zz", // non-hex digits
		"12345678-1234-1234-1234-123456789ab",  // too short tail
	} {
		if validMessageID(id) {
			t.Errorf("validMessageID(%q) = true, want false", id)
		}
	}
	if !validMessageID("123e4567-e89b-12d3-a456-426614174000") {
		t.Error("validMessageID must accept a canonical UUID")
	}
}
