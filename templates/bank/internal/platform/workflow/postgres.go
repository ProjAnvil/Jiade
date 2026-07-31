package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
)

// PostgresStore is the production Store implementation backed by PostgreSQL.
// All per-instance writes occur inside a single transaction that takes a
// row-level lock (SELECT ... FOR UPDATE) on the workflow_instance row, so the
// engine's Tx writes commit atomically and concurrent callers serialize per
// instance.
//
// PostgresStore values are safe for concurrent use: they hold no mutable state
// of their own, and all per-instance state is guarded by PostgreSQL row locks.
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore binds a *sql.DB to a PostgresStore. The caller owns the
// *sql.DB lifecycle (open/close/ping); the store only borrows the pool.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// Compile-time assertion that PostgresStore satisfies the full Store
// interface (Create, WithInstance, and the 5 recovery methods).
var _ Store = (*PostgresStore)(nil)

// ---------------------------------------------------------------------------
// Load helpers (shared by WithInstance and tests).
//
// scanInstanceRow/scanActionRow live in postgres_integration_test.go but use
// the rowScanner interface defined there; here we declare the same minimal
// interface so the production code can scan both *sql.Row and *sql.Rows.
// ---------------------------------------------------------------------------

type pgRowScanner interface {
	Scan(dest ...any) error
}

