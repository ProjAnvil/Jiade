// Package messaging provides domain-neutral, at-least-once event delivery.
package messaging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	count, err := randomRead(raw[:])
	if err != nil {
		panic(fmt.Errorf("generate event UUID: %w", err))
	}
	if count != len(raw) {
		panic(fmt.Sprintf("generate event UUID: read %d bytes, want %d", count, len(raw)))
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

var randomRead = rand.Read

func validEventID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	compact := id[0:8] + id[9:13] + id[14:18] + id[19:23] + id[24:36]
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16
}
