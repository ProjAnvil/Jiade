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
// exactly once (Inbox dedup + row lock), and advances or compensates the
// workflow — the event-driven resume path complementing Engine.Start/Prepare.
// All work happens inside one Store.WithInstance transaction so concurrent
// duplicate deliveries are serialized by the row lock and deduplicated by the
// Inbox insert.
//
// ApplyResult is the SINGLE entry point for both forward results and
// compensation results: compensation commands emit result events through the
// same messaging consumer, so routing is by Instance state —
//
//   - StatusRunning      → forward-result path (applyForwardResult)
//   - StatusCompensating → compensation-result path (applyCompensationResult)
//
// Any other Instance status yields ErrInvalidMessage. This is the documented,
// testable routing mechanism: no envelope-direction or command-id heuristic is
// needed because the Instance can only be in one phase at a time.
//
// Flow (all inside the per-instance Tx):
//  1. InsertInbox FIRST. If the event was already processed (duplicate), return
//     nil — idempotent, no re-advance.
//  2. Defensive workflow-ID check.
//  3. Route by Instance status to the forward or compensation handler.
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

		// 2. Defensive: envelope workflow must match the locked instance.
		if env.WorkflowID != current.ID {
			return fmt.Errorf("%w: envelope workflow %q != instance %q",
				ErrInvalidMessage, env.WorkflowID, current.ID)
		}

		// 3. Route by Instance status to the forward or compensation handler.
		switch current.Status {
		case StatusRunning:
			return e.applyForwardResult(tx, current, env, ctx)
		case StatusCompensating:
			return e.applyCompensationResult(tx, current, env, ctx)
		default:
			return fmt.Errorf("%w: instance %q status %q, want %q or %q",
				ErrInvalidMessage, current.ID, current.Status,
				StatusRunning, StatusCompensating)
		}
	})
}

// applyForwardResult handles a result envelope for an Instance in
// StatusRunning. It validates the envelope against the current action, calls
// Action.ApplyResult, and either advances to the next action (success),
// triggers reverse compensation (terminal failure), or leaves the action
// failed for Task 6 recovery (transient failure).
func (e *Engine) applyForwardResult(tx Tx, current *Instance, env messaging.Envelope, ctx context.Context) error {
	// Validate the current action record.
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

	// Look up the Definition + Action interface for this step.
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

	// Apply the result event to the action.
	view := View{Instance: *current, Action: actionRec}
	outcome, err := action.ApplyResult(ctx, view, env)
	if err != nil {
		return fmt.Errorf("action %q ApplyResult: %w", actionRec.Name, err)
	}

	inst := *current

	// Handle failure outcome.
	if !outcome.Succeeded {
		// Record the failure on the action record regardless of class.
		actionRec.Status = ActionFailed
		actionRec.LastErrorClass = outcome.Class
		actionRec.LastError = outcome.Message
		actionRec.ResultEventID = env.MessageID
		if err := tx.SaveAction(actionRec); err != nil {
			return fmt.Errorf("save failed action %q: %w", actionRec.Name, err)
		}

		// Terminal execution failures trigger reverse compensation.
		// Transient failures and unknown outcomes (timeouts) are left for
		// Task 6's recovery path: the action stays failed and the instance
		// stays running so a later task can re-dispatch or escalate.
		if isTerminalExecutionFailure(outcome.Class) {
			return e.beginCompensation(tx, inst, def, ctx)
		}
		// Transient: record state, leave instance running for Task 6.
		inst.Revision++
		return tx.SaveInstance(inst)
	}

	// Success: record output and mark action succeeded.
	actionRec.Status = ActionSucceeded
	actionRec.Output = append(json.RawMessage(nil), outcome.Output...)
	actionRec.ResultEventID = env.MessageID
	if err := tx.SaveAction(actionRec); err != nil {
		return fmt.Errorf("save succeeded action %q: %w", actionRec.Name, err)
	}

	// Advance: dispatch the next action, or mark the instance succeeded.
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
}

