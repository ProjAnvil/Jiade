package config

import (
	"errors"
	"testing"
)

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
}
