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
