// Package postgres provides pgx pool construction for a service-owned database.
package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"commerce/internal/platform/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Open creates and verifies a bounded PostgreSQL connection pool.
func Open(ctx context.Context, database config.Database) (*pgxpool.Pool, error) {
	if database.Name == "" {
		return nil, fmt.Errorf("database name is required")
	}
	connectionURL := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(database.User, database.Password),
		Host:   net.JoinHostPort(database.Host, strconv.Itoa(database.Port)),
		Path:   database.Name,
	}
	query := connectionURL.Query()
	query.Set("sslmode", database.SSLMode)
	connectionURL.RawQuery = query.Encode()

	poolConfig, err := pgxpool.ParseConfig(connectionURL.String())
	if err != nil {
		return nil, fmt.Errorf("parse postgres configuration: %w", err)
	}
	poolConfig.MaxConns = database.MaxConns
	poolConfig.MinConns = database.MinConns
	poolConfig.MaxConnLifetime = database.MaxConnLifetime
	poolConfig.MaxConnIdleTime = database.MaxConnIdleTime
	poolConfig.HealthCheckPeriod = database.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres pool: %w", err)
	}
	return pool, nil
}