// loadInstanceRow loads a workflow_instance row by id with a FOR UPDATE lock
// and all its workflow_action rows ordered by action_index. The lock is
// released when the surrounding transaction commits or rolls back. Returns
// ErrInstanceNotFound when no instance row exists.
func (s *PostgresStore) loadInstanceRow(ctx context.Context, q pg.DBTX, id string) (Instance, error) {
	row := q.QueryRowContext(ctx, `
		SELECT workflow_id, type, definition_version, status, input_json,
		       prepared_context_json, current_action, revision, lease_owner,
		       lease_until, next_wakeup_at, operational_deadline,
		       last_error_class, last_error, correlation_id
		FROM workflow_instance
		WHERE workflow_id = $1
		FOR UPDATE`, id)
	inst, err := scanInstanceRowPG(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, fmt.Errorf("%w: id=%s", ErrInstanceNotFound, id)
		}
		return Instance{}, fmt.Errorf("load instance %q: %w", id, err)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT action_index, name, status, direction, attempt, idempotency_key,
		       command_id, result_event_id, deadline_at, output,
		       last_error_class, last_error, accepted_result_types
		FROM workflow_action
		WHERE workflow_id = $1
		ORDER BY action_index`, id)
	if err != nil {
		return Instance{}, fmt.Errorf("load actions for %q: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanActionRowPG(rows)
		if err != nil {
			return Instance{}, fmt.Errorf("scan action for %q: %w", id, err)
		}
		inst.Actions = append(inst.Actions, rec)
	}
	if err := rows.Err(); err != nil {
		return Instance{}, fmt.Errorf("iter actions for %q: %w", id, err)
	}
	return inst, nil
}

// scanInstanceRowPG populates an Instance from a single workflow_instance row.
// Reused by the integration tests' loadInstanceDirect helper (which reads the
// committed state outside any Store transaction).
func scanInstanceRowPG(s pgRowScanner) (Instance, error) {
	var (
		inst           Instance
		input          []byte
		preparedCtx    []byte
		leaseOwner     sql.NullString
		leaseUntil     sql.NullTime
		nextWakeup     sql.NullTime
		operational    sql.NullTime
		lastErrorClass sql.NullString
		lastError      sql.NullString
		correlationID  sql.NullString
	)
	if err := s.Scan(
		&inst.ID, &inst.Type, &inst.Version, &inst.Status, &input, &preparedCtx,
		&inst.CurrentAction, &inst.Revision, &leaseOwner, &leaseUntil, &nextWakeup,
		&operational, &lastErrorClass, &lastError, &correlationID,
	); err != nil {
		return Instance{}, err
	}
	inst.Input = append(json.RawMessage(nil), input...)
	if len(preparedCtx) > 0 {
		inst.PreparedContext = append(json.RawMessage(nil), preparedCtx...)
	}
	if leaseOwner.Valid {
		inst.LeaseOwner = leaseOwner.String
	}
	if leaseUntil.Valid {
		inst.LeaseUntil = leaseUntil.Time
	}
	if nextWakeup.Valid {
		inst.NextWakeupAt = nextWakeup.Time
	}
	if operational.Valid {
		inst.OperationalDeadline = operational.Time
	}
	if lastErrorClass.Valid {
		inst.LastErrorClass = ErrorClass(lastErrorClass.String)
	}
	if lastError.Valid {
		inst.LastError = lastError.String
	}
	if correlationID.Valid {
		inst.CorrelationID = correlationID.String
	}
	return inst, nil
}

// scanActionRowPG populates an ActionRecord from a single workflow_action row.
func scanActionRowPG(s pgRowScanner) (ActionRecord, error) {
	var (
		rec           ActionRecord
		status        string
		direction     string
		idemKey       string
		commandID     sql.NullString
		resultID      sql.NullString
		deadlineAt    sql.NullTime
		output        []byte
		errClass      sql.NullString
		errMsg        sql.NullString
		acceptedTypes []byte // nullable jsonb; NULL → nil slice
	)
	if err := s.Scan(
		&rec.Index, &rec.Name, &status, &direction, &rec.Attempt, &idemKey,
		&commandID, &resultID, &deadlineAt, &output, &errClass, &errMsg,
		&acceptedTypes,
	); err != nil {
		return ActionRecord{}, err
	}
	rec.Status = ActionStatus(status)
	rec.Direction = direction
	rec.IdempotencyKey = idemKey
	if commandID.Valid {
		rec.CommandID = commandID.String
	}
	if resultID.Valid {
		rec.ResultEventID = resultID.String
	}
	if deadlineAt.Valid {
		rec.DeadlineAt = deadlineAt.Time
	}
	if len(output) > 0 {
		rec.Output = append(json.RawMessage(nil), output...)
	}
	if errClass.Valid {
		rec.LastErrorClass = ErrorClass(errClass.String)
	}
	if errMsg.Valid {
		rec.LastError = errMsg.String
	}
	if len(acceptedTypes) > 0 {
		// accepted_result_types is a JSON array of strings; an Unmarshal
		// failure is fatal — it indicates schema corruption.
		if err := json.Unmarshal(acceptedTypes, &rec.AcceptedResultTypes); err != nil {
			return ActionRecord{}, fmt.Errorf(
				"decode accepted_result_types for action %q: %w", rec.Name, err)
		}
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Create + CreateInTx.
//
// CreateInTx inserts a new workflow_instance row using the caller-supplied
// *sql.Tx so payment code can persist its intent + the workflow instance in
// one transaction. Create opens its own transaction via pg.RunInTx and
// delegates to CreateInTx.
// ---------------------------------------------------------------------------

// Create persists a new Instance in StatusPreparing with CurrentAction=0,
// Revision=0, and Input copied verbatim. It returns ErrInstanceExists when
// req.WorkflowID already exists.
func (s *PostgresStore) Create(ctx context.Context, req StartRequest) (Instance, error) {
	var inst Instance
	err := pg.RunInTx(ctx, s.db, func(q pg.DBTX) error {
		tx, ok := q.(*sql.Tx)
		if !ok {
			// pg.RunInTx always passes its internally-begun *sql.Tx as the
			// pg.DBTX argument; defend against future divergence regardless.
			return fmt.Errorf("PostgresStore.Create: pg.DBTX is %T, want *sql.Tx", q)
		}
		var err error
		inst, err = s.CreateInTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return Instance{}, err
	}
	return inst, nil
}

// CreateInTx inserts a new Instance inside the caller's *sql.Tx. The caller is
// responsible for Commit/Rollback. Empty Input is normalized to `{}` so the
// `jsonb_typeof(input_json) = 'object'` CHECK constraint cannot fail on a nil
// or empty payload.
func (s *PostgresStore) CreateInTx(ctx context.Context, tx *sql.Tx, req StartRequest) (Instance, error) {
	input := req.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	// ON CONFLICT DO NOTHING + RETURNING lets us distinguish "inserted" from
	// "duplicate existed" without parsing driver-specific unique-violation
	// errors. QueryRow returns sql.ErrNoRows when the conflict path fires.
	var insertedID string
	err := tx.QueryRowContext(ctx, `
		INSERT INTO workflow_instance
		  (workflow_id, type, definition_version, status, input_json,
		   current_action, revision, correlation_id, created_at, updated_at)
		VALUES ($1, $2, $3, 'preparing', $4, 0, 0, $5, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (workflow_id) DO NOTHING
		RETURNING workflow_id`,
		req.WorkflowID, req.Type, req.Version, []byte(input), req.CorrelationID,
	).Scan(&insertedID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, fmt.Errorf("%w: id=%s", ErrInstanceExists, req.WorkflowID)
		}
		return Instance{}, fmt.Errorf("insert workflow_instance %q: %w", req.WorkflowID, err)
	}
	return Instance{
		ID:            req.WorkflowID,
		Type:          req.Type,
		Version:       req.Version,
		Status:        StatusPreparing,
		Input:         append(json.RawMessage(nil), input...),
		CorrelationID: req.CorrelationID,
	}, nil
}

// ---------------------------------------------------------------------------
// WithInstance: lock, run callback, commit/rollback atomically.
// ---------------------------------------------------------------------------

// WithInstance loads the instance identified by id (acquiring a row lock),
// runs fn against a pgTx whose writes are buffered into the in-flight tx, and
// commits when fn returns nil — rolling back on error or panic. The Tx.Instance
// pointer is stable for the lifetime of fn and reflects writes performed via
// SaveInstance/SaveAction within the same callback (read-your-writes).
func (s *PostgresStore) WithInstance(ctx context.Context, id string, fn func(Tx) error) (err error) {
	tx, beginErr := s.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return fmt.Errorf("begin tx for %q: %w", id, beginErr)
	}
	// Deferred rollback: runs on every error return AND on panic unwinding
	// the stack. After Commit succeeds, Rollback returns sql.ErrTxDone (a
	// harmless no-op); the discarded error is intentional. We don't need a
	// separate "committed" flag.
	defer func() { _ = tx.Rollback() }()
	if err = s.runInstanceLocked(ctx, tx, id, fn); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit tx for %q: %w", id, err)
	}
	return nil
}

// WithInstanceTx runs fn against the instance identified by id, locked within
// the caller-supplied *sql.Tx. Unlike WithInstance it does NOT commit — the
// caller owns the transaction (typically via pg.RunInTx) and is responsible for
// commit/rollback. This lets a caller persist its own rows (e.g. an immutable
// operator-audit row) atomically with the instance state change: either both
// commit or both roll back.
//
// The caller's *sql.Tx must still be open. loadInstanceRow acquires the row
// lock (SELECT ... FOR UPDATE) within that tx, so concurrent WithInstance /
// WithInstanceTx callers on the same instance serialize as usual.
func (s *PostgresStore) WithInstanceTx(ctx context.Context, tx *sql.Tx, id string, fn func(Tx) error) error {
	return s.runInstanceLocked(ctx, tx, id, fn)
}

// runInstanceLocked is the shared lock-and-run body used by both WithInstance
// (which owns its tx) and WithInstanceTx (which borrows the caller's tx). It
// loads the instance row with a FOR UPDATE lock, wraps it in a pgTx, and runs
// fn. It neither begins nor commits a transaction.
func (s *PostgresStore) runInstanceLocked(ctx context.Context, tx *sql.Tx, id string, fn func(Tx) error) error {
	inst, err := s.loadInstanceRow(ctx, tx, id)
	if err != nil {
		return err
	}
	// inst is heap-escaped once &inst flows into pgTx; Go handles this
	// transparently. The pointer is stable for the lifetime of fn so
	// read-your-writes works as the Tx contract requires.
	pTx := &pgTx{store: s, tx: tx, inst: &inst, loadedRevision: inst.Revision}
	return fn(pTx)
}

// pgTx is the per-instance transactional unit handed to Engine code inside
// PostgresStore.WithInstance. None of its methods commit on their own — only
// the WithInstance return value commits (or rolls back).
type pgTx struct {
	store *PostgresStore
	tx    *sql.Tx
	inst  *Instance // working copy; mutated by SaveInstance/SaveAction

	// loadedRevision is the instance.Revision observed at load time. SaveInstance
	// writes `WHERE revision = loadedRevision` as an optimistic-lock defence:
	// even though SELECT FOR UPDATE already serializes concurrent transactions,
	// a future refactor that drops the row lock must still refuse to clobber a
	// concurrently-modified row.
	loadedRevision int64
}

// Instance returns the locked Instance, reflecting all prior writes performed
// within this Tx.
func (t *pgTx) Instance() *Instance { return t.inst }

// InsertInbox records an incoming envelope as processed for the given
// consumer. Returns inserted=true when the row is new; false on duplicate
// (the at-least-once delivery dedup path).
//
// SQL: INSERT ... ON CONFLICT (consumer, message_id) DO NOTHING RETURNING
// message_id. The RETURNING clause is empty on conflict, which the driver
// surfaces as sql.ErrNoRows.
func (t *pgTx) InsertInbox(consumer string, env messaging.Envelope) (bool, error) {
	var returned string
	err := t.tx.QueryRowContext(context.Background(), `
		INSERT INTO inbox_message (consumer, message_id, message_type, processed_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (consumer, message_id) DO NOTHING
		RETURNING message_id`,
		consumer, env.MessageID, env.MessageType,
	).Scan(&returned)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("insert inbox %q/%s: %w", consumer, env.MessageID, err)
	}
	return true, nil
}

// SaveInstance persists the full Instance state — Status, PreparedContext,
// CurrentAction, Revision, lease fields, and LastError(Class). The update
// asserts the on-disk revision still matches the value loaded at the start of
// this Tx (loadedRevision); zero rows affected surfaces a conflict error so a
// concurrent modification cannot be silently overwritten.
func (t *pgTx) SaveInstance(inst Instance) error {
	var (
		leaseOwner  sql.NullString
		leaseUntil  sql.NullTime
		nextWakeup  sql.NullTime
		operational sql.NullTime
		errClass    sql.NullString
		errMsg      sql.NullString
	)
	if inst.LeaseOwner != "" {
		leaseOwner = sql.NullString{String: inst.LeaseOwner, Valid: true}
	}
	if !inst.LeaseUntil.IsZero() {
		leaseUntil = sql.NullTime{Time: inst.LeaseUntil, Valid: true}
	}
	if !inst.NextWakeupAt.IsZero() {
		nextWakeup = sql.NullTime{Time: inst.NextWakeupAt, Valid: true}
	}
	if !inst.OperationalDeadline.IsZero() {
		operational = sql.NullTime{Time: inst.OperationalDeadline, Valid: true}
	}
	if inst.LastErrorClass != "" {
		errClass = sql.NullString{String: string(inst.LastErrorClass), Valid: true}
	}
	if inst.LastError != "" {
		errMsg = sql.NullString{String: inst.LastError, Valid: true}
	}

	var preparedCtx any
	if len(inst.PreparedContext) > 0 {
		preparedCtx = []byte(inst.PreparedContext)
	}

	res, err := t.tx.ExecContext(context.Background(), `
		UPDATE workflow_instance SET
		  status = $3,
		  prepared_context_json = $4,
		  current_action = $5,
		  revision = $6,
		  lease_owner = $7,
		  lease_until = $8,
		  next_wakeup_at = $9,
		  operational_deadline = $10,
		  last_error_class = $11,
		  last_error = $12,
		  correlation_id = $13,
		  updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $1 AND revision = $2`,
		inst.ID, t.loadedRevision,
		string(inst.Status), preparedCtx, inst.CurrentAction, inst.Revision,
		leaseOwner, leaseUntil, nextWakeup, operational, errClass, errMsg, inst.CorrelationID,
	)
	if err != nil {
		return fmt.Errorf("update workflow_instance %q: %w", inst.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for %q: %w", inst.ID, err)
	}
	if n != 1 {
		return fmt.Errorf(
			"update workflow_instance %q: %d rows affected (optimistic revision conflict; loaded=%d, attempted=%d)",
			inst.ID, n, t.loadedRevision, inst.Revision)
	}
	// Read-your-writes: subsequent Instance()/SaveInstance calls observe the
	// freshly-persisted state.
	t.loadedRevision = inst.Revision
	*t.inst = inst
	return nil
}

// SaveAction upserts a single ActionRecord keyed by (workflow_id, action_index).
// On conflict it preserves a previously-stored forward Output via
// COALESCE(EXCLUDED.output, workflow_action.output) so a partial update
// (e.g. a compensation transition that forgets to carry Output) cannot NULL
// out the recorded forward output — the Task 5 aliasing concern.
func (t *pgTx) SaveAction(rec ActionRecord) error {
	var (
		commandID  sql.NullString
		resultID   sql.NullString
		deadlineAt sql.NullTime
		errClass   sql.NullString
		errMsg     sql.NullString
		output     any // nil → NULL
	)
	if rec.CommandID != "" {
		commandID = sql.NullString{String: rec.CommandID, Valid: true}
	}
	if rec.ResultEventID != "" {
		resultID = sql.NullString{String: rec.ResultEventID, Valid: true}
	}
	if !rec.DeadlineAt.IsZero() {
		deadlineAt = sql.NullTime{Time: rec.DeadlineAt, Valid: true}
	}
	if rec.LastErrorClass != "" {
		errClass = sql.NullString{String: string(rec.LastErrorClass), Valid: true}
	}
	if rec.LastError != "" {
		errMsg = sql.NullString{String: rec.LastError, Valid: true}
	}
	if len(rec.Output) > 0 {
		output = []byte(rec.Output)
	}

	// accepted_result_types is a JSON array; persist `[]` (not NULL) so reads
	// round-trip a non-nil empty slice. Marshal of a nil slice yields "null"
	// which we'd rather avoid for an action-metadata column.
	acceptedTypes := []byte("[]")
	if len(rec.AcceptedResultTypes) > 0 {
		b, err := json.Marshal(rec.AcceptedResultTypes)
		if err != nil {
			return fmt.Errorf("marshal accepted_result_types for %q: %w", rec.Name, err)
		}
		acceptedTypes = b
	}

	res, err := t.tx.ExecContext(context.Background(), `
		INSERT INTO workflow_action
		  (workflow_id, action_index, name, status, direction, attempt,
		   idempotency_key, command_id, result_event_id, deadline_at,
		   output, last_error_class, last_error, accepted_result_types,
		   created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT (workflow_id, action_index) DO UPDATE SET
		  name = EXCLUDED.name,
		  status = EXCLUDED.status,
		  direction = EXCLUDED.direction,
		  attempt = EXCLUDED.attempt,
		  idempotency_key = EXCLUDED.idempotency_key,
		  command_id = EXCLUDED.command_id,
		  result_event_id = EXCLUDED.result_event_id,
		  deadline_at = EXCLUDED.deadline_at,
		  -- Preserve forward Output if the incoming record omits it. This
		  -- guards against the Task 5 aliasing concern where a compensation
		  -- transition's partial update could NULL the recorded forward output.
		  output = COALESCE(EXCLUDED.output, workflow_action.output),
		  last_error_class = EXCLUDED.last_error_class,
		  last_error = EXCLUDED.last_error,
		  accepted_result_types = EXCLUDED.accepted_result_types,
		  updated_at = CURRENT_TIMESTAMP`,
		t.inst.ID, rec.Index, rec.Name, string(rec.Status), rec.Direction, rec.Attempt,
		rec.IdempotencyKey, commandID, resultID, deadlineAt, output, errClass, errMsg,
		[]byte(acceptedTypes),
	)
	if err != nil {
		return fmt.Errorf("upsert workflow_action[%d] for %q: %w",
			rec.Index, t.inst.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for action[%d]: %w", rec.Index, err)
	}
	if n != 1 {
		return fmt.Errorf("upsert workflow_action[%d] for %q: %d rows affected",
			rec.Index, t.inst.ID, n)
	}

	// Read-your-writes: update the Actions slice in the working Instance so a
	// subsequent Instance() reflects the new state. We must NOT mutate the
	// pointer's underlying Instance if SaveInstance was never called for the
	// revision bump — but the engine always pairs SaveInstance with SaveAction
	// when it advances actions, so this is consistent with how the in-memory
	// store behaves.
	actions := t.inst.Actions
	for len(actions) <= rec.Index {
		actions = append(actions, ActionRecord{Index: len(actions)})
	}
	actions[rec.Index] = rec
	t.inst.Actions = actions
	return nil
}

// AppendOutbox enqueues an envelope for at-least-once publishing once the
// surrounding transaction commits. routingKey is the broker routing key.
// Stores the full serialized envelope as jsonb in the SAME tx.
func (t *pgTx) AppendOutbox(env messaging.Envelope, routingKey string) error {
	payload, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal outbox envelope %s: %w", env.MessageID, err)
	}
	_, err = t.tx.ExecContext(context.Background(), `
		INSERT INTO outbox_message
		  (message_id, message_type, schema_version, routing_key, envelope,
		   attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, 0, CURRENT_TIMESTAMP)`,
		env.MessageID, env.MessageType, env.SchemaVersion, routingKey, []byte(payload),
	)
	if err != nil {
		return fmt.Errorf("insert outbox_message %s: %w", env.MessageID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recovery methods (Task 6 contracts).
// ---------------------------------------------------------------------------

// terminalStatusesSQL is the SQL literal listing every terminal status. Used
// by ClaimRunnable and NonTerminalDefinitions to filter instances the recovery
// loop cannot advance. Mirrors isTerminal() in recovery_test.go.
const terminalStatusesSQL = "('succeeded','rejected','compensated','compensation_failed')"

// ClaimRunnable atomically claims up to limit instances that (a) are runnable,
// (b) are non-terminal, and (c) have no active lease. On each claimed instance
// it sets LeaseOwner=owner, LeaseUntil=now+lease, bumps Revision, and returns
// the instance id. A lease that has not yet expired cannot be claimed — not
// even by the same owner (use RenewLease to extend).
//
// Runnable means: preparing (needs Prepare), running with a timed-out waiting
// action (needs Redispatch), or compensating with a timed-out compensating
// action. A transiently-failed forward action (running + ActionFailed) is
// deliberately NOT claimed: processInstance only handles Prepare and
// Redispatch, neither of which retries a failed action, so claiming such an
// instance would release it every poll tick forever (busyspin). Forward-retry
// of transiently-failed actions is a known limitation NOT implemented in this
// engine plan; such instances require operator intervention or a future retry
// path.
//
// Implementation: a single CTE-based UPDATE whose WITH clause SELECTs matching
// rows with FOR UPDATE OF wi SKIP LOCKED, then UPDATEs them. SKIP LOCKED
// ensures concurrent ClaimRunnable calls divide work rather than block each
// other. The whole statement runs in the implicit per-statement transaction
// the driver wraps multi-row updates in.
func (s *PostgresStore) ClaimRunnable(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	until := now.Add(lease)
	rows, err := s.db.QueryContext(ctx, `
		WITH locked AS (
		  SELECT wi.workflow_id
		  FROM workflow_instance wi
		  LEFT JOIN workflow_action wa
		    ON wa.workflow_id = wi.workflow_id
		   AND wa.action_index = wi.current_action
		  WHERE wi.status NOT IN `+terminalStatusesSQL+`
		    AND (wi.lease_owner IS NULL OR wi.lease_until <= $1)
		    AND (
		      wi.status = 'preparing'
		      OR (wi.status = 'running'    AND wa.status = 'waiting_result'
		                                   AND wa.deadline_at IS NOT NULL
		                                   AND wa.deadline_at < $1)
		      OR (wi.status = 'compensating' AND wa.status = 'compensating'
		                                     AND wa.deadline_at IS NOT NULL
		                                     AND wa.deadline_at < $1)
		    )
		  LIMIT $2
		  FOR UPDATE OF wi SKIP LOCKED
		)
		UPDATE workflow_instance
		SET lease_owner = $3,
		    lease_until = $4,
		    revision = revision + 1,
		    updated_at = CURRENT_TIMESTAMP
		FROM locked
		WHERE workflow_instance.workflow_id = locked.workflow_id
		RETURNING workflow_instance.workflow_id`,
		now, limit, owner, until,
	)
	if err != nil {
		return nil, fmt.Errorf("ClaimRunnable: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ClaimRunnable scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RenewLease extends the lease on instance id for owner until now+lease.
// Returns ErrLeaseNotHeld if owner does not currently hold the lease, or
// ErrInstanceNotFound if the instance does not exist.
func (s *PostgresStore) RenewLease(ctx context.Context, id, owner string, now time.Time, lease time.Duration) error {
	until := now.Add(lease)
	res, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instance
		SET lease_until = $1,
		    revision = revision + 1,
		    updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $2 AND lease_owner = $3`,
		until, id, owner,
	)
	if err != nil {
		return fmt.Errorf("RenewLease %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("RenewLease %q rows: %w", id, err)
	}
	if n == 1 {
		return nil
	}
	// Distinguish not-found from not-held.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM workflow_instance WHERE workflow_id = $1`, id,
	).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: id=%s", ErrInstanceNotFound, id)
		}
		return fmt.Errorf("RenewLease existence check %q: %w", id, err)
	}
	return fmt.Errorf("%w: id=%s owner=%s", ErrLeaseNotHeld, id, owner)
}

