package telemetry

// Tests for NewJSONLogger's output shape, TraceFields' empty context, and the
// amqpCarrier Keys/Get/Set helpers.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestNewJSONLogger_WritesStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := NewJSONLogger(&buf)
	logger.Info("hello", "trace_id", "t-1")
	line := buf.String()
	if !strings.HasPrefix(line, "{") {
		t.Fatalf("output must be a JSON line, got %q", line)
	}
	if !strings.Contains(line, `"msg":"hello"`) || !strings.Contains(line, `"trace_id":"t-1"`) {
		t.Errorf("structured fields missing: %s", line)
	}
}

func TestTraceFields_EmptyContextYieldsNil(t *testing.T) {
	if got := TraceFields(context.Background()); got != nil {
		t.Errorf("TraceFields on a context without a span = %v, want nil", got)
	}
}

func TestAMQPCarrier_GetSetKeys(t *testing.T) {
	carrier := amqpCarrier{}
	carrier.Set("traceparent", "00-abc-def-01")
	if got := carrier.Get("traceparent"); got != "00-abc-def-01" {
		t.Errorf("Get = %q", got)
	}
	if got := carrier.Get("missing"); got != "" {
		t.Errorf("missing key must yield empty string, got %q", got)
	}
	// AMQP headers may carry []byte values; Get must stringify them.
	carrier["binary"] = []byte("raw")
	if got := carrier.Get("binary"); got != "raw" {
		t.Errorf("[]byte value must stringify, got %q", got)
	}
	carrier["unsupported"] = 42
	if got := carrier.Get("unsupported"); got != "" {
		t.Errorf("unsupported value type must yield empty string, got %q", got)
	}

	keys := carrier.Keys()
	if len(keys) != 3 {
		t.Errorf("Keys = %v, want 3 entries", keys)
	}
	// A nil carrier must be safe to Set on (no panic) per the guard.
	var nilCarrier amqpCarrier
	nilCarrier.Set("k", "v")
	if len(nilCarrier) != 0 {
		t.Error("nil carrier Set must be a no-op")
	}
	if got := nilCarrier.Keys(); len(got) != 0 {
		t.Errorf("nil carrier Keys = %v, want empty", got)
	}
	_ = amqp.Table(carrier) // constructor compatibility with InjectAMQP/ExtractAMQP
}
