CREATE TABLE IF NOT EXISTS payment_intent (
  payment_intent_id text PRIMARY KEY,
  order_id text NOT NULL,
  amount_minor bigint NOT NULL CHECK (amount_minor > 0),
  currency char(3) NOT NULL,
  status text NOT NULL CHECK (status IN ('requires_method', 'processing', 'authorized', 'succeeded', 'failed', 'cancelled', 'partially_refunded', 'refunded')),
  provider text NOT NULL,
  provider_reference text UNIQUE,
  idempotency_key text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_payment_order
  ON payment_intent(order_id, created_at DESC, payment_intent_id);
CREATE INDEX IF NOT EXISTS idx_payment_created_at
  ON payment_intent(created_at DESC, payment_intent_id);
CREATE INDEX IF NOT EXISTS idx_payment_status
  ON payment_intent(status, created_at, payment_intent_id);

CREATE TABLE IF NOT EXISTS payment_method_snapshot (
  payment_method_id text PRIMARY KEY,
  payment_intent_id text NOT NULL REFERENCES payment_intent(payment_intent_id) ON DELETE CASCADE,
  method_type text NOT NULL CHECK (method_type IN ('card', 'wallet', 'bank_transfer')),
  network text,
  last_four char(4),
  expiry_month integer,
  expiry_year integer,
  billing_address jsonb,
  created_at timestamptz NOT NULL,
  CHECK (expiry_month IS NULL OR expiry_month BETWEEN 1 AND 12),
  CHECK (expiry_year IS NULL OR expiry_year >= 2000),
  CHECK (billing_address IS NULL OR jsonb_typeof(billing_address) = 'object')
);

CREATE INDEX IF NOT EXISTS idx_payment_method_intent
  ON payment_method_snapshot(payment_intent_id, payment_method_id);

CREATE TABLE IF NOT EXISTS payment_attempt (
  attempt_id text PRIMARY KEY,
  payment_intent_id text NOT NULL REFERENCES payment_intent(payment_intent_id) ON DELETE CASCADE,
  status text NOT NULL CHECK (status IN ('processing', 'authorized', 'succeeded', 'failed', 'cancelled')),
  failure_code text CHECK (failure_code IS NULL OR failure_code IN ('insufficient_funds', 'card_declined', 'provider_timeout', 'risk_rejection')),
  amount_minor bigint NOT NULL CHECK (amount_minor > 0),
  created_at timestamptz NOT NULL,
  CHECK (
    (status = 'failed' AND failure_code IS NOT NULL)
    OR (status <> 'failed' AND failure_code IS NULL)
  )
);

CREATE INDEX IF NOT EXISTS idx_payment_attempt_intent
  ON payment_attempt(payment_intent_id, created_at, attempt_id);

CREATE OR REPLACE FUNCTION validate_payment_attempt()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  intent_amount bigint;
BEGIN
  SELECT amount_minor
    INTO intent_amount
    FROM payment_intent
    WHERE payment_intent_id = NEW.payment_intent_id;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'payment intent % does not exist', NEW.payment_intent_id;
  END IF;
  IF NEW.amount_minor > intent_amount THEN
    RAISE EXCEPTION 'payment attempt amount exceeds intent amount';
  END IF;
  IF NEW.status = 'failed' AND NEW.failure_code IS NULL THEN
    RAISE EXCEPTION 'failed payment attempt requires failure_code';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_validate_payment_attempt ON payment_attempt;
CREATE TRIGGER trg_validate_payment_attempt
BEFORE INSERT OR UPDATE OF payment_intent_id, status, failure_code, amount_minor
ON payment_attempt
FOR EACH ROW
EXECUTE FUNCTION validate_payment_attempt();

CREATE TABLE IF NOT EXISTS refund (
  refund_id text PRIMARY KEY,
  payment_intent_id text NOT NULL REFERENCES payment_intent(payment_intent_id) ON DELETE CASCADE,
  amount_minor bigint NOT NULL CHECK (amount_minor > 0),
  status text NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed', 'cancelled')),
  reason text NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  created_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_refund_intent
  ON refund(payment_intent_id, created_at, refund_id);

CREATE OR REPLACE FUNCTION validate_refund()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  intent_amount bigint;
  intent_status text;
  refunded_amount bigint;
BEGIN
  SELECT amount_minor, status
    INTO intent_amount, intent_status
    FROM payment_intent
    WHERE payment_intent_id = NEW.payment_intent_id
    FOR UPDATE;

  IF NOT FOUND THEN
    RAISE EXCEPTION 'payment intent % does not exist', NEW.payment_intent_id;
  END IF;
  IF intent_status NOT IN ('succeeded', 'partially_refunded') THEN
    RAISE EXCEPTION 'refund requires a captured payment intent';
  END IF;

  IF NEW.status IN ('pending', 'succeeded') THEN
    SELECT COALESCE(SUM(amount_minor), 0)
      INTO refunded_amount
      FROM refund
      WHERE payment_intent_id = NEW.payment_intent_id
        AND status IN ('pending', 'succeeded')
        AND refund_id <> NEW.refund_id;

    IF refunded_amount > intent_amount
       OR NEW.amount_minor > intent_amount - refunded_amount THEN
      RAISE EXCEPTION 'cumulative refund amount exceeds intent amount';
    END IF;
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_validate_refund ON refund;
CREATE TRIGGER trg_validate_refund
BEFORE INSERT OR UPDATE OF payment_intent_id, amount_minor, status
ON refund
FOR EACH ROW
EXECUTE FUNCTION validate_refund();

CREATE TABLE IF NOT EXISTS webhook_inbox (
  provider_event_id text PRIMARY KEY,
  event_type text NOT NULL,
  payload jsonb NOT NULL,
  received_at timestamptz NOT NULL,
  processed_at timestamptz,
  CHECK (jsonb_typeof(payload) = 'object'),
  CHECK (processed_at IS NULL OR processed_at >= received_at)
);

CREATE INDEX IF NOT EXISTS idx_webhook_pending
  ON webhook_inbox(received_at, provider_event_id)
  WHERE processed_at IS NULL;

CREATE TABLE IF NOT EXISTS outbox_event (
  event_id uuid PRIMARY KEY,
  event_type text NOT NULL,
  schema_version integer NOT NULL CHECK (schema_version > 0),
  subject text NOT NULL,
  correlation_id text NOT NULL,
  causation_id text,
  occurred_at timestamptz NOT NULL,
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  claim_token uuid,
  claimed_at timestamptz,
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  last_error text,
  published_at timestamptz
);

CREATE INDEX IF NOT EXISTS idx_outbox_event_pending
  ON outbox_event(created_at, event_id)
  WHERE published_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_outbox_event_claim_expiry
  ON outbox_event(claimed_at)
  WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS inbox_event (
  consumer text NOT NULL,
  event_id uuid NOT NULL,
  event_type text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
