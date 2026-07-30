CREATE TABLE IF NOT EXISTS risk_rule (
    rule_id        TEXT PRIMARY KEY,
    rule_name      TEXT NOT NULL,
    rule_type      TEXT,
    condition_json TEXT,
    threshold      NUMERIC(18,2),
    action         TEXT,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS risk_event (
    event_id      TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    cust_id       TEXT,
    account_no    TEXT,
    rule_id       TEXT,
    risk_score    NUMERIC(6,2),
    action_taken  TEXT,
    txn_ref       TEXT,
    summary       TEXT
);
CREATE INDEX IF NOT EXISTS idx_risk_event_bizdate ON risk_event(biz_date);
CREATE INDEX IF NOT EXISTS idx_risk_event_rule ON risk_event(rule_id, biz_date);

CREATE TABLE IF NOT EXISTS blacklist (
    list_id            TEXT PRIMARY KEY,
    cust_id            TEXT,
    entity_type        TEXT,
    reason             TEXT,
    effective_biz_date DATE,
    expire_date        DATE,
    status             TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS payment_authorization (
    authorization_id TEXT PRIMARY KEY,
    workflow_id      TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL UNIQUE,
    customer_id      TEXT NOT NULL,
    amount_cents     BIGINT NOT NULL,
    currency         TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'pending',
    matched_rules    JSONB NOT NULL DEFAULT '[]'::jsonb,
    context_digest   TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payment_authorization_workflow
    ON payment_authorization(workflow_id);
