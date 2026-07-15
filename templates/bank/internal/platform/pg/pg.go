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
		Getenv("DB_USER", "bank"),
		Getenv("DB_PASSWORD", "bank"),
		Getenv("DB_HOST", "localhost"),
		Getenv("DB_PORT", "5432"),
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

// Getenv 读环境变量，空则返回 def。
func Getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
