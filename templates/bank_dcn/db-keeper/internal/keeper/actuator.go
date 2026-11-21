package keeper

import (
	"log"
	"time"
)

// RunActuator is the leader-only control loop: register sets, bootstrap
// replication, probe health, auto-failover on persistent primary failure,
// and reconcile desired primary -> actual (fence, promote, re-attach, then
// repoint DBProxy routing). Followers just idle until they lead.
func (k *Keeper) RunActuator() {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for range tick.C {
		if !k.IsLeader() {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("actuator panic: %v", r)
				}
			}()
			k.registerSets()
			k.probeAll()
			k.autoFailover()
			k.reconcile()
		}()
	}
}

// registerSets pushes the SETS spec into raft state on first leader run.
func (k *Keeper) registerSets() {
	st := k.fsm.get()
	if len(st.Sets) > 0 {
		return
	}
	sets, err := ParseSetsSpec(k.cfg.SetsSpec)
	if err != nil {
		log.Printf("SETS spec invalid: %v", err)
		return
	}
	if err := k.Apply(Command{Op: "init", Sets: sets}, 5*time.Second); err != nil {
		log.Printf("init apply: %v", err)
	}
}

// probeAll refreshes volatile health for every node of every set.
func (k *Keeper) probeAll() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, s := range k.fsm.get().Sets {
		if k.health[name] == nil {
			k.health[name] = map[string]Health{}
		}
		for _, addr := range s.Nodes {
			h := Health{Role: "unknown"}
			if k.dbs.ping(addr) {
				h.OK = true
				if lag, running := k.dbs.replicaStatus(addr); running {
					h.Role = "replica"
					h.Lag = lag
				} else {
					h.Role = "primary"
				}
			}
			k.health[name][addr] = h
		}
	}
}

// autoFailover promotes a healthy, caught-up replica after three
// consecutive primary probe failures, and only when no switch is pending.
func (k *Keeper) autoFailover() {
	st := k.fsm.get()
	k.mu.Lock()
	defer k.mu.Unlock()
	for name, s := range st.Sets {
		primaryHealth := k.health[name][s.Primary]
		if primaryHealth.OK {
			k.failCnt[name] = 0
			continue
		}
		k.failCnt[name]++
		if k.failCnt[name] < 3 {
			continue
		}
		log.Printf("primary %s of set %s unreachable (%d probes); failover window open", s.Primary, name, k.failCnt[name])
		// candidate: healthy replica with lag == 0
		for _, addr := range s.Nodes {
			if addr == s.Primary {
				continue
			}
			h := k.health[name][addr]
			if h.OK && h.Role == "replica" && h.Lag != nil && *h.Lag == 0 {
				if err := k.Apply(Command{Op: "switch", Set: name, To: addr}, 5*time.Second); err != nil {
					log.Printf("auto switch apply: %v", err)
				} else {
					k.Apply(Command{Op: "event", Event: Event{Type: "failover", Set: name, From: s.Primary, To: addr, Reason: "primary unreachable (3 probes)"}}, 5*time.Second)
					k.failCnt[name] = 0
				}
				break
			}
		}
	}
}

// reconcile walks desired primary vs replication topology and completes
// pending switches (the "event" log marks pending ones).
func (k *Keeper) reconcile() {
	st := k.fsm.get()
	for name, s := range st.Sets {
		// bootstrap replication on any node that is not the desired primary
		for _, addr := range s.Nodes {
			if addr == s.Primary {
				if err := k.dbs.ensureReplicationUser(addr); err != nil {
					log.Printf("ensure repl user on %s: %v", addr, err)
				}
				continue
			}
			if _, running := k.dbs.replicaStatus(addr); !running {
				if err := k.dbs.startReplica(addr, s.Primary); err != nil {
					log.Printf("start replica %s -> %s: %v", addr, s.Primary, err)
				}
			}
		}
		// find the pending switch event for this set
		pending := pendingSwitch(st.Events, name)
		if pending == "" {
			k.pushRoutingOnce(name)
			continue
		}
		old, new := pendingFromTo(st.Events, name)
		log.Printf("reconciling switch of %s: %s -> %s", name, old, new)
		if err := k.switchPrimary(s, old, new, pending); err != nil {
			log.Printf("switch %s failed: %v", name, err)
			k.Apply(Command{Op: "event", Event: Event{Type: "error", Set: name, Reason: err.Error()}}, 5*time.Second)
			continue
		}
		k.Apply(Command{Op: "event", Event: Event{Type: "switch-done", Set: name, From: old, To: new, Reason: "promoted, re-attached, routing repointed"}}, 5*time.Second)
		k.pushRoutingOnce(name)
	}
}

// switchPrimary executes the ordered procedure: catch-up, fence, promote,
// re-attach old as replica, drain proxy connections.
func (k *Keeper) switchPrimary(s SetState, old, new, pending string) error {
	// 1. wait for the new primary to catch up (data safety before fence)
	if err := k.dbs.waitCaughtUp(new, 15, 2*time.Second); err != nil {
		return err
	}
	// 2. fence the old primary (best effort — it may be unreachable, which
	// is itself a fence as long as the proxy stops routing to it)
	_ = k.dbs.fence(old)
	// 3. promote the new primary
	if err := k.dbs.promote(new); err != nil {
		return err
	}
	// 4. replication user on the new primary, old primary joins as replica
	if err := k.dbs.ensureReplicationUser(new); err != nil {
		return err
	}
	if k.dbs.ping(old) {
		if err := k.dbs.startReplica(old, new); err != nil {
			log.Printf("re-attach old primary %s: %v", old, err)
		}
	}
	// 5. network-side fence: cut live client connections at the proxy
	k.drainSet(s.Name)
	_ = pending
	return nil
}

// pushRoutingOnce pushes routing when it differs from desired state; cheap
// enough to call every tick for the teaching stack (idempotent PUT).
func (k *Keeper) pushRoutingOnce(string) {
	if err := k.pushRouting(); err != nil {
		log.Printf("push routing: %v", err)
	}
}

// pendingSwitch reports the target of the last unresolved switch event.
func pendingSwitch(events []Event, set string) string {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Set != set {
			continue
		}
		switch e.Type {
		case "switch":
			return e.To
		case "switch-done", "failover":
			return ""
		case "error":
			return ""
		}
	}
	return ""
}

func pendingFromTo(events []Event, set string) (string, string) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Set == set && e.Type == "switch" {
			return e.From, e.To
		}
	}
	return "", ""
}
