// Package mysqlx 封装 MySQL 连接（带容器启动等待重试）。
package mysqlx

import (
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Open 打开并 ping 通数据库（最多等待 60s，容忍 compose 启动顺序）。
func Open(dsn string) *sql.DB {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("open mysql: %v", err)
	}
	for i := 0; i < 60; i++ {
		if err := db.Ping(); err == nil {
			return db
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("mysql not reachable: %s", dsn)
	return nil
}

// IsDuplicate 判断是否为 MySQL 唯一键冲突（1062）。
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
