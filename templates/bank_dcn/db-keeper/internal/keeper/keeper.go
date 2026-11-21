package keeper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Config is the keeper runtime configuration (env-driven).
type Config struct {
	NodeID   string
	RaftAddr string // e.g. db-keeper-1:18111
	Peers    string // comma list, all raft addresses
	SetsSpec string
	RootPW   string
	Proxies  string // comma list of DBProxy admin URLs
}

// Keeper wires the raft group, the FSM, and the actuator together.
type Keeper struct {
	cfg     Config
	r       *raft.Raft
	fsm     *fsm
	dbs     *dbPool
	health  map[string]map[string]Health // set -> addr -> health (volatile)
	failCnt map[string]int               // set -> consecutive primary probe failures
	mu      sync.Mutex
}

// New builds the keeper and bootstraps the raft group.
func New(cfg Config) (*Keeper, error) {
	cfg.RaftAddr = withPort(cfg.RaftAddr, 18111)
	f := newFSM()
	r, err := raft.NewRaft(&raft.Config{
		LocalID:         raft.ServerID(cfg.NodeID),
		ProtocolVersion: raft.ProtocolVersionMax,
		// v1.7 validates the timer chain explicitly (lease <= heartbeat
		// <= election); zeros are rejected instead of defaulted.
		HeartbeatTimeout:   1000 * time.Millisecond,
		ElectionTimeout:    2000 * time.Millisecond,
		LeaderLeaseTimeout: 500 * time.Millisecond,
		CommitTimeout:      50 * time.Millisecond,
		SnapshotThreshold:  4096,
		SnapshotInterval:   30 * time.Second,
		MaxAppendEntries:   64,
		LogOutput:          os.Stderr,
	}, f, raft.NewInmemStore(), raft.NewInmemStore(), newNoopSnapshot(), makeTransport(cfg.RaftAddr))
	if err != nil {
		return nil, fmt.Errorf("raft: %w", err)
	}
	var voters []raft.Server
	for _, p := range splitList(cfg.Peers) {
		p = withPort(p, 18111)
		id, _, err := net.SplitHostPort(p)
		if err != nil {
			return nil, fmt.Errorf("peer %q: %w", p, err)
		}
		voters = append(voters, raft.Server{ID: raft.ServerID(id), Address: raft.ServerAddress(p)})
	}
	if err := r.BootstrapCluster(raft.Configuration{Servers: voters}).Error(); err != nil {
		log.Printf("bootstrap (ok if already bootstrapped): %v", err)
	}
	return &Keeper{
		cfg:     cfg,
		r:       r,
		fsm:     f,
		dbs:     newDBPool(cfg.RootPW),
		health:  map[string]map[string]Health{},
		failCnt: map[string]int{},
	}, nil
}

// State returns the FSM snapshot.
func (k *Keeper) State() State { return k.fsm.get() }

// Apply submits a command through raft (majority commit).
func (k *Keeper) Apply(cmd Command, timeout time.Duration) error {
	b, _ := json.Marshal(cmd)
	f := k.r.Apply(b, timeout)
	if f.Error() != nil {
		return f.Error()
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
}

// LeaderHTTPAddr returns the leader's HTTP endpoint ("host:18110") or "".
func (k *Keeper) LeaderHTTPAddr() string {
	addr, _ := k.r.LeaderWithID()
	s := string(addr)
	if s == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return ""
	}
	return net.JoinHostPort(host, "18110")
}

// IsLeader reports whether this node currently leads the raft group.
func (k *Keeper) IsLeader() bool { return k.r.State() == raft.Leader }

// relayToLeader proxies a request to the current leader inside the docker
// network (a browser/curl-facing 307 would point at an unreachable
// container IP, so followers relay server-side instead).
func (k *Keeper) relayToLeader(w http.ResponseWriter, r *http.Request, body []byte) bool {
	lead := k.LeaderHTTPAddr()
	if lead == "" || (k.IsLeader()) {
		return false
	}
	url := "http://" + lead + r.URL.Path + "?redirect=no"
	var req *http.Request
	if body != nil {
		req, _ = http.NewRequest(r.Method, url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, _ = http.NewRequest(r.Method, url, nil)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "leader unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return true
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return true
}

// HandleState serves GET /state: raft desired state + volatile health.
func (k *Keeper) HandleState(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("redirect") != "no" && k.relayToLeader(w, r, nil) {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	sets := []SetView{}
	for name, s := range k.fsm.get().Sets {
		v := SetView{SetState: s}
		for _, addr := range s.Nodes {
			v.Health = append(v.Health, NodeHealth{Addr: addr, Health: k.health[name][addr]})
		}
		sets = append(sets, v)
	}
	writeJSON(w, map[string]any{
		"leader": k.LeaderHTTPAddr(),
		"self":   k.cfg.NodeID,
		"sets":   sets,
		"events": k.fsm.get().Events,
	})
}

// HandleSwitch serves POST /sets/{name}/switch {"to": "host:port"}.
func (k *Keeper) HandleSwitch(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if k.relayToLeader(w, r, raw) {
		return
	}
	var body struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st := k.fsm.get()
	set, ok := st.Sets[r.PathValue("name")]
	if !ok {
		http.Error(w, "unknown set", http.StatusNotFound)
		return
	}
	if body.To == "" { // pick the first non-primary node
		for _, n := range set.Nodes {
			if n != set.Primary {
				body.To = n
				break
			}
		}
	}
	if body.To == "" || body.To == set.Primary {
		http.Error(w, "no alternate node", http.StatusConflict)
		return
	}
	if err := k.Apply(Command{Op: "switch", Set: set.Name, To: body.To}, 5*time.Second); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if body.Reason == "" {
		body.Reason = "manual"
	}
	writeJSON(w, map[string]any{"ok": true, "set": set.Name, "switchingTo": body.To})
}

// pushRouting PUTs the routing table to every DBProxy.
func (k *Keeper) pushRouting() error {
	st := k.fsm.get()
	sets := make([]map[string]any, 0, len(st.Sets))
	for _, s := range st.Sets {
		sets = append(sets, map[string]any{"name": s.Name, "port": s.Port, "nodes": s.Nodes, "primary": s.Primary})
	}
	body, _ := json.Marshal(map[string]any{"sets": sets})
	for _, p := range splitList(k.cfg.Proxies) {
		req, _ := http.NewRequest(http.MethodPut, p+"/admin/routing", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("proxy %s: %w", p, err)
		}
		_ = resp.Body.Close()
	}
	return nil
}

// drainSet asks every DBProxy to cut live client connections of one set
// (network-side fence of the demoted primary).
func (k *Keeper) drainSet(name string) {
	body, _ := json.Marshal(map[string]string{"set": name})
	for _, p := range splitList(k.cfg.Proxies) {
		req, _ := http.NewRequest(http.MethodPost, p+"/admin/drain", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range splitComma(s) {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitComma(s string) []string { return stringsSplitTrim(s, ",") }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func withPort(addr string, port int) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return net.JoinHostPort(addr, fmt.Sprint(port))
	}
	return addr
}
