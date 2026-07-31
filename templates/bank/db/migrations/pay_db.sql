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

CREATE TABLE IF NOT EXISTS workflow_instance (
    workflow_id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    definition_version INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'preparing',
    input_json JSONB NOT NULL,
    prepared_context_json JSONB,
    current_action INTEGER NOT NULL DEFAULT 0,
    revision BIGINT NOT NULL DEFAULT 0,
    lease_owner TEXT,
    lease_until TIMESTAMPTZ,
    next_wakeup_at TIMESTAMPTZ,
    operational_deadline TIMESTAMPTZ,
    last_error_class TEXT,
    last_error TEXT,
    correlation_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT ck_workflow_instance_status CHECK (status IN (
        'preparing',
        'ready',
        'running',
        'succeeded',
        'rejected',
        'compensating',
        'compensated',
        'compensation_failed'
    )),
    CONSTRAINT ck_workflow_instance_input_json CHECK (jsonb_typeof(input_json) = 'object'),
    CONSTRAINT ck_workflow_instance_prepared_context CHECK (
        prepared_context_json IS NULL
        OR jsonb_typeof(prepared_context_json) = 'object'
    )
);
ALTER TABLE workflow_instance ADD COLUMN IF NOT EXISTS correlation_id TEXT;

CREATE INDEX IF NOT EXISTS idx_workflow_instance_status_wakeup
    ON workflow_instance(status, next_wakeup_at);
CREATE INDEX IF NOT EXISTS idx_workflow_instance_lease_until
    ON workflow_instance(lease_until);

CREATE TABLE IF NOT EXISTS workflow_action (
    action_id BIGSERIAL PRIMARY KEY,
    workflow_id TEXT NOT NULL,
    action_index INTEGER NOT NULL,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    direction TEXT NOT NULL DEFAULT 'forward',
    attempt INTEGER NOT NULL DEFAULT 0,
    idempotency_key TEXT NOT NULL,
    command_id TEXT,
    result_event_id TEXT,
    deadline_at TIMESTAMPTZ,
    output JSONB,
    last_error_class TEXT,
    last_error TEXT,
    accepted_result_types JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (workflow_id, action_index),
    CONSTRAINT fk_workflow_action_instance
        FOREIGN KEY (workflow_id) REFERENCES workflow_instance(workflow_id)
        ON DELETE RESTRICT,
    CONSTRAINT ck_workflow_action_status CHECK (status IN (
        'pending',
        'waiting_result',
        'succeeded',
        'failed',
        'compensating',
        'compensated',
        'compensation_failed'
    )),
    CONSTRAINT ck_workflow_action_direction CHECK (direction IN ('forward', 'compensation')),
    CONSTRAINT ck_workflow_action_output CHECK (
        output IS NULL
        OR jsonb_typeof(output) = 'object'
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_action_command_id
    ON workflow_action(command_id) WHERE command_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS payment_intent (
    idempotency_key    TEXT PRIMARY KEY,
    request_hash       TEXT NOT NULL,
    workflow_id        TEXT NOT NULL UNIQUE,
    payer_customer_id  TEXT NOT NULL,
    payer_account_no   TEXT NOT NULL,
    payee_account_no   TEXT NOT NULL,
    currency           TEXT NOT NULL,
    amount_minor       BIGINT NOT NULL,
    status             TEXT NOT NULL DEFAULT 'pending',
    reversed           BOOLEAN NOT NULL DEFAULT FALSE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT ck_payment_intent_amount CHECK (amount_minor > 0),
    CONSTRAINT ck_payment_intent_status CHECK (status IN (
        'pending',
        'running',
        'succeeded',
        'compensated',
        'compensation_failed',
        'rejected',
        'reversal_pending',
        'reversed'
    ))
);

CREATE INDEX IF NOT EXISTS idx_payment_intent_workflow ON payment_intent(workflow_id);
CREATE INDEX IF NOT EXISTS idx_payment_intent_status ON payment_intent(status);

CREATE TABLE IF NOT EXISTS workflow_operator_audit (
    audit_id          BIGSERIAL PRIMARY KEY,
    workflow_id       TEXT NOT NULL,
    operator          TEXT NOT NULL,
    action            TEXT NOT NULL,
    external_reference TEXT NOT NULL,
    reason            TEXT,
    previous_state    TEXT NOT NULL,
    new_state         TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_workflow_operator_audit_workflow
    ON workflow_operator_audit(workflow_id, created_at);

CREATE OR REPLACE FUNCTION workflow_operator_audit_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'workflow_operator_audit is immutable: % operation not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS workflow_operator_audit_no_update ON workflow_operator_audit;
CREATE TRIGGER workflow_operator_audit_no_update
    BEFORE UPDATE OR DELETE ON workflow_operator_audit
    FOR EACH ROW EXECUTE FUNCTION workflow_operator_audit_reject_mutation();
