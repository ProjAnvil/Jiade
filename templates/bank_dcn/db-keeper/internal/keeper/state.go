// Package keeper implements the DBKeeper service: raft-replicated desired
// state plus a leader-side actuator that reconciles it against real MySQL
// backends and DBProxy routing.
package keeper

import (
	"fmt"
	"strconv"
	"strings"
)

// SetState is the desired state of one MySQL set (raft-replicated).
type SetState struct {
	Name    string   `json:"name"`
	Port    int      `json:"port"` // DBProxy listen port for this set
	Nodes   []string `json:"nodes"`
	Primary string   `json:"primary"` // desired primary "host:port"
}

// Event is an audit-log entry (raft-replicated).
type Event struct {
	Time   string `json:"time"`
	Type   string `json:"type"` // init | switch | error | probe
	Set    string `json:"set,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// State is the full raft-replicated document.
type State struct {
	Sets   map[string]SetState `json:"sets"`
	Events []Event             `json:"events"`
}

// Health is volatile per-node probe output (not raft-replicated).
type Health struct {
	OK   bool   `json:"ok"`
	Lag  *int   `json:"lagSeconds,omitempty"`
	Role string `json:"role"` // primary | replica | unknown
}

// NodeHealth aggregates one set's probe snapshot.
type NodeHealth struct {
	Addr string `json:"addr"`
	Health
}

// SetView merges desired state with volatile health for the API.
type SetView struct {
	SetState
	Health []NodeHealth `json:"health"`
}

// Command is an FSM Apply payload.
type Command struct {
	Op    string     `json:"op"` // init | switch | event
	Sets  []SetState `json:"sets,omitempty"`
	Set   string     `json:"set,omitempty"`
	To    string     `json:"to,omitempty"`
	Event Event      `json:"event,omitempty"`
}

// ParseSetsSpec parses SETS="name:port:addr,addr;name2:port2:addr2,addr2".
func ParseSetsSpec(spec string) ([]SetState, error) {
	var out []SetState
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.SplitN(part, ":", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("bad set spec %q (want name:port:addr,addr)", part)
		}
		port, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("bad port in %q: %v", part, err)
		}
		nodes := strings.Split(fields[2], ",")
		for i := range nodes {
			nodes[i] = strings.TrimSpace(nodes[i])
		}
		out = append(out, SetState{Name: fields[0], Port: port, Nodes: nodes, Primary: nodes[0]})
	}
	return out, nil
}
