// Package config loads and validates process configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidConfig identifies a missing or malformed configuration value.
var ErrInvalidConfig = errors.New("invalid configuration")

type Config struct {
	Service  string
	Instance string
	Database Database
	Broker   Broker
	HTTP     HTTP
	Clients  Clients
	Outbox   Outbox
	Shutdown Shutdown
}

type Database struct {
	Host              string
	Port              int
	User              string
	Password          string
	Name              string
	SSLMode           string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

type Broker struct {
	URL string
}

type HTTP struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	RequestBodyLimit  int64
}

type Clients struct {
	RequestTimeout time.Duration
	AttemptTimeout time.Duration
}

type Outbox struct {
	BatchSize    int
	PollInterval time.Duration
}

type Shutdown struct {
	Timeout time.Duration
}

// Load returns configuration for service, rejecting invalid required or typed values.
func Load(service string) (Config, error) {
	if strings.TrimSpace(service) == "" {
		return Config{}, invalid("service", "is required")
	}

	dbPort, err := intEnv("DB_PORT", 5432, 1)
	if err != nil {
		return Config{}, err
	}
	maxConns, err := intEnv("DB_MAX_CONNS", 10, 1)
	if err != nil {
		return Config{}, err
	}
	minConns, err := intEnv("DB_MIN_CONNS", 0, 0)
	if err != nil {
		return Config{}, err
	}
	if minConns > maxConns {
		return Config{}, invalid("DB_MIN_CONNS", "must not exceed DB_MAX_CONNS")
	}
	bodyLimit, err := int64Env("HTTP_REQUEST_BODY_LIMIT", 1<<20, 1)
	if err != nil {
		return Config{}, err
	}
	batchSize, err := intEnv("OUTBOX_BATCH_SIZE", 100, 1)
	if err != nil {
		return Config{}, err
	}

	dbLifetime, err := durationEnv("DB_MAX_CONN_LIFETIME", time.Hour)
	if err != nil {
		return Config{}, err
	}
	dbIdle, err := durationEnv("DB_MAX_CONN_IDLE_TIME", 30*time.Minute)
	if err != nil {
		return Config{}, err
	}
	healthCheck, err := durationEnv("DB_HEALTH_CHECK_PERIOD", time.Minute)
	if err != nil {
		return Config{}, err
	}
	readHeader, err := durationEnv("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	read, err := durationEnv("HTTP_READ_TIMEOUT", 15*time.Second)
	if err != nil {
		return Config{}, err
	}
	write, err := durationEnv("HTTP_WRITE_TIMEOUT", 30*time.Second)
	if err != nil {
		return Config{}, err
	}
	idle, err := durationEnv("HTTP_IDLE_TIMEOUT", time.Minute)
	if err != nil {
		return Config{}, err
	}
	request, err := durationEnv("CLIENT_REQUEST_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	attempt, err := durationEnv("CLIENT_ATTEMPT_TIMEOUT", 2*time.Second)
	if err != nil {
		return Config{}, err
	}
	if attempt > request {
		return Config{}, invalid("CLIENT_ATTEMPT_TIMEOUT", "must not exceed CLIENT_REQUEST_TIMEOUT")
	}
	poll, err := durationEnv("OUTBOX_POLL_INTERVAL", time.Second)
	if err != nil {
		return Config{}, err
	}
	shutdown, err := durationEnv("SHUTDOWN_TIMEOUT", 20*time.Second)
	if err != nil {
		return Config{}, err
	}

	dbName := strings.TrimSpace(os.Getenv("DB_NAME"))
	if dbName == "" {
		return Config{}, invalid("DB_NAME", "is required")
	}
	return Config{
		Service:  service,
		Instance: stringEnv("INSTANCE_ID", service),
		Database: Database{
			Host: stringEnv("DB_HOST", "localhost"), Port: dbPort,
			User: stringEnv("DB_USER", "commerce"), Password: stringEnv("DB_PASSWORD", "commerce"),
			Name: dbName, SSLMode: stringEnv("DB_SSLMODE", "disable"),
			MaxConns: int32(maxConns), MinConns: int32(minConns),
			MaxConnLifetime: dbLifetime, MaxConnIdleTime: dbIdle, HealthCheckPeriod: healthCheck,
		},
		Broker: Broker{URL: stringEnv("BROKER_URL", "amqp://guest:guest@localhost:5672/")},
		HTTP: HTTP{Addr: stringEnv("HTTP_ADDR", ":"+stringEnv("PORT", "8080")), ReadHeaderTimeout: readHeader,
			ReadTimeout: read, WriteTimeout: write, IdleTimeout: idle, RequestBodyLimit: bodyLimit},
		Clients:  Clients{RequestTimeout: request, AttemptTimeout: attempt},
		Outbox:   Outbox{BatchSize: batchSize, PollInterval: poll},
		Shutdown: Shutdown{Timeout: shutdown},
	}, nil
}

func stringEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback, minimum int) (int, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < minimum {
			return 0, invalid(name, "must be an integer >= "+strconv.Itoa(minimum))
		}
		return parsed, nil
	}
	return fallback, nil
}

func int64Env(name string, fallback, minimum int64) (int64, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < minimum {
			return 0, invalid(name, "must be an integer >= "+strconv.FormatInt(minimum, 10))
		}
		return parsed, nil
	}
	return fallback, nil
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return 0, invalid(name, "must be a positive duration")
		}
		return parsed, nil
	}
	return fallback, nil
}

func invalid(field, message string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidConfig, field, message)
}
