CREATE TABLE IF NOT EXISTS cart (
  cart_id text PRIMARY KEY,
  customer_id text NOT NULL,
  status text NOT NULL CHECK (status IN ('active', 'converted', 'abandoned', 'expired')),
  currency char(3) NOT NULL,
  expires_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_cart_customer_status
  ON cart(customer_id, status, expires_at DESC, cart_id);
CREATE INDEX IF NOT EXISTS idx_cart_expiry
  ON cart(expires_at, cart_id)
  WHERE status = 'active';

CREATE TABLE IF NOT EXISTS cart_item (
  cart_id text NOT NULL REFERENCES cart(cart_id) ON DELETE CASCADE,
  sku text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  unit_price_minor bigint NOT NULL CHECK (unit_price_minor >= 0),
  PRIMARY KEY (cart_id, sku)
);

-- The first fifteen columns intentionally match the current seed contract.
CREATE TABLE IF NOT EXISTS sales_order (
  order_id text PRIMARY KEY,
  order_no text NOT NULL UNIQUE,
  customer_id text NOT NULL,
  status text NOT NULL CHECK (status IN ('pending', 'confirmed', 'cancelled', 'completed')),
  payment_status text NOT NULL CHECK (payment_status IN ('pending', 'authorized', 'paid', 'failed', 'partially_refunded', 'refunded')),
  fulfillment_status text NOT NULL CHECK (fulfillment_status IN ('unfulfilled', 'partial', 'fulfilled')),
  currency char(3) NOT NULL,
  subtotal_minor bigint NOT NULL CHECK (subtotal_minor >= 0),
  discount_minor bigint NOT NULL CHECK (discount_minor >= 0),
  shipping_minor bigint NOT NULL CHECK (shipping_minor >= 0),
  tax_minor bigint NOT NULL CHECK (tax_minor >= 0),
  total_minor bigint NOT NULL CHECK (total_minor >= 0),
  shipping_address jsonb NOT NULL,
  idempotency_key text NOT NULL UNIQUE,
  placed_at timestamptz NOT NULL,
  CHECK (discount_minor <= subtotal_minor),
  CHECK (total_minor = subtotal_minor - discount_minor + shipping_minor + tax_minor),
  CHECK (jsonb_typeof(shipping_address) = 'object')
);

CREATE INDEX IF NOT EXISTS idx_order_customer
  ON sales_order(customer_id, placed_at DESC, order_id);
CREATE INDEX IF NOT EXISTS idx_order_status
  ON sales_order(status, placed_at DESC, order_id);
CREATE INDEX IF NOT EXISTS idx_order_payment_fulfillment
  ON sales_order(payment_status, fulfillment_status, placed_at, order_id);

CREATE TABLE IF NOT EXISTS order_customer_snapshot (
  order_id text PRIMARY KEY REFERENCES sales_order(order_id) ON DELETE CASCADE,
  email text NOT NULL,
  name text NOT NULL,
  phone text,
  billing_address jsonb,
  CHECK (billing_address IS NULL OR jsonb_typeof(billing_address) = 'object')
);

CREATE TABLE IF NOT EXISTS order_item (
  order_item_id text PRIMARY KEY,
  order_id text NOT NULL REFERENCES sales_order(order_id) ON DELETE CASCADE,
  sku text NOT NULL,
  title text NOT NULL,
  quantity integer NOT NULL CHECK (quantity > 0),
  unit_price_minor bigint NOT NULL CHECK (unit_price_minor >= 0),
  discount_minor bigint NOT NULL CHECK (discount_minor >= 0),
  total_minor bigint NOT NULL CHECK (total_minor >= 0),
  CHECK (discount_minor <= unit_price_minor * quantity),
  CHECK (total_minor = unit_price_minor * quantity - discount_minor)
);

CREATE INDEX IF NOT EXISTS idx_order_item_order
  ON order_item(order_id, order_item_id);
CREATE INDEX IF NOT EXISTS idx_order_item_sku
  ON order_item(sku, order_id);

CREATE TABLE IF NOT EXISTS order_discount_allocation (
  allocation_id text PRIMARY KEY,
  order_id text NOT NULL REFERENCES sales_order(order_id) ON DELETE CASCADE,
  order_item_id text REFERENCES order_item(order_item_id) ON DELETE CASCADE,
  source text NOT NULL,
  amount_minor bigint NOT NULL CHECK (amount_minor > 0),
  UNIQUE (order_id, order_item_id, source)
);

CREATE INDEX IF NOT EXISTS idx_order_discount_order
  ON order_discount_allocation(order_id, allocation_id);

CREATE TABLE IF NOT EXISTS order_status_history (
  event_id text PRIMARY KEY,
  order_id text NOT NULL REFERENCES sales_order(order_id) ON DELETE CASCADE,
  from_status text,
  to_status text NOT NULL CHECK (to_status IN ('pending', 'confirmed', 'cancelled', 'completed')),
  reason text,
  occurred_at timestamptz NOT NULL,
  CHECK (from_status IS NULL OR from_status IN ('pending', 'confirmed', 'cancelled', 'completed'))
);

CREATE INDEX IF NOT EXISTS idx_order_status_history
  ON order_status_history(order_id, occurred_at, event_id);

CREATE TABLE IF NOT EXISTS order_saga (
  saga_id text PRIMARY KEY,
  order_id text NOT NULL UNIQUE REFERENCES sales_order(order_id) ON DELETE CASCADE,
  state text NOT NULL CHECK (state IN ('pending', 'reserving', 'paying', 'compensating', 'completed', 'failed')),
  version bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
  last_event_id uuid,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL,
  CHECK (updated_at >= created_at)
);

CREATE INDEX IF NOT EXISTS idx_order_saga_state
  ON order_saga(state, updated_at, saga_id)
  WHERE state NOT IN ('completed', 'failed');

CREATE TABLE IF NOT EXISTS order_saga_step (
  saga_id text NOT NULL REFERENCES order_saga(saga_id) ON DELETE CASCADE,
  step text NOT NULL CHECK (step IN ('customer_validated', 'catalog_snapshotted', 'inventory_reserved', 'payment_requested', 'inventory_released', 'refund_requested', 'fulfillment_requested')),
  status text NOT NULL CHECK (status IN ('pending', 'completed', 'failed', 'compensated')),
  event_id uuid,
  error_code text,
  updated_at timestamptz NOT NULL,
  PRIMARY KEY (saga_id, step)
);

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
