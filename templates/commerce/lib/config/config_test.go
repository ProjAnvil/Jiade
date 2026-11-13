package config

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadTelemetryDefaults(t *testing.T) {
	settings := loadWithRequiredEnvironment(t)
	if settings.Telemetry.Enabled {
		t.Fatal("telemetry enabled by default")
	}
	if settings.Telemetry.Endpoint != "otel-collector:4317" {
		t.Fatalf("endpoint=%q", settings.Telemetry.Endpoint)
	}
	if settings.Telemetry.Insecure {
		t.Fatal("telemetry insecure by default")
	}
}

func TestLoadRejectsInvalidTelemetryBoolean(t *testing.T) {
	t.Setenv("DB_NAME", "order_db")
	t.Setenv("OTEL_ENABLED", "sometimes")
	_, err := Load("order")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "OTEL_ENABLED") {
		t.Fatalf("Load() error = %q, want OTEL_ENABLED field name", err)
	}
}

func TestLoadRejectsEmptyTelemetryEndpoint(t *testing.T) {
	t.Setenv("DB_NAME", "order_db")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", " ")
	_, err := Load("order")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_OTLP_ENDPOINT") {
		t.Fatalf("Load() error = %q, want OTEL_EXPORTER_OTLP_ENDPOINT field name", err)
	}
}

func loadWithRequiredEnvironment(t *testing.T) Config {
	t.Helper()
	t.Setenv("DB_NAME", "order_db")
	settings, err := Load("order")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return settings
}

func TestLoadRejectsMissingDatabaseName(t *testing.T) {
	t.Setenv("DB_NAME", "")
	_, err := Load("order")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
	}
}

func TestLoadRejectsInvalidDurationWithFieldName(t *testing.T) {
	t.Setenv("DB_NAME", "order_db")
	t.Setenv("HTTP_READ_TIMEOUT", "soon")
	_, err := Load("order")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load() error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "HTTP_READ_TIMEOUT") {
		t.Fatalf("Load() error = %q, want HTTP_READ_TIMEOUT field name", err)
	}
}
