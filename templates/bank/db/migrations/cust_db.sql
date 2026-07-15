CREATE TABLE IF NOT EXISTS cust_info (
    cust_id        TEXT PRIMARY KEY,
    cust_type      TEXT NOT NULL,
    name           TEXT NOT NULL,
    cert_type      TEXT,
    cert_no        TEXT,
    gender         TEXT,
    birthday       DATE,
    nationality    TEXT,
    risk_level     TEXT,
    kyc_status     TEXT,
    create_biz_date DATE NOT NULL,
    create_ts      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cust_id_doc (
    doc_id      TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    cert_type   TEXT NOT NULL,
    cert_no     TEXT NOT NULL,
    issue_date  DATE,
    expire_date DATE,
    issue_org   TEXT
);

CREATE TABLE IF NOT EXISTS cust_contact (
    contact_id  TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    phone       TEXT,
    email       TEXT,
    address     TEXT,
    region_code TEXT,
    is_primary  INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS cust_org (
    cust_id         TEXT PRIMARY KEY,
    org_name        TEXT,
    industry_code   TEXT,
    regist_capital  NUMERIC(18,2),
    legal_rep       TEXT,
    establish_date  DATE
);

CREATE TABLE IF NOT EXISTS cust_account_rel (
    rel_id     TEXT PRIMARY KEY,
    cust_id    TEXT NOT NULL,
    account_no TEXT NOT NULL,
    role       TEXT,
    rel_type   TEXT
);
CREATE INDEX IF NOT EXISTS idx_cust_account_rel_cust ON cust_account_rel(cust_id);
