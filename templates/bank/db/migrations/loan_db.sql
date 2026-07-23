CREATE TABLE IF NOT EXISTS loan_product (
    product_code TEXT PRIMARY KEY,
    product_name TEXT NOT NULL,
    loan_type    TEXT NOT NULL,              -- Personal/Corporate/Mortgage/Consumption/Business
    rate_type    TEXT,
    min_rate     NUMERIC(10,6),
    max_rate     NUMERIC(10,6),
    max_term     INTEGER,
    max_amount   NUMERIC(18,2),
    status       TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS loan_account (
    loan_no         TEXT PRIMARY KEY,
    cust_id         TEXT NOT NULL,
    product_code    TEXT NOT NULL,
    ccy             TEXT NOT NULL,
    principal       NUMERIC(18,2) NOT NULL,
    balance         NUMERIC(18,2) NOT NULL,
    rate            NUMERIC(10,6) NOT NULL,
    start_biz_date  DATE NOT NULL,
    mature_date     DATE NOT NULL,
    term_months     INTEGER NOT NULL,
    status          TEXT DEFAULT 'disbursed',-- 放款/还款中/结清/逾期
    guarantee_type  TEXT,                    -- Credit/Mortgage/Guarantee
    branch_code     TEXT
);
CREATE INDEX IF NOT EXISTS idx_loan_account_cust ON loan_account(cust_id);
CREATE INDEX IF NOT EXISTS idx_loan_account_product ON loan_account(product_code);

CREATE TABLE IF NOT EXISTS loan_disbursement (
    disb_id    TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    loan_no    TEXT NOT NULL,
    amount     NUMERIC(18,2) NOT NULL,
    to_account TEXT,
    disb_ts    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_loan_disb_bizdate ON loan_disbursement(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_disb_loan ON loan_disbursement(loan_no);

CREATE TABLE IF NOT EXISTS loan_repay (
    repay_id       TEXT PRIMARY KEY,
    biz_date       DATE NOT NULL,
    loan_no        TEXT NOT NULL,
    due_date       DATE NOT NULL,
    principal_amt  NUMERIC(18,2) NOT NULL,
    interest_amt   NUMERIC(18,2) NOT NULL,
    paid_principal NUMERIC(18,2) DEFAULT 0,
    paid_interest  NUMERIC(18,2) DEFAULT 0,
    status         TEXT DEFAULT 'open'       -- Not due/repaid/overdue
);
CREATE INDEX IF NOT EXISTS idx_loan_repay_bizdate ON loan_repay(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_repay_loan ON loan_repay(loan_no);

CREATE TABLE IF NOT EXISTS loan_overdue (
    overdue_id     TEXT PRIMARY KEY,
    biz_date       DATE NOT NULL,
    loan_no        TEXT NOT NULL,
    overdue_days   INTEGER NOT NULL,
    overdue_class  TEXT NOT NULL,            -- Normal/Concern/Secondary/Suspicious/Loss
    overdue_amount NUMERIC(18,2) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_bizdate ON loan_overdue(biz_date);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_loan ON loan_overdue(loan_no);
CREATE INDEX IF NOT EXISTS idx_loan_overdue_class ON loan_overdue(overdue_class);

CREATE TABLE IF NOT EXISTS loan_balance (
    loan_no              TEXT NOT NULL,
    biz_date             DATE NOT NULL,
    principal_balance    NUMERIC(18,2) NOT NULL,
    interest_receivable  NUMERIC(18,2) DEFAULT 0,
    PRIMARY KEY (loan_no, biz_date)
);
CREATE INDEX IF NOT EXISTS idx_loan_balance_bizdate ON loan_balance(biz_date);
