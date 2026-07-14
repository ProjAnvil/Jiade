// Package pg 提供 PostgreSQL 连接构造。
package pg

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 pgx 到 database/sql
)

// DSN 从环境变量构造连接串。dbName 指定连哪个库（postgres / core_db）。
func DSN(dbName string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		getenv("DB_USER", "bank"),
		getenv("DB_PASSWORD", "bank"),
		getenv("DB_HOST", "localhost"),
		getenv("DB_PORT", "5432"),
		dbName,
	)
}

// Open 打开一个到 dbName 的连接池。调用方负责 Close。
func Open(dbName string) (*sql.DB, error) {
	db, err := sql.Open("pgx", DSN(dbName))
	if err != nil {
		return nil, fmt.Errorf("pg: 打开 %s 失败: %w", dbName, err)
	}
	return db, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
