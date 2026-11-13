package telemetry

import (
	"context"

	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// InjectAMQP injects the current trace context into AMQP message headers.
func InjectAMQP(ctx context.Context, headers amqp.Table) {
	otel.GetTextMapPropagator().Inject(ctx, amqpCarrier(headers))
}

// ExtractAMQP extracts a trace context from AMQP message headers.
func ExtractAMQP(ctx context.Context, headers amqp.Table) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, amqpCarrier(headers))
}

type amqpCarrier amqp.Table

func (c amqpCarrier) Get(key string) string {
	value, ok := c[key]
	if !ok {
		return ""
	}
	switch value := value.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return ""
	}
}

func (c amqpCarrier) Set(key, value string) {
	if c == nil {
		return
	}
	c[key] = value
}

func (c amqpCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for key := range c {
		keys = append(keys, key)
	}
	return keys
}

var _ propagation.TextMapCarrier = amqpCarrier{}
