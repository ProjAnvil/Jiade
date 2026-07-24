CREATE TABLE IF NOT EXISTS fulfillment_order (
  fulfillment_id text PRIMARY KEY,
  order_id text NOT NULL,
  location_id text NOT NULL,
  status text NOT NULL CHECK (status IN ('open', 'in_progress', 'on_hold', 'fulfilled', 'cancelled')),
  created_at timestamptz NOT NULL,
  UNIQUE (order_id, location_id)
);

CREATE INDEX IF NOT EXISTS idx_fulfillment_order
  ON fulfillment_order(order_id, fulfillment_id);
CREATE INDEX IF NOT EXISTS idx_fulfillment_location_status
  ON fulfillment_order(location_id, status, created_at, fulfillment_id);

CREATE TABLE IF NOT EXISTS fulfillment_item (
  fulfillment_id text NOT NULL REFERENCES fulfillment_order(fulfillment_id) ON DELETE CASCADE,
  order_item_id text NOT NULL,
  sku text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  PRIMARY KEY (fulfillment_id, order_item_id)
);

CREATE INDEX IF NOT EXISTS idx_fulfillment_item_sku
  ON fulfillment_item(sku, fulfillment_id);

CREATE TABLE IF NOT EXISTS pick_item (
  pick_item_id text PRIMARY KEY,
  fulfillment_id text NOT NULL,
  order_item_id text NOT NULL,
  requested_quantity integer NOT NULL CHECK (requested_quantity > 0),
  picked_quantity integer NOT NULL DEFAULT 0 CHECK (picked_quantity >= 0),
  status text NOT NULL CHECK (status IN ('pending', 'picking', 'picked', 'short', 'cancelled')),
  UNIQUE (fulfillment_id, order_item_id),
  FOREIGN KEY (fulfillment_id, order_item_id)
    REFERENCES fulfillment_item(fulfillment_id, order_item_id) ON DELETE CASCADE,
  CHECK (picked_quantity <= requested_quantity)
);

CREATE INDEX IF NOT EXISTS idx_pick_item_status
  ON pick_item(fulfillment_id, status, pick_item_id);

CREATE TABLE IF NOT EXISTS package (
  package_id text PRIMARY KEY,
  fulfillment_id text NOT NULL REFERENCES fulfillment_order(fulfillment_id) ON DELETE CASCADE,
  weight_grams integer NOT NULL CHECK (weight_grams > 0),
  length_mm integer NOT NULL CHECK (length_mm > 0),
  width_mm integer NOT NULL CHECK (width_mm > 0),
  height_mm integer NOT NULL CHECK (height_mm > 0),
  created_at timestamptz NOT NULL,
  UNIQUE (package_id, fulfillment_id)
);

CREATE INDEX IF NOT EXISTS idx_package_fulfillment
  ON package(fulfillment_id, package_id);

CREATE TABLE IF NOT EXISTS package_item (
  package_id text NOT NULL,
  fulfillment_id text NOT NULL,
  order_item_id text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  PRIMARY KEY (package_id, order_item_id),
  FOREIGN KEY (package_id, fulfillment_id)
    REFERENCES package(package_id, fulfillment_id) ON DELETE CASCADE,
  FOREIGN KEY (fulfillment_id, order_item_id)
    REFERENCES fulfillment_item(fulfillment_id, order_item_id)
);

CREATE TABLE IF NOT EXISTS shipment (
  shipment_id text PRIMARY KEY,
  fulfillment_id text NOT NULL REFERENCES fulfillment_order(fulfillment_id),
  carrier text NOT NULL,
  tracking_number text NOT NULL UNIQUE,
  status text NOT NULL CHECK (status IN ('label_created', 'in_transit', 'delivered', 'delayed', 'exception', 'returned', 'lost')),
  shipped_at timestamptz,
  delivered_at timestamptz,
  UNIQUE (shipment_id, fulfillment_id),
  CHECK (delivered_at IS NULL OR (shipped_at IS NOT NULL AND delivered_at >= shipped_at))
);

CREATE INDEX IF NOT EXISTS idx_shipment_fulfillment
  ON shipment(fulfillment_id, shipment_id);
CREATE INDEX IF NOT EXISTS idx_shipment_status
  ON shipment(status, shipped_at, shipment_id);

CREATE TABLE IF NOT EXISTS shipment_package (
  shipment_id text NOT NULL,
  package_id text NOT NULL UNIQUE,
  fulfillment_id text NOT NULL,
  PRIMARY KEY (shipment_id, package_id),
  FOREIGN KEY (shipment_id, fulfillment_id)
    REFERENCES shipment(shipment_id, fulfillment_id) ON DELETE CASCADE,
  FOREIGN KEY (package_id, fulfillment_id)
    REFERENCES package(package_id, fulfillment_id)
);

CREATE TABLE IF NOT EXISTS tracking_event (
  tracking_event_id text PRIMARY KEY,
  shipment_id text NOT NULL REFERENCES shipment(shipment_id) ON DELETE CASCADE,
  status text NOT NULL CHECK (status IN ('label_created', 'picked_up', 'in_transit', 'out_for_delivery', 'delivered', 'delayed', 'exception', 'returned', 'lost')),
  description text NOT NULL,
  location text,
  occurred_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tracking_event_shipment
  ON tracking_event(shipment_id, occurred_at, tracking_event_id);

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
