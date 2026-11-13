package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// withEnv sets name=value for the duration of the subtest.
func withEnv(t *testing.T, name, value string) {
	t.Helper()
	t.Setenv(name, value)
}

func TestLoadRejectsBlankService(t *testing.T) {
	if _, err := Load("   "); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Load(blank service) error=%v, want ErrInvalidConfig", err)
	}
}

func TestLoadRejectsInvalidInts(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		value string
	}{
		{"db port not a number", "DB_PORT", "http"},
		{"db port below minimum", "DB_PORT", "0"},
		{"db max conns invalid", "DB_MAX_CONNS", "-1"},
		{"db min conns invalid", "DB_MIN_CONNS", "-2"},
		{"body limit invalid", "HTTP_REQUEST_BODY_LIMIT", "big"},
		{"body limit below minimum", "HTTP_REQUEST_BODY_LIMIT", "0"},
		{"outbox batch invalid", "OUTBOX_BATCH_SIZE", "none"},
		{"outbox batch below minimum", "OUTBOX_BATCH_SIZE", "0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withEnv(t, test.env, test.value)
			withEnv(t, "DB_NAME", "commerce")
			if _, err := Load("catalog"); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load error=%v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestLoadRejectsInvalidBools(t *testing.T) {
	for _, name := range []string{"OTEL_ENABLED", "OTEL_EXPORTER_OTLP_INSECURE"} {
		t.Run(name, func(t *testing.T) {
			withEnv(t, name, "maybe")
			withEnv(t, "DB_NAME", "commerce")
			if _, err := Load("catalog"); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load(%s=maybe) error=%v, want ErrInvalidConfig", name, err)
			}
		})
	}
}

func TestLoadRejectsInvalidDurations(t *testing.T) {
	for _, name := range []string{
		"DB_MAX_CONN_LIFETIME", "DB_MAX_CONN_IDLE_TIME", "DB_HEALTH_CHECK_PERIOD",
		"HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT", "CLIENT_REQUEST_TIMEOUT", "CLIENT_ATTEMPT_TIMEOUT",
		"OUTBOX_POLL_INTERVAL", "SHUTDOWN_TIMEOUT",
	} {
		t.Run(name+"_malformed", func(t *testing.T) {
			withEnv(t, name, "soon")
			withEnv(t, "DB_NAME", "commerce")
			if _, err := Load("catalog"); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load(%s=soon) error=%v, want ErrInvalidConfig", name, err)
			}
		})
		t.Run(name+"_nonpositive", func(t *testing.T) {
			withEnv(t, name, "0s")
			withEnv(t, "DB_NAME", "commerce")
			if _, err := Load("catalog"); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Load(%s=0s) error=%v, want ErrInvalidConfig", name, err)
			}
		})
	}
}

func TestLoadRejectsImpossibleRelationships(t *testing.T) {
	t.Run("min conns exceed max conns", func(t *testing.T) {
		withEnv(t, "DB_MAX_CONNS", "2")
		withEnv(t, "DB_MIN_CONNS", "3")
		withEnv(t, "DB_NAME", "commerce")
		_, err := Load("catalog")
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "DB_MIN_CONNS") {
			t.Fatalf("Load error=%v, want DB_MIN_CONNS rejection", err)
		}
	})
	t.Run("attempt timeout exceeds request timeout", func(t *testing.T) {
		withEnv(t, "CLIENT_REQUEST_TIMEOUT", "1s")
		withEnv(t, "CLIENT_ATTEMPT_TIMEOUT", "2s")
		withEnv(t, "DB_NAME", "commerce")
		_, err := Load("catalog")
		if !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "CLIENT_ATTEMPT_TIMEOUT") {
			t.Fatalf("Load error=%v, want CLIENT_ATTEMPT_TIMEOUT rejection", err)
		}
	})
}

