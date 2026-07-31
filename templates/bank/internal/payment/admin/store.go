package admin

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"bank/internal/platform/pg"
	"bank/internal/platform/workflow"
)

// PgGateway is the production CompensationGateway. It performs each operator
// operation inside a single database transaction that holds the instance row
// lock (SELECT ... FOR UPDATE): the engine state transition, its outbox
// writes, and the immutable workflow_operator_audit row all commit together or
// roll back together. A crash can therefore never leave a state transition
// without its audit record, nor an audit record without its transition.
type PgGateway struct {
	db     *sql.DB
	store  *workflow.PostgresStore
	engine *workflow.Engine
}

// NewPgGateway wires the gateway to the payment service's DB, workflow store,
// and engine. The DB, store, and engine are the SAME instances the payment
// service uses, so operator operations share the workflow schema and outbox.
func NewPgGateway(db *sql.DB, store *workflow.PostgresStore, engine *workflow.Engine) *PgGateway {
	return &PgGateway{db: db, store: store, engine: engine}
}

// RetryCompensation re-dispatches the failed compensation and records the audit
// row in one transaction. The audit's PrevState/NewState are captured from the
// engine's transition return inside the transaction.
func (g *PgGateway) RetryCompensation(ctx context.Context, workflowID string, audit OperatorAudit) (Transition, error) {
	var transition Transition
	err := pg.RunInTx(ctx, g.db, func(q pg.DBTX) error {
		tx, ok := q.(*sql.Tx)
		if !ok {
			return fmt.Errorf("PgGateway.RetryCompensation: pg.DBTX is %T, want *sql.Tx", q)
		}
		return g.store.WithInstanceTx(ctx, tx, workflowID, func(wtx workflow.Tx) error {
			prev, curr, err := g.engine.RetryCompensationTx(wtx, ctx)
			if err != nil {
				return err
			}
			transition = Transition{WorkflowID: workflowID, Prev: string(prev), New: string(curr)}
			return insertOperatorAudit(ctx, tx, workflowID, audit.withTransition(prev, curr))
		})
	})
	return transition, err
}

// ResolveCompensation marks the named action's compensation resolved and
// records the audit row in one transaction.
func (g *PgGateway) ResolveCompensation(ctx context.Context, workflowID, actionName string, audit OperatorAudit) (Transition, error) {
	var transition Transition
	err := pg.RunInTx(ctx, g.db, func(q pg.DBTX) error {
		tx, ok := q.(*sql.Tx)
		if !ok {
			return fmt.Errorf("PgGateway.ResolveCompensation: pg.DBTX is %T, want *sql.Tx", q)
		}
		return g.store.WithInstanceTx(ctx, tx, workflowID, func(wtx workflow.Tx) error {
			prev, curr, err := g.engine.ResolveCompensationTx(wtx, ctx, actionName)
			if err != nil {
				return err
			}
			transition = Transition{WorkflowID: workflowID, Prev: string(prev), New: string(curr)}
			return insertOperatorAudit(ctx, tx, workflowID, audit.withTransition(prev, curr))
		})
	})
	return transition, err
}

// withTransition returns a copy of the audit stamped with the transition the
// engine reported. Called inside the in-flight transaction so the audit row
// records exactly what changed.
func (a OperatorAudit) withTransition(prev, curr workflow.InstanceStatus) OperatorAudit {
	out := a
	out.PrevState = string(prev)
	out.NewState = string(curr)
	if out.CreatedAt.IsZero() {
		out.CreatedAt = time.Now()
	}
	return out
}

// insertOperatorAudit persists an immutable audit row on the caller's
// transaction. The workflow_operator_audit table has a BEFORE UPDATE OR DELETE
// trigger that rejects mutation, so a row, once written, can only ever be
// appended — never altered or erased.
func insertOperatorAudit(ctx context.Context, tx *sql.Tx, workflowID string, audit OperatorAudit) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_operator_audit
		  (workflow_id, operator, action, external_reference, reason,
		   previous_state, new_state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		workflowID, audit.Operator, audit.Action, audit.Reference, audit.Reason,
		audit.PrevState, audit.NewState, audit.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert workflow_operator_audit: %w", err)
	}
	return nil
}

// PgInstanceReader is the production InstanceReader. It loads a read-only
// instance snapshot via the PostgresStore's per-instance transaction. The
// transaction commits immediately after the read, so the row lock is held only
// for the snapshot duration.
type PgInstanceReader struct {
	store *workflow.PostgresStore
}

// NewPgInstanceReader wires the reader to a PostgresStore.
func NewPgInstanceReader(store *workflow.PostgresStore) *PgInstanceReader {
	return &PgInstanceReader{store: store}
}

func (r *PgInstanceReader) Instance(ctx context.Context, workflowID string) (workflow.Instance, error) {
	var inst workflow.Instance
	err := r.store.WithInstance(ctx, workflowID, func(tx workflow.Tx) error {
		inst = *tx.Instance()
		return nil
	})
	if err != nil {
		return workflow.Instance{}, err
	}
	return inst, nil
}
