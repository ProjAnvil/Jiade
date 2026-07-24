CREATE TABLE IF NOT EXISTS membership_tier (
  tier_id text PRIMARY KEY,
  name text NOT NULL UNIQUE,
  rank integer NOT NULL UNIQUE CHECK (rank >= 0),
  minimum_spend_minor bigint NOT NULL CHECK (minimum_spend_minor >= 0),
  benefits jsonb NOT NULL DEFAULT '{}',
  CHECK (jsonb_typeof(benefits) = 'object')
);

-- The column order remains compatible with the current seed.
CREATE TABLE IF NOT EXISTS customer (
  customer_id text PRIMARY KEY,
  email text NOT NULL UNIQUE,
  name text NOT NULL,
  phone text,
  status text NOT NULL CHECK (status IN ('active', 'disabled', 'guest')),
  created_at timestamptz NOT NULL,
  CHECK (email <> ''),
  CHECK (name <> '')
);

CREATE INDEX IF NOT EXISTS idx_customer_status
  ON customer(status, created_at DESC, customer_id);
CREATE INDEX IF NOT EXISTS idx_customer_phone
  ON customer(phone)
  WHERE phone IS NOT NULL;

CREATE TABLE IF NOT EXISTS customer_membership (
  customer_id text PRIMARY KEY REFERENCES customer(customer_id) ON DELETE CASCADE,
  tier_id text NOT NULL REFERENCES membership_tier(tier_id),
  enrolled_at timestamptz NOT NULL,
  expires_at timestamptz,
  CHECK (expires_at IS NULL OR expires_at > enrolled_at)
);

CREATE INDEX IF NOT EXISTS idx_customer_membership_tier
  ON customer_membership(tier_id, customer_id);

CREATE TABLE IF NOT EXISTS address (
  address_id text PRIMARY KEY,
  customer_id text NOT NULL REFERENCES customer(customer_id) ON DELETE CASCADE,
  label text NOT NULL,
  recipient text NOT NULL,
  phone text NOT NULL,
  country_code char(2) NOT NULL,
  province text NOT NULL,
  city text NOT NULL,
  district text NOT NULL,
  line1 text NOT NULL,
  postal_code text NOT NULL,
  is_default boolean NOT NULL,
  CHECK (recipient <> ''),
  CHECK (line1 <> '')
);

CREATE INDEX IF NOT EXISTS idx_address_customer
  ON address(customer_id, address_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_address_one_default
  ON address(customer_id)
  WHERE is_default;

CREATE TABLE IF NOT EXISTS customer_consent (
  consent_id text PRIMARY KEY,
  customer_id text NOT NULL REFERENCES customer(customer_id) ON DELETE CASCADE,
  channel text NOT NULL CHECK (channel IN ('email', 'sms', 'phone', 'push')),
  granted boolean NOT NULL,
  source text NOT NULL,
  occurred_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_customer_consent_history
  ON customer_consent(customer_id, channel, occurred_at DESC);

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
