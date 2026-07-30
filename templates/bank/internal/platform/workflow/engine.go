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
	return e.store.WithInstance(ctx, id, func(tx Tx) error {
		current := tx.Instance()
		// Idempotent: if another worker advanced the instance between our
		// readHeader and this lock, leave it alone.
		if current.Status != StatusPreparing {
			return nil
		}

		inst := *current
		inst.PreparedContext = append(json.RawMessage(nil), preparedContext...)
		inst.Status = StatusRunning
		inst.CurrentAction = 0

		now := e.config.Now()
		inst.OperationalDeadline = now.Add(e.config.OperationalDeadline)
		inst.Revision++

		if err := tx.SaveInstance(inst); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}

		return e.persistActionDispatch(tx, inst, def, ctx)
	})
}

// persistActionDispatch executes the action at inst.CurrentAction, persists its
// ActionRecord (Status=waiting_result, Direction=forward, Attempt=1), and
// appends the dispatch command to the outbox. The caller MUST have already
// saved the Instance with the correct CurrentAction via tx.SaveInstance. This
// is the shared dispatch path for the first action (Prepare) and subsequent
// actions (ApplyResult advancement).
func (e *Engine) persistActionDispatch(tx Tx, inst Instance, def Definition, ctx context.Context) error {
	actions := def.Actions()
	idx := inst.CurrentAction
	action := actions[idx]

	view := View{Instance: inst, Action: ActionRecord{Index: idx, Name: action.Name()}}
	dispatch, err := action.Execute(ctx, view)
	if err != nil {
		// Propagate to caller for retry; the Tx rolls back.
		return fmt.Errorf("action %q Execute: %w", action.Name(), err)
	}

	now := e.config.Now()
	idempotencyKey := dispatch.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("%s:%s:%d", inst.ID, action.Name(), 1)
	}
	actionRec := ActionRecord{
		Index:               idx,
		Name:                action.Name(),
		Status:              ActionWaitingResult,
		Direction:           directionForward,
		Attempt:             1,
		IdempotencyKey:      idempotencyKey,
		CommandID:           newUUID(),
		DeadlineAt:          deadlineAt(now, dispatch.Deadline),
		AcceptedResultTypes: dispatch.AcceptedResultTypes,
	}
	if err := tx.SaveAction(actionRec); err != nil {
		return fmt.Errorf("save action %q: %w", action.Name(), err)
	}

	env := buildCommandEnvelope(inst.ID, action.Name(), actionRec, dispatch, e.config.Now)
	if err := tx.AppendOutbox(env, dispatch.RoutingKey); err != nil {
		return fmt.Errorf("append outbox for %q: %w", action.Name(), err)
	}
	return nil
}

// resultConsumer is the Inbox consumer name used by ApplyResult to deduplicate
// result event deliveries.
const resultConsumer = "workflow"

