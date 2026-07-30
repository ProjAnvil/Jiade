// Package workflow implements a domain-neutral durable linear workflow
// engine. An instance advances through an ordered list of actions; each action
// emits a command, awaits a result event, and may be compensated in reverse
// order on failure.
//
// This file defines only the public contracts: state and error enums, request
// and record structs, the Definition and Action interfaces, EngineConfig, and
// the associated Dispatch/Outcome value types. The engine itself, its store,
// and runtime behaviour live in other files.
package workflow

import (
	"context"
	"encoding/json"
	"time"

	"bank/internal/platform/messaging"
)

// ErrorClass categorises the outcome of an action attempt so the engine can
// decide whether to retry, compensate, or reject the instance.
type ErrorClass string

const (
	BusinessRejected   ErrorClass = "business_rejected"
	TransientFailure   ErrorClass = "transient_failure"
	UnknownOutcome     ErrorClass = "unknown_outcome"
	InvariantViolation ErrorClass = "invariant_violation"
	InvalidMessage     ErrorClass = "invalid_message"
)

// InstanceStatus is the lifecycle state of a workflow instance.
type InstanceStatus string

const (
	StatusPreparing          InstanceStatus = "preparing"
	StatusReady              InstanceStatus = "ready"
	StatusRunning            InstanceStatus = "running"
	StatusSucceeded          InstanceStatus = "succeeded"
	StatusRejected           InstanceStatus = "rejected"
	StatusCompensating       InstanceStatus = "compensating"
	StatusCompensated        InstanceStatus = "compensated"
	StatusCompensationFailed InstanceStatus = "compensation_failed"
)

// ActionStatus is the lifecycle state of a single action within an instance.
type ActionStatus string

const (
	ActionPending            ActionStatus = "pending"
	ActionWaitingResult      ActionStatus = "waiting_result"
	ActionSucceeded          ActionStatus = "succeeded"
	ActionFailed             ActionStatus = "failed"
	ActionCompensating       ActionStatus = "compensating"
	ActionCompensated        ActionStatus = "compensated"
	ActionCompensationFailed ActionStatus = "compensation_failed"
)

// StartRequest carries the input needed to create a new workflow instance.
type StartRequest struct {
	WorkflowID    string
	Type          string
	Version       int
	Input         json.RawMessage
	CorrelationID string
	CreatedAt     time.Time
}

// Instance is the persisted state of a single workflow execution. The
// PreparedContext is immutable once written; progress is recorded via Status,
// CurrentAction, Revision, leases, and the Actions slice.
type Instance struct {
	ID                  string
	Type                string
	Version             int
	Status              InstanceStatus
	Input               json.RawMessage
	PreparedContext     json.RawMessage
	CurrentAction       int
	Revision            int64
	LeaseOwner          string
	LeaseUntil          time.Time
	NextWakeupAt        time.Time
	OperationalDeadline time.Time
	LastErrorClass      ErrorClass
	LastError           string
	Actions             []ActionRecord
}

// ActionRecord is the persisted state of one step within an instance.
type ActionRecord struct {
	Index          int
	Name           string
	Status         ActionStatus
	Direction      string
	Attempt        int
	IdempotencyKey string
	CommandID      string
	ResultEventID  string
	DeadlineAt     time.Time
	Output         json.RawMessage
	LastErrorClass ErrorClass
	LastError      string
}

// View is the immutable snapshot handed to an Action's Execute/Apply methods.
// The engine guarantees the Instance and Action fields are not mutated by
// calling code.
type View struct {
	Instance Instance
	Action   ActionRecord
}

// Dispatch describes a command the engine must emit on behalf of an action.
type Dispatch struct {
	RoutingKey          string
	Payload             json.RawMessage
	AcceptedResultTypes []string
	Deadline            time.Duration
	IdempotencyKey      string
}

// Outcome is the result of applying a result event to an action. When
// Succeeded is false, Class and Message describe why.
type Outcome struct {
	Succeeded bool
	Class     ErrorClass
	Output    json.RawMessage
	Message   string
}

// DefinitionRef identifies a registered workflow definition by type+version.
type DefinitionRef struct {
	Type    string
	Version int
}

// Definition is the contract a workflow author implements and registers with
// the engine. Prepare produces an immutable context; Actions returns the
// ordered steps executed left-to-right (and compensated right-to-left).
type Definition interface {
	Type() string
	Version() int
	Prepare(context.Context, json.RawMessage) (json.RawMessage, error)
	Actions() []Action
}

// Action is one step of a workflow. Execute emits the forward command;
// ApplyResult ingests the result event and yields an Outcome. Compensate and
// ApplyCompensationResult mirror those for the reverse (undo) direction.
type Action interface {
	Name() string
	Execute(context.Context, View) (Dispatch, error)
	ApplyResult(context.Context, View, messaging.Envelope) (Outcome, error)
	Compensate(context.Context, View) (Dispatch, error)
	ApplyCompensationResult(context.Context, View, messaging.Envelope) (Outcome, error)
}

// EngineConfig tunes engine retry and deadline policy. Zero values yield the
// documented defaults: three execution attempts, five compensation attempts,
// and a two-minute operational deadline. Crossing the operational deadline
// records an observable error and schedules a wake-up; it never deletes or
// abandons an instance.
type EngineConfig struct {
	ExecuteMaxAttempts      int
	CompensationMaxAttempts int
	OperationalDeadline     time.Duration
	Now                     func() time.Time
}
