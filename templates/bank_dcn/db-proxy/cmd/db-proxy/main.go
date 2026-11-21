// Command db-proxy is the L4 connection layer between applications and the
// MySQL sets. Each backend set owns one listen port (port-per-set); the
// proxy speaks plain TCP and never parses the MySQL protocol. DBKeeper
// pushes the routing table over the admin API after a primary switch, so
// applications keep one stable endpoint (proxy host + set port) while the
// actual primary moves underneath.
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"bank-dcn/lib/httpx"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/runx"
)

// setEntry is one routed set: listen port -> current primary backend.
type setEntry struct {
	Name    string   `json:"name"`
	Port    int      `json:"port"`
	Nodes   []string `json:"nodes"`
	Primary string   `json:"primary"`
}

type setStats struct {
	ActiveConns int64 `json:"activeConns"`
	TotalConns  int64 `json:"totalConns"`
}

// Proxy owns the routing table and one listener per configured port.
type Proxy struct {
	mu       sync.Mutex
	sets     map[string]setEntry
	listens  map[int]net.Listener
	conns    map[int][]net.Conn
	counters map[string]*setStats
}

func New() *Proxy {
	return &Proxy{
		sets:     map[string]setEntry{},
		listens:  map[int]net.Listener{},
		conns:    map[int][]net.Conn{},
		counters: map[string]*setStats{},
	}
}

// Handler returns the admin route table.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "db-proxy", "GET /admin/routing", http.HandlerFunc(p.handleRouting))
	metrics.Handle(mux, "db-proxy", "PUT /admin/routing", http.HandlerFunc(p.handlePutRouting))
	metrics.Handle(mux, "db-proxy", "POST /admin/drain", http.HandlerFunc(p.handleDrain))
	return mux
}

func main() {
	p := New()
	adminAddr := runx.Env("PROXY_ADMIN_ADDR", ":18120")
	go func() {
		log.Printf("db-proxy admin listening on %s", adminAddr)
		srv := &http.Server{Addr: adminAddr, Handler: metrics.Mount(p.Handler())}
		if err := srv.ListenAndServe(); err != nil {
			log.Fatalf("admin server: %v", err)
		}
	}()
	select {}
}

func (p *Proxy) handleRouting(w http.ResponseWriter, _ *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sets := make([]setEntry, 0, len(p.sets))
	for _, s := range p.sets {
		sets = append(sets, s)
	}
	httpx.JSON(w, 200, map[string]any{"sets": sets, "stats": p.statsLocked()})
}

// handlePutRouting replaces the routing table (DBKeeper is the source of truth).
func (p *Proxy) handlePutRouting(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sets []setEntry `json:"sets"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Error(w, 400, err.Error())
		return
	}
	if err := p.applyRouting(body.Sets); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{"ok": true, "sets": len(body.Sets)})
}

// applyRouting replaces the whole routing table: ports that keep existing
// keep their listener; removed ports close their listener; live client
// connections are left to drain naturally.
func (p *Proxy) applyRouting(entries []setEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := map[string]setEntry{}
	for _, s := range entries {
		next[s.Name] = s
		if _, ok := p.counters[s.Name]; !ok {
			p.counters[s.Name] = &setStats{}
		}
	}
	// close listeners for ports that disappeared
	for port, ln := range p.listens {
		used := false
		for _, s := range next {
			if s.Port == port {
				used = true
				break
			}
		}
		if !used {
			_ = ln.Close()
			delete(p.listens, port)
		}
	}
	// open listeners for new ports
	for _, s := range next {
		if _, ok := p.listens[s.Port]; ok {
			continue
		}
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(s.Port))
		if err != nil {
			log.Printf("listen :%d for set %s: %v", s.Port, s.Name, err)
			continue
		}
		p.listens[s.Port] = ln
		go p.acceptLoop(s.Port, ln)
	}
	p.sets = next
	return nil
}

// handleDrain force-closes live client connections of one set. DBKeeper
// uses it as a network-side fence right after a switch, so clients cannot
// keep talking to the demoted primary.
func (p *Proxy) handleDrain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Set string `json:"set"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Error(w, 400, err.Error())
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sets[body.Set]
	if !ok {
		httpx.Error(w, 404, "unknown set")
		return
	}
	for _, c := range p.conns[s.Port] {
		_ = c.Close()
	}
	p.conns[s.Port] = nil
	httpx.JSON(w, 200, map[string]any{"drained": body.Set})
}

func (p *Proxy) acceptLoop(port int, ln net.Listener) {
	for {
		cconn, err := ln.Accept()
		if err != nil {
			return // listener closed by a routing-table replace
		}
		go p.pipe(port, cconn)
	}
}

func (p *Proxy) pipe(port int, cconn net.Conn) {
	p.mu.Lock()
	set, backend := "", ""
	for _, s := range p.sets {
		if s.Port == port {
			set, backend = s.Name, s.Primary
			break
		}
	}
	if set != "" {
		p.conns[port] = append(p.conns[port], cconn)
	}
	p.mu.Unlock()
	if set == "" {
		_ = cconn.Close()
		return
	}
	st := p.counters[set]
	if st != nil {
		atomic.AddInt64(&st.TotalConns, 1)
		atomic.AddInt64(&st.ActiveConns, 1)
	}
	// Snapshot the backend per connection: routing changes only affect NEW
	// connections, mirroring how a managed endpoint behaves on failover.
	bconn, err := net.DialTimeout("tcp", backend, 3*time.Second)
	if err != nil {
		_ = cconn.Close()
		if st != nil {
			atomic.AddInt64(&st.ActiveConns, -1)
		}
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(bconn, cconn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(cconn, bconn); done <- struct{}{} }()
	<-done
	_ = cconn.Close()
	_ = bconn.Close()
	<-done
	p.mu.Lock()
	p.conns[port] = removeConn(p.conns[port], cconn)
	p.mu.Unlock()
	if st != nil {
		atomic.AddInt64(&st.ActiveConns, -1)
	}
}

func (p *Proxy) statsLocked() map[string]setStats {
	out := map[string]setStats{}
	for name, st := range p.counters {
		out[name] = setStats{
			ActiveConns: atomic.LoadInt64(&st.ActiveConns),
			TotalConns:  atomic.LoadInt64(&st.TotalConns),
		}
	}
	return out
}

func removeConn(list []net.Conn, c net.Conn) []net.Conn {
	for i, x := range list {
		if x == c {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}
