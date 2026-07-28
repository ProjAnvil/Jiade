package telemetry

import (
	"context"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

func TestAMQPPropagationRoundTrip(t *testing.T) {
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, span := provider.Tracer("test").Start(context.Background(), "publish")
	defer span.End()
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	headers := amqp.Table{}
	InjectAMQP(ctx, headers)
	extracted := ExtractAMQP(context.Background(), headers)
	if got, want := oteltrace.SpanContextFromContext(extracted).TraceID(), span.SpanContext().TraceID(); got != want {
		t.Fatalf("trace ID=%s, want %s", got, want)
	}
}

func TestTraceFieldsReturnsIDsOnlyForValidSpan(t *testing.T) {
	if fields := TraceFields(context.Background()); len(fields) != 0 {
		t.Fatalf("TraceFields without a span = %v, want no fields", fields)
	}

	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	ctx, span := provider.Tracer("test").Start(context.Background(), "operation")
	defer span.End()
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	fields := TraceFields(ctx)
	if got, want := fields, []any{"trace_id", span.SpanContext().TraceID().String(), "span_id", span.SpanContext().SpanID().String()}; !equalFields(got, want) {
		t.Fatalf("TraceFields = %v, want %v", got, want)
	}
}

func equalFields(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