// isTerminalExecutionFailure reports whether an ErrorClass from a failed
// forward ApplyResult outcome is terminal — meaning the action cannot succeed
// on retry and the engine must reverse (compensate) the workflow.
//
// Terminal classes (trigger compensation):
//   - BusinessRejected: the domain has rejected the operation; retrying the
//     same command will not change the outcome.
//   - InvariantViolation: a precondition or invariant was violated; this is a
//     logic-level rejection, not a transient infra fault.
//
// Non-terminal classes (left for Task 6 recovery, instance stays running):
//   - TransientFailure: a downstream/broker fault that may resolve on retry.
//   - UnknownOutcome: a timeout/no-answer that recovery can re-dispatch.
//   - InvalidMessage: structural rejection handled earlier by the engine.
func isTerminalExecutionFailure(class ErrorClass) bool {
	switch class {
	case BusinessRejected, InvariantViolation:
		return true
	default:
		return false
	}
}

// beginCompensation transitions an Instance from running to compensating and
// dispatches the FIRST compensation command — for the last succeeded Action
// before the failed current action (compensation walks in REVERSE order). The
// caller has already persisted the failed ActionRecord; this function only
// mutates Instance state and the newly-targeted compensation ActionRecord.
//
// If no prior action succeeded (e.g. a 1-step workflow whose only action
// failed), there is nothing to undo and the instance jumps straight to
// StatusCompensated.
func (e *Engine) beginCompensation(tx Tx, inst Instance, def Definition, ctx context.Context) error {
	inst.Status = StatusCompensating
	target := lastSucceededBefore(inst, inst.CurrentAction)
	if target < 0 {
		// Nothing to undo.
		inst.Status = StatusCompensated
		inst.Revision++
		return tx.SaveInstance(inst)
	}
	inst.CurrentAction = target
	inst.Revision++
	if err := tx.SaveInstance(inst); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}
	return e.persistCompensationDispatch(tx, inst, def, target, 1, ctx)
}

// lastSucceededBefore returns the index of the most-recent Action whose Status
// is ActionSucceeded and whose Index is strictly less than before, or -1 if
// none exists. Used to walk the compensation stack in reverse order.
func lastSucceededBefore(inst Instance, before int) int {
	for i := before - 1; i >= 0; i-- {
		if i < len(inst.Actions) && inst.Actions[i].Status == ActionSucceeded {
			return i
		}
	}
	return -1
}

// persistCompensationDispatch builds and persists a compensation command for
// the action at idx: calls Action.Compensate, saves the ActionRecord as
// ActionCompensating with Direction=compensation, and AppendOutbox-es the
// compensation command. The compensation IdempotencyKey is STABLE across
// retry attempts (semantic identity); the CommandID is fresh on each dispatch
// (transport identity). The caller MUST have already set inst.CurrentAction
// and saved the Instance.
func (e *Engine) persistCompensationDispatch(tx Tx, inst Instance, def Definition, idx, attempt int, ctx context.Context) error {
	actions := def.Actions()
	action := actions[idx]

	// View carries the CURRENT action record (still succeeded from the forward
	// pass) so Compensate can read the forward Output to construct the undo.
	view := View{Instance: inst, Action: inst.Actions[idx]}
	dispatch, err := action.Compensate(ctx, view)
	if err != nil {
		return fmt.Errorf("action %q Compensate: %w", action.Name(), err)
	}

	now := e.config.Now()
	idempotencyKey := dispatch.IdempotencyKey
	if idempotencyKey == "" {
		// Stable semantic key: same across compensation retries of this action.
		idempotencyKey = fmt.Sprintf("%s:%s:compensate", inst.ID, action.Name())
	}

	oldRec := inst.Actions[idx]
	actionRec := ActionRecord{
		Index:               idx,
		Name:                action.Name(),
		Status:              ActionCompensating,
		Direction:           directionCompensation,
		Attempt:             attempt,
		IdempotencyKey:      idempotencyKey,
		CommandID:           newUUID(),
		DeadlineAt:          deadlineAt(now, dispatch.Deadline),
		AcceptedResultTypes: dispatch.AcceptedResultTypes,
		// Preserve forward provenance for audit and for Compensate on retry.
		Output:        append(json.RawMessage(nil), oldRec.Output...),
		ResultEventID: oldRec.ResultEventID,
	}
	if err := tx.SaveAction(actionRec); err != nil {
		return fmt.Errorf("save compensation action %q: %w", action.Name(), err)
	}

	env := buildCompensationEnvelope(inst.ID, action.Name(), actionRec, dispatch, e.config.Now)
	if err := tx.AppendOutbox(env, dispatch.RoutingKey); err != nil {
		return fmt.Errorf("append outbox for compensation %q: %w", action.Name(), err)
	}
	return nil
}

