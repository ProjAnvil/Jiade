package workflow

import (
	"context"
	"errors"

	"bank/internal/platform/messaging"
)

// Sentinel errors returned by Store implementations and Engine.Start/Prepare.
// Callers may use errors.Is to branch on a specific failure.
var (
	// ErrInstanceExists is returned when Create is called with a WorkflowID
	// that is already persisted.
	ErrInstanceExists = errors.New("workflow instance already exists")
	// ErrInstanceNotFound is returned when an instance lookup fails.
	ErrInstanceNotFound = errors.New("workflow instance not found")
	// ErrDefinitionNotFound is returned by Engine.Start when no Definition is
	// registered for the requested (Type, Version).
	ErrDefinitionNotFound = errors.New("workflow definition not registered")
	// ErrInvalidMessage is returned by Engine.ApplyResult when a result envelope
	// fails validation (wrong workflow/action/command/event-type or unexpected
	// action state). The workflow state is left unchanged.
	ErrInvalidMessage = errors.New("invalid result message")
)

// Store is the persistence boundary the Engine uses to create workflow
// instances and run atomic per-instance transactions. The concrete production
// implementation is a Postgres-backed Store (added in a later task); an
// in-memory implementation backs the unit tests in this package.
//
// Both methods honour context cancellation as dictated by the underlying
// driver; engine callers should pass a context with an appropriate deadline.
type Store interface {
	// Create persists a new Instance in StatusPreparing with the input copied
	// verbatim, CurrentAction=0, Revision=0, and returns a snapshot. It is
	// an error for req.WorkflowID to already exist.
	Create(ctx context.Context, req StartRequest) (Instance, error)

	// WithInstance loads the instance identified by id, locks it for the
	// duration of fn, and commits fn's Tx writes atomically when fn returns
	// nil — rolling back on error or panic. The Tx.Instance pointer is
	// stable for the lifetime of fn and reflects writes performed via
	// SaveInstance/SaveAction within the same callback (read-your-writes).
	WithInstance(ctx context.Context, id string, fn func(Tx) error) error
}

// Tx is the per-instance transactional unit of work handed to Engine code
// inside Store.WithInstance. All Tx writes commit together when the
// surrounding WithInstance callback returns nil and are otherwise discarded.
//
// Methods are intended to be composed: the Engine reads Instance(), performs
// computations, then issues SaveInstance/SaveAction/AppendOutbox/InsertInbox
// in whatever order the workflow step requires. None of the methods commit
// on their own — only the WithInstance return value does.
type Tx interface {
	// Instance returns the locked Instance, reflecting all prior writes
	// performed within this Tx. The caller MUST treat the value as read-only
	// and persist any mutation via SaveInstance (or SaveAction for the
	// Actions slice).
	Instance() *Instance

	// InsertInbox records an incoming envelope as processed for the given
	// consumer, returning inserted=true when the row is new (false on
	// duplicate, which is the at-least-once delivery dedup path). The
	// Engine uses this for idempotency dedup of result events in later
	// tasks; Task 3's Prepare does not call it directly.
	InsertInbox(consumer string, envelope messaging.Envelope) (inserted bool, err error)

	// SaveInstance persists the full Instance state — Status,
	// PreparedContext, CurrentAction, Revision, lease fields, and
	// LastError(Class). The Actions slice is persisted separately via
	// SaveAction; the Engine keeps the two in sync.
	SaveInstance(Instance) error

	// SaveAction upserts a single ActionRecord keyed by its Index within
	// the Instance's Actions slice.
	SaveAction(ActionRecord) error

	// AppendOutbox enqueues an envelope for at-least-once publishing once
	// the surrounding transaction commits. routingKey is the broker routing
	// key (typically derived from the Action's Dispatch.RoutingKey).
	AppendOutbox(envelope messaging.Envelope, routingKey string) error
}
