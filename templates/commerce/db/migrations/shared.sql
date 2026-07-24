-- Include this idempotent shared schema in every service-owned migration.
-- Domain writes insert outbox_event in the same transaction as their state;
-- each consumer records its own inbox_event key before applying its mutation.

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
  ON outbox_event (created_at, event_id)
  WHERE published_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_outbox_event_claim_expiry
  ON outbox_event (claimed_at)
  WHERE published_at IS NULL;

CREATE TABLE IF NOT EXISTS inbox_event (
  consumer text NOT NULL,
  event_id uuid NOT NULL,
  event_type text NOT NULL,
  received_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