// applyCompensationResult handles a compensation-result envelope for an
// Instance in StatusCompensating. It validates the envelope against the
// current action (which is in ActionCompensating), calls
// Action.ApplyCompensationResult, and either walks to the previous succeeded
// action (compensation success), retries the same action (transient failure,
// stable idempotency key), or marks compensation_failed after
// CompensationMaxAttempts transient failures.
func (e *Engine) applyCompensationResult(tx Tx, current *Instance, env messaging.Envelope, ctx context.Context) error {
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
	if actionRec.Status != ActionCompensating {
		return fmt.Errorf("%w: action %q status %q, want %q",
			ErrInvalidMessage, actionRec.Name, actionRec.Status, ActionCompensating)
	}

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

	view := View{Instance: *current, Action: actionRec}
	outcome, err := action.ApplyCompensationResult(ctx, view, env)
	if err != nil {
		return fmt.Errorf("action %q ApplyCompensationResult: %w", actionRec.Name, err)
	}

	inst := *current

	// Transient compensation failure: retry same semantic idempotency key.
	if !outcome.Succeeded {
		if actionRec.Attempt >= e.config.CompensationMaxAttempts {
			// Exhausted: mark action and instance compensation_failed.
			// CurrentAction is preserved so the operator can see which step
			// could not be undone. (The failure metric is Task 8.)
			actionRec.Status = ActionCompensationFailed
			actionRec.LastErrorClass = outcome.Class
			actionRec.LastError = outcome.Message
			actionRec.ResultEventID = env.MessageID
			if err := tx.SaveAction(actionRec); err != nil {
				return fmt.Errorf("save compensation_failed action %q: %w", actionRec.Name, err)
			}
			inst.Status = StatusCompensationFailed
			inst.LastErrorClass = outcome.Class
			inst.LastError = outcome.Message
			inst.Revision++
			return tx.SaveInstance(inst)
		}
		// Retry: same idempotency key, fresh CommandID, attempt+1.
		return e.persistCompensationDispatch(tx, inst, def, actionIdx, actionRec.Attempt+1, ctx)
	}

	// Compensation success: mark action compensated.
	actionRec.Status = ActionCompensated
	actionRec.LastErrorClass = ""
	actionRec.LastError = ""
	actionRec.ResultEventID = env.MessageID
	if err := tx.SaveAction(actionRec); err != nil {
		return fmt.Errorf("save compensated action %q: %w", actionRec.Name, err)
	}

	// Walk to the previous succeeded action, or finish.
	target := lastSucceededBefore(inst, actionIdx)
	if target < 0 {
		inst.Status = StatusCompensated
		inst.Revision++
		return tx.SaveInstance(inst)
	}
	inst.CurrentAction = target
	inst.Revision++
	if err := tx.SaveInstance(inst); err != nil {
		return fmt.Errorf("save instance: %w", err)
	}
	return e.persistCompensationDispatch(tx, inst, def, target, 1, ctx)
}

// buildCompensationEnvelope assembles the messaging.Envelope for a compensation
// dispatch command. Mirrors buildCommandEnvelope but uses the
// "compensation.<action_name>" message-type prefix so consumers can distinguish
// undo commands from forward commands at the transport layer.
func buildCompensationEnvelope(workflowID, actionName string, action ActionRecord, dispatch Dispatch, now func() time.Time) messaging.Envelope {
	messageType := "compensation." + actionName
	env := messaging.NewEnvelope(messageType, workflowID, dispatch.Payload, now)
	env.WorkflowID = workflowID
	env.ActionName = actionName
	env.CommandID = action.CommandID
	env.IdempotencyKey = action.IdempotencyKey
	return env
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
