// Package messaging provides domain-neutral, at-least-once event delivery.
package messaging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// CurrentSchemaVersion is the version of the envelope, not of an event type.
const CurrentSchemaVersion = 1

// Event is the stable, schema-versioned envelope transported between services.
// Data is event-type-specific JSON owned by the producing domain.
type Event struct {
	ID            string          `json:"id"`
	SchemaVersion int             `json:"schema_version"`
	Type          string          `json:"type"`
	Subject       string          `json:"subject"`
	OccurredAt    time.Time       `json:"occurred_at"`
	CorrelationID string          `json:"correlation_id"`
	CausationID   string          `json:"causation_id,omitempty"`
	Data          json.RawMessage `json:"data"`
}

// NewEvent constructs an envelope with a generated event ID. The clock is
// injected to make event timestamps deterministic at call sites and in tests.
func NewEvent(eventType, subject, correlationID, causationID string, data json.RawMessage, clock func() time.Time) Event {
	if clock == nil {
		clock = time.Now
	}
	return Event{
		ID:            newEventID(),
		SchemaVersion: CurrentSchemaVersion,
		Type:          eventType,
		Subject:       subject,
		OccurredAt:    clock().UTC(),
		CorrelationID: correlationID,
		CausationID:   causationID,
		Data:          append(json.RawMessage(nil), data...),
	}
}

func newEventID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failures are exceptional; retain a unique-enough fallback
		// rather than preventing a caller from recording its transaction.
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
