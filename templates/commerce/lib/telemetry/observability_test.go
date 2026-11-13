package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestNewJSONLoggerWritesStructuredRecords(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := NewJSONLogger(buffer)
	logger.Info("order captured", "order_id", "ORD-1", "amount", 42)
	var record map[string]any
	if err := json.Unmarshal(buffer.Bytes(), &record); err != nil {
		t.Fatalf("decode log line: %v (%s)", err, buffer.String())
	}
	if record["msg"] != "order captured" || record["order_id"] != "ORD-1" {
		t.Fatalf("record=%v", record)
	}
	if !strings.Contains(buffer.String(), `"time"`) {
		t.Fatalf("log line missing time field: %s", buffer.String())
	}
}

func TestAMQPPropagatorKeysAreStable(t *testing.T) {
	// Keys is part of the propagation.TextMapCarrier contract; after injecting
	// a trace context the carrier must report the injected headers.
	original := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(original) })

	spanned := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:  trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		Remote:  true,
	}))
	headers := amqp.Table{}
	InjectAMQP(spanned, headers)
	carrier := amqpCarrier(headers)
	if len(carrier.Keys()) == 0 {
		t.Fatal("Keys must list the injected trace headers")
	}
	if carrier.Get("traceparent") == "" {
		t.Fatal("carrier must expose the injected traceparent")
	}
}

func TestAMQPCarrierGetFromNilTable(t *testing.T) {
	carrier := amqpCarrier{}
	if got := carrier.Get("traceparent"); got != "" {
		t.Fatalf("Get on empty carrier=%q, want empty", got)
	}
}

var _ propagation.TextMapCarrier = amqpCarrier{}
