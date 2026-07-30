CREATE TABLE IF NOT EXISTS sys_param (
    param_key   TEXT PRIMARY KEY,
    param_value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ccy (
    ccy_code       TEXT PRIMARY KEY,
    ccy_name       TEXT NOT NULL,
    decimal_digits INTEGER DEFAULT 2,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS chart_of_acct (
    subject_code   TEXT PRIMARY KEY,
    subject_name   TEXT NOT NULL,
    dc_attr        TEXT NOT NULL,            -- borrow/loan
    level          INTEGER,
    parent_subject TEXT,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS interest_rate (
    rate_id            TEXT PRIMARY KEY,
    acct_type          TEXT NOT NULL,
    ccy                TEXT NOT NULL,
    rate_value         NUMERIC(10,6) NOT NULL,
    effective_biz_date DATE NOT NULL,
    status             TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS branch (
    branch_code   TEXT PRIMARY KEY,
    branch_name   TEXT NOT NULL,
    parent_branch TEXT,
    region        TEXT,
    level         INTEGER,
    status        TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS demand_account (
    account_no     TEXT PRIMARY KEY,
    cust_id        TEXT NOT NULL,
    ccy            TEXT NOT NULL,
    acct_status    TEXT DEFAULT 'active',
    open_biz_date  DATE NOT NULL,
    branch_code    TEXT,
    product_code   TEXT,
    subject_code   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS fixed_account (
    account_no     TEXT PRIMARY KEY,
    cust_id        TEXT NOT NULL,
    ccy            TEXT NOT NULL,
    principal      NUMERIC(18,2) NOT NULL,
    rate           NUMERIC(10,6) NOT NULL,
    term_months    INTEGER NOT NULL,
    start_biz_date DATE NOT NULL,
    mature_date    DATE NOT NULL,
    acct_status    TEXT DEFAULT 'active',
    subject_code   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_balance (
    account_no        TEXT NOT NULL,
    biz_date          DATE NOT NULL,
    balance           NUMERIC(18,2) NOT NULL,
    available_balance NUMERIC(18,2) NOT NULL,
    frozen_amount     NUMERIC(18,2) DEFAULT 0,
    subject_code      TEXT,
    PRIMARY KEY (account_no, biz_date)
);

CREATE TABLE IF NOT EXISTS acct_txn (
    txn_id        TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    txn_ts        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    account_no    TEXT NOT NULL,
    dc_flag       TEXT NOT NULL,             -- borrow/loan
    amount        NUMERIC(18,2) NOT NULL,
    ccy           TEXT NOT NULL,
    subject_code  TEXT NOT NULL,
    opp_account   TEXT,
    ref_txn_id    TEXT,
    channel       TEXT,
    summary       TEXT
);
CREATE INDEX IF NOT EXISTS idx_acct_txn_bizdate ON acct_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_acct_txn_acct ON acct_txn(account_no, biz_date);
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS voucher_no  TEXT NOT NULL DEFAULT '';
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS txn_status  TEXT NOT NULL DEFAULT 'normal';
CREATE INDEX IF NOT EXISTS idx_acct_txn_voucher ON acct_txn(voucher_no);

CREATE TABLE IF NOT EXISTS gl_balance (
    subject_code TEXT NOT NULL,
    biz_date     DATE NOT NULL,
    dc_balance   NUMERIC(18,2) DEFAULT 0,
    cc_balance   NUMERIC(18,2) DEFAULT 0,
    ccy          TEXT NOT NULL,
    PRIMARY KEY (subject_code, biz_date, ccy)
);

-- Funds holds: reservations of available balance on a demand account.
-- A hold reduces available = ledger balance - active holds; the ledger
-- balance itself is only moved when the hold is captured (held transfer,
-- Task 3) or released back to available.
CREATE TABLE IF NOT EXISTS funds_hold (
    hold_id         TEXT PRIMARY KEY,
    idempotency_key TEXT NOT NULL UNIQUE,
    account_no      TEXT NOT NULL,
    amount          NUMERIC(18,2) NOT NULL,
    ccy             TEXT NOT NULL,
    workflow_id     TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active',   -- active / released / captured
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Partial index: active holds by account (the hot-path query for available-balance computation).
CREATE INDEX IF NOT EXISTS idx_funds_hold_account_active
    ON funds_hold(account_no) WHERE status = 'active';
