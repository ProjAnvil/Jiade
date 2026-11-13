// Package messaging provides domain-neutral, at-least-once bank messaging.
package messaging

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// CurrentSchemaVersion is the transport envelope schema version.
const CurrentSchemaVersion = 1

// Envelope is the stable, schema-versioned message transported between bank
// services.
type Envelope struct {
	MessageID      string          `json:"message_id"`
	MessageType    string          `json:"message_type"`
	SchemaVersion  int             `json:"schema_version"`
	WorkflowID     string          `json:"workflow_id,omitempty"`
	ActionName     string          `json:"action_name,omitempty"`
	CommandID      string          `json:"command_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CorrelationID  string          `json:"correlation_id"`
	CausationID    string          `json:"causation_id,omitempty"`
	OccurredAt     time.Time       `json:"occurred_at"`
	Payload        json.RawMessage `json:"payload"`
}

// NewEnvelope creates an envelope with a generated message ID.
func NewEnvelope(messageType, correlationID string, payload json.RawMessage, now func() time.Time) Envelope {
	if now == nil {
		now = time.Now
	}
	return Envelope{
		MessageID:     newMessageID(),
		MessageType:   messageType,
		SchemaVersion: CurrentSchemaVersion,
		CorrelationID: correlationID,
		OccurredAt:    now().UTC(),
		Payload:       append(json.RawMessage(nil), payload...),
	}
}

func newMessageID() string {
	var raw [16]byte
	count, err := rand.Read(raw[:])
	if err != nil {
		panic(fmt.Errorf("generate message UUID: %w", err))
	}
	if count != len(raw) {
		panic(fmt.Sprintf("generate message UUID: read %d bytes, want %d", count, len(raw)))
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func validMessageID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	compact := id[0:8] + id[9:13] + id[14:18] + id[19:23] + id[24:36]
	decoded, err := hex.DecodeString(compact)
	return err == nil && len(decoded) == 16
}

func validateEnvelope(envelope Envelope) error {
	switch {
	case !validMessageID(envelope.MessageID):
		return fmt.Errorf("message_id must be a UUID")
	case envelope.MessageType == "":
		return fmt.Errorf("message_type is required")
	case envelope.SchemaVersion != CurrentSchemaVersion:
		return fmt.Errorf("unsupported schema_version %d", envelope.SchemaVersion)
	case envelope.CorrelationID == "":
		return fmt.Errorf("correlation_id is required")
	case envelope.OccurredAt.IsZero():
		return fmt.Errorf("occurred_at is required")
	case !json.Valid(envelope.Payload):
		return fmt.Errorf("payload must be valid JSON")
	default:
		return nil
	}
}
