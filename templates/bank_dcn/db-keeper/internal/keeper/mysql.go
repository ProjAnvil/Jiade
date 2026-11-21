package keeper

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

var (
	errUnknownSet = errors.New("unknown set")
	errNotANode   = errors.New("target is not a node of this set")
	errUnknownOp  = errors.New("unknown op")
	replUser      = "repl"
	replPass      = "repl123"
)

// dbPool lazily opens one *sql.DB per backend address (root credentials).
type dbPool struct {
	rootPW string
	mu     sync.Mutex
	dbs    map[string]*sql.DB
}

func newDBPool(rootPW string) *dbPool {
	return &dbPool{rootPW: rootPW, dbs: map[string]*sql.DB{}}
}

func (p *dbPool) get(addr string) (*sql.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if db, ok := p.dbs[addr]; ok {
		return db, nil
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root:%s@tcp(%s)/mysql?timeout=2s&readTimeout=3s&writeTimeout=3s&parseTime=true", p.rootPW, addr))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	p.dbs[addr] = db
	return db, nil
}

// ping reports reachability with a short deadline.
func (p *dbPool) ping(addr string) bool {
	db, err := p.get(addr)
	if err != nil {
		return false
	}
	c, cancel := contextTimeout(1500 * time.Millisecond)
	defer cancel()
	return db.PingContext(c) == nil
}

// exec runs statements (best-effort) and reports the first error.
func (p *dbPool) exec(addr string, stmts ...string) error {
	db, err := p.get(addr)
	if err != nil {
		return err
	}
	c, cancel := contextTimeout(5 * time.Second)
	defer cancel()
	for _, s := range stmts {
		if _, err := db.ExecContext(c, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

// replicaStatus returns Seconds_Behind_Source and replication liveness for
// a node; ok=false when the node is not replicating (e.g. it is a primary).
func (p *dbPool) replicaStatus(addr string) (lag *int, running bool) {
	db, err := p.get(addr)
	if err != nil {
		return nil, false
	}
	c, cancel := contextTimeout(3 * time.Second)
	defer cancel()
	rows, err := db.QueryContext(c, "SHOW REPLICA STATUS")
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	if !rows.Next() {
		return nil, false // primary: no replica rows
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, false
	}
	m := map[string]any{}
	for i, col := range cols {
		m[col] = vals[i]
	}
	io, sqlOK := m["Replica_IO_Running"], m["Replica_SQL_Running"]
	running = str(io) == "Yes" && str(sqlOK) == "Yes"
	if s, ok := m["Seconds_Behind_Source"]; ok && s != nil {
		switch v := s.(type) {
		case int64:
			lagi := int(v)
			return &lagi, running
		case []byte:
			var n int
			if _, err := fmt.Sscanf(string(v), "%d", &n); err == nil {
				return &n, running
			}
		}
	}
	return nil, running
}

// ensureReplicationUser creates the replication account on one node.
func (p *dbPool) ensureReplicationUser(addr string) error {
	return p.exec(addr,
		fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'", replUser, replPass),
		// 8.0 parses only the legacy privilege name here (REPLICA alias
		// is not accepted in GRANT on 8.0.x).
		fmt.Sprintf("GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'", replUser),
	)
}

// startReplica points addr at source and starts replication (GTID-based).
func (p *dbPool) startReplica(addr, source string) error {
	host, port, err := net.SplitHostPort(source)
	if err != nil {
		return err
	}
	return p.exec(addr,
		"STOP REPLICA",
		fmt.Sprintf("CHANGE REPLICATION SOURCE TO SOURCE_HOST='%s', SOURCE_PORT=%s, SOURCE_USER='%s', SOURCE_PASSWORD='%s', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1", host, port, replUser, replPass),
		"START REPLICA",
	)
}

// waitCaughtUp blocks until the replica at addr reports zero lag (or fails).
func (p *dbPool) waitCaughtUp(addr string, attempts int, sleep time.Duration) error {
	for i := 0; i < attempts; i++ {
		lag, running := p.replicaStatus(addr)
		if running && lag != nil && *lag == 0 {
			return nil
		}
		time.Sleep(sleep)
	}
	return fmt.Errorf("%s did not catch up", addr)
}

// fence makes a primary refuse writes (best-effort, teaching-grade fence:
// in production pair this with credential rotation or a SG lockdown).
func (p *dbPool) fence(addr string) error {
	return p.exec(addr, "SET GLOBAL super_read_only=ON")
}

// promote turns a replica into an independent writable primary.
func (p *dbPool) promote(addr string) error {
	return p.exec(addr,
		"STOP REPLICA",
		"RESET REPLICA ALL",
		"SET GLOBAL super_read_only=OFF",
		"SET GLOBAL read_only=OFF",
	)
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\n;"); i >= 0 {
		return s[:i]
	}
	return s
}
