CREATE TABLE IF NOT EXISTS location (
  location_id text PRIMARY KEY,
  name text NOT NULL,
  type text NOT NULL CHECK (type IN ('warehouse', 'store')),
  priority integer NOT NULL CHECK (priority > 0),
  UNIQUE (name, type)
);

CREATE TABLE IF NOT EXISTS location_profile (
  location_id text PRIMARY KEY REFERENCES location(location_id) ON DELETE CASCADE,
  region text NOT NULL,
  fulfills_orders boolean NOT NULL,
  time_zone text NOT NULL,
  CHECK (region <> ''),
  CHECK (time_zone <> '')
);

CREATE INDEX IF NOT EXISTS idx_location_fulfillment
  ON location_profile(region, location_id)
  WHERE fulfills_orders;

CREATE TABLE IF NOT EXISTS inventory_level (
  sku text NOT NULL,
  location_id text NOT NULL REFERENCES location(location_id),
  on_hand integer NOT NULL CHECK (on_hand >= 0),
  reserved integer NOT NULL CHECK (reserved >= 0 AND reserved <= on_hand),
  updated_at timestamptz NOT NULL,
  available integer GENERATED ALWAYS AS (on_hand - reserved) STORED,
  PRIMARY KEY (sku, location_id)
);

CREATE INDEX IF NOT EXISTS idx_inventory_level_available
  ON inventory_level(location_id, sku)
  WHERE on_hand > reserved;

CREATE TABLE IF NOT EXISTS reservation (
  reservation_id text PRIMARY KEY,
  order_id text NOT NULL,
  sku text NOT NULL,
  location_id text NOT NULL REFERENCES location(location_id),
  quantity integer NOT NULL CHECK (quantity > 0),
  status text NOT NULL CHECK (status IN ('active', 'committed', 'released', 'expired')),
  expires_at timestamptz NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  FOREIGN KEY (sku, location_id) REFERENCES inventory_level(sku, location_id)
);

CREATE INDEX IF NOT EXISTS idx_reservation_order
  ON reservation(order_id, reservation_id);
CREATE INDEX IF NOT EXISTS idx_reservation_active
  ON reservation(expires_at, reservation_id)
  WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_reservation_level_active
  ON reservation(sku, location_id, expires_at)
  WHERE status = 'active';

CREATE TABLE IF NOT EXISTS reservation_order_state (
  order_id text PRIMARY KEY,
  terminal_state text NOT NULL
    CHECK (terminal_state IN ('release', 'commit', 'expire')),
  updated_at timestamptz NOT NULL
);

CREATE TABLE IF NOT EXISTS stock_movement (
  movement_id text PRIMARY KEY,
  sku text NOT NULL,
  location_id text NOT NULL,
  delta integer NOT NULL CHECK (delta <> 0),
  reason text NOT NULL CHECK (reason IN ('replenishment', 'sale', 'return', 'adjustment', 'transfer')),
  reference_id text,
  created_at timestamptz NOT NULL,
  FOREIGN KEY (sku, location_id) REFERENCES inventory_level(sku, location_id)
);

CREATE INDEX IF NOT EXISTS idx_stock_movement_level
  ON stock_movement(sku, location_id, created_at, movement_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_stock_movement_reference
  ON stock_movement(reason, reference_id, sku, location_id)
  WHERE reference_id IS NOT NULL;

-- Movement/level reconciliation is enforced transactionally by the Task 5
-- inventory service and rechecked by the Task 8 verifier. A per-row CHECK
-- cannot aggregate the movement ledger while staged writes are in progress.
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
