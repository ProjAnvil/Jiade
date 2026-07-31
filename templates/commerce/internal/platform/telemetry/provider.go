package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Config configures an OpenTelemetry trace provider.
type Config struct {
	Service  string
	Instance string
	Endpoint string
	Enabled  bool
	Insecure bool
}

// Provider owns the process trace provider and its shutdown lifecycle.
type Provider struct {
	tracer   *sdktrace.TracerProvider
	resource *resource.Resource
}

// New creates and installs an OpenTelemetry trace provider.
func New(ctx context.Context, cfg Config) (*Provider, error) {
	res := newResource(cfg)

	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if cfg.Enabled {
		exporterOptions := []otlptracegrpc.Option{}
		if cfg.Endpoint != "" {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithEndpoint(cfg.Endpoint))
		}
		if cfg.Insecure {
			exporterOptions = append(exporterOptions, otlptracegrpc.WithInsecure())
		}
		exporter, err := otlptracegrpc.New(ctx, exporterOptions...)
		if err != nil {
			return nil, err
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	}

	provider := &Provider{tracer: sdktrace.NewTracerProvider(options...), resource: res}
	otel.SetTracerProvider(provider.tracer)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider, nil
}

func newResource(cfg Config) *resource.Resource {
	return resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.Service),
		semconv.ServiceInstanceID(cfg.Instance),
	)
}

// Disabled returns and installs a provider that exports no spans.
func Disabled() *Provider {
	provider := &Provider{tracer: sdktrace.NewTracerProvider()}
	otel.SetTracerProvider(provider.tracer)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider
}

// Shutdown flushes and releases the trace provider resources.
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.tracer.Shutdown(ctx)
}
