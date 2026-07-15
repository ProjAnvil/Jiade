CREATE TABLE IF NOT EXISTS transfer_txn (
    txn_id       TEXT PRIMARY KEY,
    biz_date     DATE NOT NULL,
    txn_ts       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    out_account  TEXT NOT NULL,
    in_account   TEXT NOT NULL,
    amount       NUMERIC(18,2) NOT NULL,
    ccy          TEXT NOT NULL,
    fee          NUMERIC(18,2) DEFAULT 0,
    channel      TEXT,
    counter_bank TEXT,
    status       TEXT DEFAULT 'success',
    summary      TEXT
);
CREATE INDEX IF NOT EXISTS idx_transfer_txn_bizdate ON transfer_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_transfer_txn_acct ON transfer_txn(out_account, biz_date);

CREATE TABLE IF NOT EXISTS consumption_txn (
    txn_id      TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    txn_ts      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    account_no  TEXT NOT NULL,
    merchant_id TEXT,
    mcc         TEXT,
    amount      NUMERIC(18,2) NOT NULL,
    ccy         TEXT NOT NULL,
    status      TEXT DEFAULT 'success',
    summary     TEXT
);
CREATE INDEX IF NOT EXISTS idx_consumption_txn_bizdate ON consumption_txn(biz_date);

CREATE TABLE IF NOT EXISTS channel_txn (
    txn_id     TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    txn_ts     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    channel    TEXT NOT NULL,
    device     TEXT,
    cust_id    TEXT,
    status     TEXT DEFAULT 'success',
    latency_ms INTEGER
);

CREATE TABLE IF NOT EXISTS merchant (
    merchant_id     TEXT PRIMARY KEY,
    merchant_name   TEXT NOT NULL,
    mcc             TEXT,
    region          TEXT,
    status          TEXT DEFAULT 'active',
    create_biz_date DATE
);

CREATE TABLE IF NOT EXISTS fee_record (
    fee_id        TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    txn_id        TEXT,
    fee_type      TEXT,
    amount        NUMERIC(18,2) NOT NULL,
    ccy           TEXT NOT NULL,
    pay_or_receive TEXT DEFAULT 'receive'
);

CREATE TABLE IF NOT EXISTS settlement_record (
    settle_id  TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    channel    TEXT,
    net_amount NUMERIC(18,2) NOT NULL,
    txn_count  INTEGER,
    status     TEXT DEFAULT 'settled',
    settle_ts  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
