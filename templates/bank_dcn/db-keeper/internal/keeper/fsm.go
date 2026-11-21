package keeper

import (
	"encoding/json"
	"io"
	"log"
	"sync"

	"github.com/hashicorp/raft"
)

// fsm is the raft state machine: a JSON document holding the desired sets
// and the audit event log. In-memory only (noop snapshots) — the desired
// state is rebuilt from SETS spec on restart, and events are best-effort.
type fsm struct {
	mu sync.Mutex
	st State
}

func newFSM() *fsm {
	return &fsm{st: State{Sets: map[string]SetState{}}}
}

func (f *fsm) get() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := State{Sets: map[string]SetState{}, Events: append([]Event{}, f.st.Events...)}
	for k, v := range f.st.Sets {
		st.Sets[k] = v
	}
	return st
}

// Apply is called by raft on the leader first, then followers (majority).
func (f *fsm) Apply(l *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		log.Printf("fsm: bad command: %v", err)
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd.Op {
	case "init":
		for _, s := range cmd.Sets {
			if _, exists := f.st.Sets[s.Name]; !exists {
				f.st.Sets[s.Name] = s
			}
		}
		f.appendEvent(Event{Type: "init", Reason: "sets registered"})
	case "switch":
		s, ok := f.st.Sets[cmd.Set]
		if !ok {
			return errUnknownSet
		}
		valid := false
		for _, n := range s.Nodes {
			if n == cmd.To {
				valid = true
				break
			}
		}
		if !valid {
			return errNotANode
		}
		f.appendEvent(Event{Type: "switch", Set: s.Name, From: s.Primary, To: cmd.To, Reason: "pending"})
		s.Primary = cmd.To
		f.st.Sets[s.Name] = s
	case "event":
		f.appendEvent(cmd.Event)
	default:
		return errUnknownOp
	}
	return nil
}

// Snapshot streams the serialized state (noop store keeps it in memory).
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(f.st)
	return &memSnapshot{data: b}, nil
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.NewDecoder(rc).Decode(&f.st)
}

func (f *fsm) appendEvent(e Event) {
	if e.Time == "" {
		e.Time = nowISO()
	}
	f.st.Events = append(f.st.Events, e)
	if len(f.st.Events) > 200 {
		f.st.Events = f.st.Events[len(f.st.Events)-200:]
	}
}

type memSnapshot struct{ data []byte }

func (m *memSnapshot) Persist(sink raft.SnapshotSink) error {
	_, err := sink.Write(m.data)
	return err
}

func (m *memSnapshot) Release() {}

// noop snapshot store: snapshots are discarded (teaching-grade; production
// would persist FSM snapshots next to the raft log).
type noopSnapshotStore struct{}

func newNoopSnapshot() raft.SnapshotStore { return &noopSnapshotStore{} }

func (n *noopSnapshotStore) Create(_ raft.SnapshotVersion, _ uint64, _ uint64, _ raft.Configuration, _ uint64, _ raft.Transport) (raft.SnapshotSink, error) {
	return &noopSink{}, nil
}

func (n *noopSnapshotStore) List() ([]*raft.SnapshotMeta, error) { return nil, nil }

func (n *noopSnapshotStore) Open(_ string) (*raft.SnapshotMeta, io.ReadCloser, error) {
	return nil, nil, nil
}

type noopSink struct{}

func (s *noopSink) Write(p []byte) (int, error) { return len(p), nil }
func (s *noopSink) Close() error                { return nil }
func (s *noopSink) ID() string                  { return "noop" }
func (s *noopSink) Cancel() error               { return nil }
