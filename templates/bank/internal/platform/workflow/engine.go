package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"bank/internal/platform/messaging"
)

// Direction values persisted on ActionRecord; mirrored from the DB CHECK
// constraint ck_workflow_action_direction. Unexported until a later task
// surfaces compensation direction to callers.
const (
	directionForward      = "forward"
	directionCompensation = "compensation"
)

// Engine is the durable workflow orchestrator. It advances Instances through
// a Definition's ordered Actions by preparing an immutable context, emitting
// the first dispatch command, and — in later tasks — applying result events
// and compensations. All state changes go through the Store.
//
// Engine values are safe for concurrent use: they hold no mutable state of
// their own, and all per-instance state is guarded by the Store's
// WithInstance lock.
type Engine struct {
	store    Store
	registry *Registry
	config   EngineConfig
}

// NewEngine wires a Store, a populated Registry, and an EngineConfig into an
// Engine. Zero-valued EngineConfig fields yield the documented defaults:
//
//   - ExecuteMaxAttempts      = 3
//   - CompensationMaxAttempts = 5
//   - OperationalDeadline     = 2 * time.Minute
//   - Now                     = time.Now
//
// Explicit non-zero values are preserved, so callers may override any subset.
// A nil Now falls back to time.Now (a zero func() time.Time value is nil in
// Go, so the standard zero check covers it).
func NewEngine(store Store, registry *Registry, config EngineConfig) *Engine {
	if config.ExecuteMaxAttempts <= 0 {
		config.ExecuteMaxAttempts = 3
	}
	if config.CompensationMaxAttempts <= 0 {
		config.CompensationMaxAttempts = 5
	}
	if config.OperationalDeadline <= 0 {
		config.OperationalDeadline = 2 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Engine{store: store, registry: registry, config: config}
}

// Start validates that a Definition is registered for (req.Type, req.Version)
// and asks the Store to persist a new Instance in StatusPreparing. The
// returned Instance is a snapshot at creation time; callers proceed by
// invoking Prepare to drive the first action.
//
// Start does not call Definition.Prepare; the immutable context is computed
// later, in Prepare, outside any transaction.
func (e *Engine) Start(ctx context.Context, req StartRequest) (Instance, error) {
	if _, ok := e.registry.Get(req.Type, req.Version); !ok {
		return Instance{}, fmt.Errorf("%w: type=%q version=%d", ErrDefinitionNotFound, req.Type, req.Version)
	}
	return e.store.Create(ctx, req)
}

// Prepare runs the Definition's Prepare outside any transaction to obtain the
// immutable preparedContext, then atomically transitions the Instance from
// StatusPreparing to StatusRunning: it locks the instance, verifies it is
// still preparing (idempotent — if already advanced, returns nil), saves the
// preparedContext, runs the first Action's Execute, persists the Instance
// (Status=running, CurrentAction=0, Revision++), persists the first
// ActionRecord (Status=waiting_result, Direction=forward, Attempt=1), and
// AppendOutbox-es the first dispatch command — all inside the one Store
// transaction.
//
// If the Definition's Prepare returns an error (business validation failure),
// the Instance is transitioned to StatusRejected with LastError and
// LastErrorClass=BusinessRejected recorded, and Engine.Prepare returns nil:
// rejection is a workflow outcome, not a system error. System errors (Store
// failure, Action.Execute failure) are returned to the caller for retry.
//
// Because the Instance is re-checked inside the transaction, Prepare is safe
// to invoke more than once: a duplicate call on an already-running or
// already-rejected instance returns nil without side effects.
func (e *Engine) Prepare(ctx context.Context, id string) error {
	header, err := e.readHeader(ctx, id)
	if err != nil {
		return err
	}
	// Idempotent short-circuit: another worker already advanced it.
	if header.Status != StatusPreparing {
		return nil
	}

	def, ok := e.registry.Get(header.Type, header.Version)
	if !ok {
		return fmt.Errorf("%w: type=%q version=%d", ErrDefinitionNotFound, header.Type, header.Version)
	}

	preparedContext, prepareErr := def.Prepare(ctx, header.Input)
	if prepareErr != nil {
		return e.rejectInstance(ctx, id, prepareErr)
	}

	// Defensive guard: a definition whose Actions() is empty cannot advance.
	// The Registry permits zero-action definitions today; the Engine rejects
	// them here (after Prepare has succeeded) rather than panicking on
	// actions[0] in dispatchFirstAction.
	if len(def.Actions()) == 0 {
		return fmt.Errorf("workflow definition %q v%d has no actions", def.Type(), def.Version())
	}

	return e.dispatchFirstAction(ctx, id, def, preparedContext)
}

// readHeader is a lightweight WithInstance read that returns a snapshot of
// the Instance used to look up the Definition and drive Definition.Prepare
// outside any transaction. It is the only way to learn (Type, Version, Input)
// given just an instance id, because the Store interface exposes no separate
// read method.
func (e *Engine) readHeader(ctx context.Context, id string) (Instance, error) {
	var header Instance
	if err := e.store.WithInstance(ctx, id, func(tx Tx) error {
		header = *tx.Instance()
		return nil
	}); err != nil {
		return Instance{}, fmt.Errorf("load instance %q: %w", id, err)
	}
	return header, nil
}

// rejectInstance transitions an Instance from StatusPreparing to
// StatusRejected, recording the business error. Idempotent: if the instance
// is no longer preparing (race or duplicate), it is left untouched.
func (e *Engine) rejectInstance(ctx context.Context, id string, prepareErr error) error {
	return e.store.WithInstance(ctx, id, func(tx Tx) error {
		current := tx.Instance()
		if current.Status != StatusPreparing {
			return nil
		}
		inst := *current
		inst.Status = StatusRejected
		inst.LastError = prepareErr.Error()
		inst.LastErrorClass = BusinessRejected
		inst.Revision++
		return tx.SaveInstance(inst)
	})
}

// dispatchFirstAction performs the real StatusPreparing → StatusRunning
// transition: saves the immutable preparedContext, builds the first Action's
// dispatch via Action.Execute, persists the instance and the first
// ActionRecord, and AppendOutbox-es the dispatch command — all inside the
// Store transaction. Idempotent via the StatusPreparing re-check.
func (e *Engine) dispatchFirstAction(ctx context.Context, id string, def Definition, preparedContext []byte) error {
	actions := def.Actions()
	firstAction := actions[0]

	return e.store.WithInstance(ctx, id, func(tx Tx) error {
		current := tx.Instance()
		// Idempotent: if another worker advanced the instance between our
		// readHeader and this lock, leave it alone.
		if current.Status != StatusPreparing {
			return nil
		}

		// Build the dispatch from a View of the soon-to-be-running instance.
		inst := *current
		inst.PreparedContext = append(json.RawMessage(nil), preparedContext...)
		inst.Status = StatusRunning
		inst.CurrentAction = 0

		now := e.config.Now()
		inst.OperationalDeadline = now.Add(e.config.OperationalDeadline)
		inst.Revision++

		view := View{Instance: inst, Action: ActionRecord{Index: 0, Name: firstAction.Name()}}
		dispatch, err := firstAction.Execute(ctx, view)
		if err != nil {
			// Propagate to caller for retry; the Tx rolls back, leaving the
			// instance in StatusPreparing. Classification of the error
			// (transient vs business) is deferred to Task 4's ApplyResult.
			return fmt.Errorf("action %q Execute: %w", firstAction.Name(), err)
		}

		if err := tx.SaveInstance(inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		idempotencyKey := dispatch.IdempotencyKey
		if idempotencyKey == "" {
			idempotencyKey = fmt.Sprintf("%s:%s:%d", inst.ID, firstAction.Name(), 1)
		}
		actionRec := ActionRecord{
			Index:          0,
			Name:           firstAction.Name(),
			Status:         ActionWaitingResult,
			Direction:      directionForward,
			Attempt:        1,
			IdempotencyKey: idempotencyKey,
			CommandID:      newUUID(),
			DeadlineAt:     deadlineAt(now, dispatch.Deadline),
		}
		if err := tx.SaveAction(actionRec); err != nil {
			return fmt.Errorf("save action %q: %w", firstAction.Name(), err)
		}

		env := buildCommandEnvelope(inst.ID, firstAction.Name(), actionRec, dispatch, e.config.Now)
		if err := tx.AppendOutbox(env, dispatch.RoutingKey); err != nil {
			return fmt.Errorf("append outbox for %q: %w", firstAction.Name(), err)
		}
		return nil
	})
}

// buildCommandEnvelope assembles the messaging.Envelope for the dispatch
// command emitted by an action. The engine uses a "command.<action_name>"
// message type convention (domain-specific message types can be added to
// Dispatch in a later task if needed) and the workflow instance id as the
// correlation id fallback (Instance has no dedicated CorrelationID field in
// Task 1's contracts).
func buildCommandEnvelope(workflowID, actionName string, action ActionRecord, dispatch Dispatch, now func() time.Time) messaging.Envelope {
	messageType := "command." + actionName
	env := messaging.NewEnvelope(messageType, workflowID, dispatch.Payload, now)
	env.WorkflowID = workflowID
	env.ActionName = actionName
	env.CommandID = action.CommandID
	env.IdempotencyKey = action.IdempotencyKey
	return env
}

// deadlineAt returns the absolute deadline for an action given its dispatch
// duration; a zero duration yields the start time.
func deadlineAt(now time.Time, duration time.Duration) time.Time {
	if duration <= 0 {
		return now
	}
	return now.Add(duration)
}

// newUUID generates a v4 UUID string. It mirrors messaging.newMessageID; a
// later refactor may export a shared UUID helper from the messaging or a
// dedicated id package. Keep self-contained here to avoid a cross-package
// change in this task.
func newUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failure is a process-level catastrophe; surface as panic
		// rather than silently producing a duplicate or empty id.
		panic(fmt.Errorf("generate command UUID: %w", err))
	}
	raw[6] = raw[6]&0x0f | 0x40 // version 4
	raw[8] = raw[8]&0x3f | 0x80 // variant 10
	s := hex.EncodeToString(raw[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
