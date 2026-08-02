// Package mysqlx wraps MySQL connections with container startup retry.
package mysqlx

import (
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Open opens the database and pings it until reachable (waits up to 60s, tolerating compose startup order).
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

// IsDuplicate reports whether err is a MySQL unique-key conflict (1062).
func IsDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
