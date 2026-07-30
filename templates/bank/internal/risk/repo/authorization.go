// Package repo is the data access layer of the risk service.
//
// This file adds AuthorizationRepo, the persistence implementation for the
// payment_authorization table. Every method accepts a pg.DBTX so the consumer
// can run Inbox insert, domain mutation, and Outbox write in one transaction.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"bank/internal/platform/pg"
	"bank/internal/risk/domain"
	"bank/internal/risk/service"
)

// Compile-time assertion that AuthorizationRepo satisfies the service layer's
// AuthorizationStore interface.
var _ service.AuthorizationStore = (*AuthorizationRepo)(nil)

// AuthorizationRepo persists PaymentAuthorization rows in the risk database.
type AuthorizationRepo struct{}

// NewAuthorizationRepo constructs an AuthorizationRepo. The repo is stateless;
// all SQL runs against the caller-supplied pg.DBTX (which may be *sql.DB for
// standalone calls or *sql.Tx for transactional consumer flows).
func NewAuthorizationRepo() *AuthorizationRepo { return &AuthorizationRepo{} }

// Insert persists a new payment authorization row. The idempotency_key unique
// constraint guards against duplicate inserts within a transaction.
func (r *AuthorizationRepo) Insert(ctx context.Context, q pg.DBTX, a domain.PaymentAuthorization) error {
	rulesJSON, err := domain.EncodeMatchedRules(a.MatchedRuleIDs)
	if err != nil {
		return fmt.Errorf("encode matched rules: %w", err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO payment_authorization
		  (authorization_id, workflow_id, idempotency_key, customer_id,
		   amount_cents, currency, status, matched_rules, context_digest,
		   created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), $10, $11)`,
		a.AuthorizationID, a.WorkflowID, a.IdempotencyKey, a.CustomerID,
		a.AmountCents, a.Currency, string(a.Status), rulesJSON, a.ContextDigest,
		a.CreatedAt, a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert payment_authorization %s: %w", a.AuthorizationID, err)
	}
	return nil
}

// GetByID loads an authorization by its primary key. Returns
// service.ErrAuthorizationNotFound when no row exists.
func (r *AuthorizationRepo) GetByID(ctx context.Context, q pg.DBTX, id string) (domain.PaymentAuthorization, error) {
	row := q.QueryRowContext(ctx, `
		SELECT authorization_id, workflow_id, idempotency_key, customer_id,
		       amount_cents, currency, status, matched_rules, context_digest,
		       created_at, updated_at
		FROM payment_authorization
		WHERE authorization_id = $1`, id)
	return scanAuthorization(row.Scan)
}

// GetByIdempotencyKey loads an authorization by its unique idempotency key.
// Returns service.ErrAuthorizationNotFound when no row exists.
func (r *AuthorizationRepo) GetByIdempotencyKey(ctx context.Context, q pg.DBTX, key string) (domain.PaymentAuthorization, error) {
	row := q.QueryRowContext(ctx, `
		SELECT authorization_id, workflow_id, idempotency_key, customer_id,
		       amount_cents, currency, status, matched_rules, context_digest,
		       created_at, updated_at
		FROM payment_authorization
		WHERE idempotency_key = $1`, key)
	return scanAuthorization(row.Scan)
}

// UpdateStatus persists the status and updated_at of an authorization. Used by
// the void transition.
func (r *AuthorizationRepo) UpdateStatus(ctx context.Context, q pg.DBTX, a domain.PaymentAuthorization) error {
	_, err := q.ExecContext(ctx, `
		UPDATE payment_authorization SET
		  status = $2,
		  updated_at = $3
		WHERE authorization_id = $1`,
		a.AuthorizationID, string(a.Status), a.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("update payment_authorization %s: %w", a.AuthorizationID, err)
	}
	return nil
}

// IsBlacklisted reports whether the customer has an active blacklist entry in
// the risk database.
func (r *AuthorizationRepo) IsBlacklisted(ctx context.Context, q pg.DBTX, customerID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
		  SELECT 1 FROM blacklist
		  WHERE cust_id = $1 AND status = 'active'
		)`, customerID,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check blacklist for %s: %w", customerID, err)
	}
	return exists, nil
}

// scanAuthorization populates a PaymentAuthorization from a scan callback
// (shared by *sql.Row and *sql.Rows). sql.ErrNoRows is translated to
// service.ErrAuthorizationNotFound.
func scanAuthorization(scan func(dest ...any) error) (domain.PaymentAuthorization, error) {
	var (
		a             domain.PaymentAuthorization
		status        string
		matchedRules  []byte
		contextDigest sql.NullString
	)
	if err := scan(
		&a.AuthorizationID, &a.WorkflowID, &a.IdempotencyKey, &a.CustomerID,
		&a.AmountCents, &a.Currency, &status, &matchedRules, &contextDigest,
		&a.CreatedAt, &a.UpdatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.PaymentAuthorization{}, service.ErrAuthorizationNotFound
		}
		return domain.PaymentAuthorization{}, err
	}
	a.Status = domain.AuthorizationStatus(status)
	a.ContextDigest = contextDigest.String
	rules, err := domain.DecodeMatchedRules(matchedRules)
	if err != nil {
		return domain.PaymentAuthorization{}, fmt.Errorf("scan authorization: %w", err)
	}
	a.MatchedRuleIDs = rules
	return a, nil
}
