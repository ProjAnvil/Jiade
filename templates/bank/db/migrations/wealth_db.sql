CREATE TABLE IF NOT EXISTS wealth_product (
    product_code     TEXT PRIMARY KEY,
    product_name     TEXT NOT NULL,
    product_type     TEXT NOT NULL,          -- Fixed income/Equity/Hybrid/Currency/Fund
    risk_level       TEXT,
    expected_return  NUMERIC(10,6),
    min_amount       NUMERIC(18,2),
    term_days        INTEGER,
    start_biz_date   DATE,
    end_biz_date     DATE,
    status           TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS wealth_nav (
    product_code TEXT NOT NULL,
    biz_date     DATE NOT NULL,
    nav          NUMERIC(12,6) NOT NULL,
    accum_nav    NUMERIC(12,6) NOT NULL,
    PRIMARY KEY (product_code, biz_date)
);
CREATE INDEX IF NOT EXISTS idx_wealth_nav_bizdate ON wealth_nav(biz_date);

CREATE TABLE IF NOT EXISTS wealth_holding (
    holding_id    TEXT PRIMARY KEY,
    cust_id       TEXT NOT NULL,
    account_no    TEXT NOT NULL,
    product_code  TEXT NOT NULL,
    ccy           TEXT NOT NULL,
    share         NUMERIC(18,4) NOT NULL,
    cost          NUMERIC(18,2) NOT NULL,
    current_value NUMERIC(18,2) NOT NULL,
    biz_date      DATE NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wealth_holding_cust ON wealth_holding(cust_id);
CREATE INDEX IF NOT EXISTS idx_wealth_holding_product ON wealth_holding(product_code);

CREATE TABLE IF NOT EXISTS wealth_order (
    order_id    TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    cust_id     TEXT NOT NULL,
    product_code TEXT NOT NULL,
    account_no  TEXT NOT NULL,
    order_type  TEXT NOT NULL,               -- Subscription/Redemption
    amount      NUMERIC(18,2),
    share       NUMERIC(18,4),
    nav         NUMERIC(12,6),
    status      TEXT DEFAULT 'done',
    order_ts    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_wealth_order_bizdate ON wealth_order(biz_date);
CREATE INDEX IF NOT EXISTS idx_wealth_order_cust ON wealth_order(cust_id);
CREATE INDEX IF NOT EXISTS idx_wealth_order_product ON wealth_order(product_code);

CREATE TABLE IF NOT EXISTS wealth_income (
    income_id   TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    holding_id  TEXT NOT NULL,
    income_type TEXT,                        -- Interest/dividends
    amount      NUMERIC(18,2) NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_wealth_income_bizdate ON wealth_income(biz_date);
CREATE INDEX IF NOT EXISTS idx_wealth_income_holding ON wealth_income(holding_id);
