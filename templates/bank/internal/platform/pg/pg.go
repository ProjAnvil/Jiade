// Package pg provides PostgreSQL connection constructs.
package pg

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // Register pgx to database/sql
)

// DSN constructs a connection string from environment variables. dbName specifies which library to connect to (postgres/core_db).
func DSN(dbName string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		Getenv("DB_USER", "bank"),
		Getenv("DB_PASSWORD", "bank"),
		Getenv("DB_HOST", "localhost"),
		Getenv("DB_PORT", "15432"),
		dbName,
	)
}

// Open opens a connection pool to dbName. The caller is responsible for Close.
func Open(dbName string) (*sql.DB, error) {
	db, err := sql.Open("pgx", DSN(dbName))
	if err != nil {
		return nil, fmt.Errorf("pg: 打开 %s 失败: %w", dbName, err)
	}
	return db, nil
}

// Getenv reads environment variables, and returns def if empty.
func Getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
