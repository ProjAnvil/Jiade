CREATE TABLE IF NOT EXISTS points_acct (
    cust_id         TEXT PRIMARY KEY,
    points_balance  INTEGER DEFAULT 0,
    frozen_points   INTEGER DEFAULT 0,
    member_level    TEXT,
    update_biz_date DATE
);

CREATE TABLE IF NOT EXISTS points_txn (
    txn_id      TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    biz_date    DATE NOT NULL,
    points      INTEGER NOT NULL,
    direction   TEXT NOT NULL,
    source_type TEXT,
    ref_txn_id  TEXT,
    summary     TEXT
);
CREATE INDEX IF NOT EXISTS idx_points_txn_bizdate ON points_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_points_txn_cust ON points_txn(cust_id, biz_date);

CREATE TABLE IF NOT EXISTS coupon (
    coupon_id       TEXT PRIMARY KEY,
    cust_id         TEXT NOT NULL,
    campaign_id     TEXT,
    face_value      NUMERIC(18,2) NOT NULL,
    min_spend       NUMERIC(18,2) DEFAULT 0,
    status          TEXT DEFAULT 'issued',
    issue_biz_date  DATE,
    expire_date     DATE
);
CREATE INDEX IF NOT EXISTS idx_coupon_cust ON coupon(cust_id);

CREATE TABLE IF NOT EXISTS coupon_usage (
    usage_id       TEXT PRIMARY KEY,
    coupon_id      TEXT NOT NULL,
    biz_date       DATE NOT NULL,
    txn_id         TEXT,
    deduct_amount  NUMERIC(18,2),
    merchant_id    TEXT
);

CREATE TABLE IF NOT EXISTS campaign (
    campaign_id     TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    type            TEXT,
    start_biz_date  DATE,
    end_biz_date    DATE,
    budget          NUMERIC(18,2),
    used_budget     NUMERIC(18,2) DEFAULT 0,
    status          TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS member_level (
    level_code       TEXT PRIMARY KEY,
    level_name       TEXT NOT NULL,
    points_threshold INTEGER NOT NULL,
    benefits_json    TEXT
);