// ReleaseLease clears the lease on instance id if it is currently held by
// owner. It is a no-op if the instance is missing or leased by a different
// owner.
func (s *PostgresStore) ReleaseLease(ctx context.Context, id, owner string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instance
		SET lease_owner = NULL,
		    lease_until = NULL,
		    revision = revision + 1,
		    updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = $1 AND lease_owner = $2`,
		id, owner,
	)
	if err != nil {
		return fmt.Errorf("ReleaseLease %q: %w", id, err)
	}
	// Per the Store interface, a no-op (missing or different owner) is NOT
	// an error.
	return nil
}

// TimedOut returns up to limit ids of instances whose current action is
// waiting for a result (forward ActionWaitingResult or compensation
// ActionCompensating) and whose DeadlineAt has passed. Read-only.
func (s *PostgresStore) TimedOut(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT wi.workflow_id
		FROM workflow_instance wi
		JOIN workflow_action wa
		  ON wa.workflow_id = wi.workflow_id
		 AND wa.action_index = wi.current_action
		WHERE wi.status IN ('running', 'compensating')
		  AND wa.status IN ('waiting_result', 'compensating')
		  AND wa.deadline_at IS NOT NULL
		  AND wa.deadline_at < $1
		LIMIT $2`,
		now, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("TimedOut: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("TimedOut scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// NonTerminalDefinitions returns the distinct (Type, Version) pairs
// referencing definitions of all non-terminal instances.
func (s *PostgresStore) NonTerminalDefinitions(ctx context.Context) ([]DefinitionRef, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT type, definition_version
		FROM workflow_instance
		WHERE status NOT IN `+terminalStatusesSQL+
		` ORDER BY type, definition_version`)
	if err != nil {
		return nil, fmt.Errorf("NonTerminalDefinitions: %w", err)
	}
	defer rows.Close()
	var refs []DefinitionRef
	for rows.Next() {
		var ref DefinitionRef
		if err := rows.Scan(&ref.Type, &ref.Version); err != nil {
			return nil, fmt.Errorf("NonTerminalDefinitions scan: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
