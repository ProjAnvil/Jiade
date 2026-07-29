-- Shared transactional messaging tables. Apply this migration to every service
-- database that produces Outbox messages or consumes Inbox messages.

CREATE TABLE IF NOT EXISTS outbox_message (
  message_id uuid PRIMARY KEY,
  message_type text NOT NULL,
  schema_version integer NOT NULL,
  routing_key text NOT NULL,
  envelope jsonb NOT NULL,
  attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  claim_token uuid,
  claimed_at timestamptz,
  dispatched_at timestamptz,
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((claim_token IS NULL) = (claimed_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_outbox_message_pending
  ON outbox_message (created_at, message_id)
  WHERE dispatched_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_outbox_message_claim
  ON outbox_message (claimed_at)
  WHERE dispatched_at IS NULL;

CREATE TABLE IF NOT EXISTS inbox_message (
  consumer text NOT NULL,
  message_id uuid NOT NULL,
  message_type text NOT NULL,
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, message_id)
);
