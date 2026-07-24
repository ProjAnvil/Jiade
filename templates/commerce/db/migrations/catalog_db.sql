CREATE TABLE IF NOT EXISTS category (
  category_id text PRIMARY KEY,
  name text NOT NULL,
  parent_id text REFERENCES category(category_id),
  path text NOT NULL UNIQUE,
  CHECK (category_id <> ''),
  CHECK (name <> ''),
  CHECK (path LIKE '/%')
);

CREATE INDEX IF NOT EXISTS idx_category_parent
  ON category(parent_id, name);

CREATE TABLE IF NOT EXISTS brand (
  brand_id text PRIMARY KEY,
  name text NOT NULL UNIQUE,
  slug text NOT NULL UNIQUE,
  status text NOT NULL CHECK (status IN ('active', 'inactive')),
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Keep the legacy brand snapshot column for the current deterministic seed.
CREATE TABLE IF NOT EXISTS product (
  product_id text PRIMARY KEY,
  title text NOT NULL,
  description text NOT NULL,
  brand text NOT NULL,
  category_id text NOT NULL REFERENCES category(category_id),
  status text NOT NULL CHECK (status IN ('draft', 'active', 'archived')),
  created_at timestamptz NOT NULL,
  CHECK (title <> '')
);

CREATE INDEX IF NOT EXISTS idx_product_category
  ON product(category_id, status, product_id);
CREATE INDEX IF NOT EXISTS idx_product_status_created
  ON product(status, created_at DESC, product_id);

CREATE TABLE IF NOT EXISTS product_brand (
  product_id text PRIMARY KEY REFERENCES product(product_id) ON DELETE CASCADE,
  brand_id text NOT NULL REFERENCES brand(brand_id)
);

CREATE INDEX IF NOT EXISTS idx_product_brand_brand
  ON product_brand(brand_id, product_id);

CREATE TABLE IF NOT EXISTS product_media (
  media_id text PRIMARY KEY,
  product_id text NOT NULL REFERENCES product(product_id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('image', 'video')),
  url text NOT NULL,
  alt_text text NOT NULL DEFAULT '',
  position integer NOT NULL CHECK (position >= 0),
  UNIQUE (product_id, position),
  CHECK (url <> '')
);

CREATE INDEX IF NOT EXISTS idx_product_media_product
  ON product_media(product_id, position);

CREATE TABLE IF NOT EXISTS product_option (
  option_id text PRIMARY KEY,
  product_id text NOT NULL REFERENCES product(product_id) ON DELETE CASCADE,
  name text NOT NULL,
  position integer NOT NULL CHECK (position >= 0),
  UNIQUE (product_id, name),
  UNIQUE (product_id, position)
);

CREATE TABLE IF NOT EXISTS product_option_value (
  option_value_id text PRIMARY KEY,
  option_id text NOT NULL REFERENCES product_option(option_id) ON DELETE CASCADE,
  value text NOT NULL,
  position integer NOT NULL CHECK (position >= 0),
  UNIQUE (option_id, value),
  UNIQUE (option_id, position)
);

-- The first nine columns intentionally match the current seed contract.
CREATE TABLE IF NOT EXISTS variant (
  sku text PRIMARY KEY,
  product_id text NOT NULL REFERENCES product(product_id),
  title text NOT NULL,
  attributes jsonb NOT NULL DEFAULT '{}',
  barcode text UNIQUE,
  price_minor bigint NOT NULL CHECK (price_minor >= 0),
  compare_at_minor bigint CHECK (compare_at_minor IS NULL OR compare_at_minor >= price_minor),
  currency char(3) NOT NULL,
  weight_grams integer NOT NULL CHECK (weight_grams >= 0),
  CHECK (jsonb_typeof(attributes) = 'object')
);

CREATE INDEX IF NOT EXISTS idx_variant_product
  ON variant(product_id, sku);

CREATE TABLE IF NOT EXISTS variant_detail (
  sku text PRIMARY KEY REFERENCES variant(sku) ON DELETE CASCADE,
  length_mm integer NOT NULL CHECK (length_mm >= 0),
  width_mm integer NOT NULL CHECK (width_mm >= 0),
  height_mm integer NOT NULL CHECK (height_mm >= 0),
  status text NOT NULL CHECK (status IN ('draft', 'active', 'discontinued'))
);

CREATE INDEX IF NOT EXISTS idx_variant_detail_status
  ON variant_detail(status, sku);

CREATE TABLE IF NOT EXISTS variant_option_value (
  sku text NOT NULL REFERENCES variant(sku) ON DELETE CASCADE,
  option_value_id text NOT NULL REFERENCES product_option_value(option_value_id),
  PRIMARY KEY (sku, option_value_id)
);

CREATE INDEX IF NOT EXISTS idx_variant_option_value_option
  ON variant_option_value(option_value_id, sku);

CREATE TABLE IF NOT EXISTS price_list (
  price_list_id text PRIMARY KEY,
  name text NOT NULL,
  channel text NOT NULL,
  currency char(3) NOT NULL,
  valid_from timestamptz NOT NULL,
  valid_until timestamptz,
  status text NOT NULL CHECK (status IN ('draft', 'active', 'expired')),
  UNIQUE (name, channel, currency),
  CHECK (valid_until IS NULL OR valid_until > valid_from)
);

CREATE INDEX IF NOT EXISTS idx_price_list_channel_validity
  ON price_list(channel, currency, valid_from, valid_until)
  WHERE status = 'active';

CREATE TABLE IF NOT EXISTS variant_price (
  price_list_id text NOT NULL REFERENCES price_list(price_list_id) ON DELETE CASCADE,
  sku text NOT NULL REFERENCES variant(sku) ON DELETE CASCADE,
  price_minor bigint NOT NULL CHECK (price_minor >= 0),
  compare_at_minor bigint,
  PRIMARY KEY (price_list_id, sku),
  CHECK (compare_at_minor IS NULL OR compare_at_minor >= price_minor)
);

CREATE INDEX IF NOT EXISTS idx_variant_price_lookup
  ON variant_price(sku, price_list_id);

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