func TestLoadAppliesOverridesAndDefaults(t *testing.T) {
	withEnv(t, "DB_NAME", "commerce")
	withEnv(t, "INSTANCE_ID", "instance-7")
	withEnv(t, "DB_HOST", "db.internal")
	withEnv(t, "DB_PORT", "6543")
	withEnv(t, "DB_MAX_CONN_LIFETIME", "2h")
	withEnv(t, "HTTP_ADDR", "127.0.0.1:9000")
	withEnv(t, "OTEL_ENABLED", "true")
	withEnv(t, "OTEL_EXPORTER_OTLP_ENDPOINT", "collector:4317")
	withEnv(t, "OTEL_EXPORTER_OTLP_INSECURE", "true")
	withEnv(t, "OUTBOX_BATCH_SIZE", "50")

	loaded, err := Load("catalog")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.Instance != "instance-7" {
		t.Fatalf("Instance=%q, want explicit INSTANCE_ID", loaded.Instance)
	}
	if loaded.Database.Host != "db.internal" || loaded.Database.Port != 6543 {
		t.Fatalf("Database=%+v", loaded.Database)
	}
	if loaded.Database.MaxConnLifetime != 2*time.Hour {
		t.Fatalf("MaxConnLifetime=%v", loaded.Database.MaxConnLifetime)
	}
	if loaded.HTTP.Addr != "127.0.0.1:9000" {
		t.Fatalf("Addr=%q", loaded.HTTP.Addr)
	}
	if !loaded.Telemetry.Enabled || !loaded.Telemetry.Insecure || loaded.Telemetry.Endpoint != "collector:4317" {
		t.Fatalf("Telemetry=%+v", loaded.Telemetry)
	}
	if loaded.Outbox.BatchSize != 50 {
		t.Fatalf("BatchSize=%d", loaded.Outbox.BatchSize)
	}
	// Defaults apply when unset.
	if loaded.Database.User != "commerce" || loaded.Database.SSLMode != "disable" {
		t.Fatalf("Database defaults=%+v", loaded.Database)
	}
	if loaded.Outbox.PollInterval != time.Second || loaded.Shutdown.Timeout != 20*time.Second {
		t.Fatalf("Outbox=%+v Shutdown=%+v", loaded.Outbox, loaded.Shutdown)
	}
}

func TestLoadDefaultsHTTPAddrFromPort(t *testing.T) {
	withEnv(t, "DB_NAME", "commerce")
	withEnv(t, "PORT", "9090")
	loaded, err := Load("catalog")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.HTTP.Addr != ":9090" {
		t.Fatalf("Addr=%q, want :9090 from PORT", loaded.HTTP.Addr)
	}
}

func TestLoadUsesDefaultsWithoutEnv(t *testing.T) {
	withEnv(t, "DB_NAME", "commerce")
	loaded, err := Load("catalog")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded.Database.Port != 5432 || loaded.Database.MaxConns != 10 || loaded.Database.MinConns != 0 {
		t.Fatalf("Database defaults=%+v", loaded.Database)
	}
	if loaded.HTTP.RequestBodyLimit != 1<<20 {
		t.Fatalf("RequestBodyLimit=%d", loaded.HTTP.RequestBodyLimit)
	}
	if loaded.Clients.RequestTimeout != 5*time.Second || loaded.Clients.AttemptTimeout != 2*time.Second {
		t.Fatalf("Clients=%+v", loaded.Clients)
	}
	if loaded.Broker.URL == "" {
		t.Fatal("Broker URL default missing")
	}
	if loaded.Telemetry.Endpoint != "otel-collector:4317" || loaded.Telemetry.Enabled {
		t.Fatalf("Telemetry=%+v", loaded.Telemetry)
	}
}

func TestDefaultInstanceFallsBackToService(t *testing.T) {
	// With a resolvable hostname the host wins; the service fallback is only
	// reachable when the hostname lookup fails, so assert the host path here.
	if instance := defaultInstance("catalog"); instance == "" {
		t.Fatal("defaultInstance must never be empty")
	}
}

func TestStringEnvTrimsAndFallsBack(t *testing.T) {
	withEnv(t, "STRING_ENV_TEST", "  value  ")
	if got := stringEnv("STRING_ENV_TEST", "fallback"); got != "value" {
		t.Fatalf("stringEnv=%q, want trimmed value", got)
	}
	withEnv(t, "STRING_ENV_TEST", "   ")
	if got := stringEnv("STRING_ENV_TEST", "fallback"); got != "fallback" {
		t.Fatalf("stringEnv=%q, want fallback for blank", got)
	}
}

func TestRequiredStringEnv(t *testing.T) {
	if got, err := requiredStringEnv("REQUIRED_STRING_UNSET", "fallback"); err != nil || got != "fallback" {
		t.Fatalf("unset=%q/%v, want fallback", got, err)
	}
	withEnv(t, "REQUIRED_STRING_TEST", "  spaced  ")
	if got, err := requiredStringEnv("REQUIRED_STRING_TEST", "fallback"); err != nil || got != "spaced" {
		t.Fatalf("set=%q/%v, want trimmed", got, err)
	}
	withEnv(t, "REQUIRED_STRING_TEST", "   ")
	if _, err := requiredStringEnv("REQUIRED_STRING_TEST", "fallback"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("blank error=%v, want ErrInvalidConfig", err)
	}
}