// ApplyResult consumes a result Envelope, applies it to the current action
// exactly once (Inbox dedup + row lock), and advances the workflow — the
// event-driven resume path complementing Engine.Start/Prepare. All work happens
// inside one Store.WithInstance transaction so concurrent duplicate deliveries
// are serialized by the row lock and deduplicated by the Inbox insert.
//
// Flow (all inside the per-instance Tx):
//  1. InsertInbox FIRST. If the event was already processed (duplicate), return
//     nil — idempotent, no re-advance.
//  2. Validate the envelope against the current action: workflow ID, action
//     name, command ID, accepted message type, and action status must all
//     match. Mismatch returns a wrapped ErrInvalidMessage and leaves state
//     unchanged (the Tx rolls back, including the Inbox insert).
//  3. Call the current Action's ApplyResult to obtain an Outcome.
//  4. On success: save the action output and mark it succeeded. If a next
//     action exists, dispatch it (CurrentAction++, next ActionRecord created,
//     command appended to outbox); otherwise mark the instance succeeded.
//     Bump instance Revision either way.
//  5. On failure (Outcome.Succeeded=false): record the action as failed but do
//     NOT trigger compensation — that is Task 5's scope.
func (e *Engine) ApplyResult(ctx context.Context, env messaging.Envelope) error {
	return e.store.WithInstance(ctx, env.WorkflowID, func(tx Tx) error {
		// 1. Inbox dedup FIRST. If already processed, this is an idempotent
		// no-op — return nil without re-advancing.
		inserted, err := tx.InsertInbox(resultConsumer, env)
		if err != nil {
			return fmt.Errorf("insert inbox: %w", err)
		}
		if !inserted {
			return nil
		}

		current := tx.Instance()

		// 2. Validate instance status.
		if current.Status != StatusRunning {
			return fmt.Errorf("%w: instance %q status %q, want %q",
				ErrInvalidMessage, current.ID, current.Status, StatusRunning)
		}
		// Defensive: envelope workflow must match the locked instance.
		if env.WorkflowID != current.ID {
			return fmt.Errorf("%w: envelope workflow %q != instance %q",
				ErrInvalidMessage, env.WorkflowID, current.ID)
		}

		// 3. Validate the current action record.
		actionIdx := current.CurrentAction
		if actionIdx < 0 || actionIdx >= len(current.Actions) {
			return fmt.Errorf("%w: current action index %d out of range (have %d actions)",
				ErrInvalidMessage, actionIdx, len(current.Actions))
		}
		actionRec := current.Actions[actionIdx]

		if env.ActionName != actionRec.Name {
			return fmt.Errorf("%w: envelope action %q != current action %q",
				ErrInvalidMessage, env.ActionName, actionRec.Name)
		}
		if env.CommandID != actionRec.CommandID {
			return fmt.Errorf("%w: envelope command_id %q != current command_id %q",
				ErrInvalidMessage, env.CommandID, actionRec.CommandID)
		}
		if !containsString(actionRec.AcceptedResultTypes, env.MessageType) {
			return fmt.Errorf("%w: message type %q not in accepted types %v",
				ErrInvalidMessage, env.MessageType, actionRec.AcceptedResultTypes)
		}
		if actionRec.Status != ActionWaitingResult {
			return fmt.Errorf("%w: action %q status %q, want %q",
				ErrInvalidMessage, actionRec.Name, actionRec.Status, ActionWaitingResult)
		}

		// 4. Look up the Definition + Action interface for this step.
		def, ok := e.registry.Get(current.Type, current.Version)
		if !ok {
			return fmt.Errorf("%w: type=%q version=%d",
				ErrDefinitionNotFound, current.Type, current.Version)
		}
		defActions := def.Actions()
		if actionIdx >= len(defActions) {
			return fmt.Errorf("%w: action index %d exceeds definition actions (%d)",
				ErrInvalidMessage, actionIdx, len(defActions))
		}
		action := defActions[actionIdx]

		// 5. Apply the result event to the action.
		view := View{Instance: *current, Action: actionRec}
		outcome, err := action.ApplyResult(ctx, view, env)
		if err != nil {
			return fmt.Errorf("action %q ApplyResult: %w", actionRec.Name, err)
		}

		inst := *current

		// 6. Handle failure outcome. Task 4 records the failure but does NOT
		// initiate compensation (running -> compensating is Task 5).
		if !outcome.Succeeded {
			actionRec.Status = ActionFailed
			actionRec.LastErrorClass = outcome.Class
			actionRec.LastError = outcome.Message
			actionRec.ResultEventID = env.MessageID
			if err := tx.SaveAction(actionRec); err != nil {
				return fmt.Errorf("save failed action %q: %w", actionRec.Name, err)
			}
			inst.Revision++
			return tx.SaveInstance(inst)
		}

		// 7. Success: record output and mark action succeeded.
		actionRec.Status = ActionSucceeded
		actionRec.Output = append(json.RawMessage(nil), outcome.Output...)
		actionRec.ResultEventID = env.MessageID
		if err := tx.SaveAction(actionRec); err != nil {
			return fmt.Errorf("save succeeded action %q: %w", actionRec.Name, err)
		}

		// 8. Advance: dispatch the next action, or mark the instance succeeded.
		if actionIdx+1 < len(defActions) {
			inst.CurrentAction = actionIdx + 1
			inst.Revision++
			if err := tx.SaveInstance(inst); err != nil {
				return fmt.Errorf("save instance: %w", err)
			}
			return e.persistActionDispatch(tx, inst, def, ctx)
		}

		// Last action succeeded — workflow is done.
		inst.Status = StatusSucceeded
		inst.Revision++
		return tx.SaveInstance(inst)
	})
}

// containsString reports whether list contains s. Used for AcceptedResultTypes
// membership checks.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
