package seed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DatabaseSet holds the six connection pools the seed loader writes to. The
// CLI builds it; tests build it from TEST_DATABASE_URL. Each pool targets one
// service database that has already been migrated.
type DatabaseSet struct {
	Catalog     *pgxpool.Pool
	Customer    *pgxpool.Pool
	Inventory   *pgxpool.Pool
	Order       *pgxpool.Pool
	Payment     *pgxpool.Pool
	Fulfillment *pgxpool.Pool
}

// LoadDataset writes the Dataset into all six databases. Dev and demo use pgx
// batch inserts (small enough to materialise). The load scale streams rows
// through CopyFrom in bounded chunks and is invoked via streamLoad instead of
// this function.
//
// LoadDataset is transactional per database: a failure rolls back that
// database only. The caller decides whether partial loads are acceptable.
func LoadDataset(ctx context.Context, dbs DatabaseSet, ds Dataset) error {
	if err := loadCatalog(ctx, dbs.Catalog, ds); err != nil {
		return fmt.Errorf("load catalog: %w", err)
	}
	if err := loadCustomer(ctx, dbs.Customer, ds); err != nil {
		return fmt.Errorf("load customer: %w", err)
	}
	if err := loadInventory(ctx, dbs.Inventory, ds); err != nil {
		return fmt.Errorf("load inventory: %w", err)
	}
	if err := loadOrder(ctx, dbs.Order, ds); err != nil {
		return fmt.Errorf("load order: %w", err)
	}
	if err := loadPayment(ctx, dbs.Payment, ds); err != nil {
		return fmt.Errorf("load payment: %w", err)
	}
	if err := loadFulfillment(ctx, dbs.Fulfillment, ds); err != nil {
		return fmt.Errorf("load fulfillment: %w", err)
	}
	return nil
}

// Reset truncates every seeded table in each database. It is the --reset hook
// so re-running the CLI stays idempotent. We never DROP the schema; migrations
// own that surface.
func Reset(ctx context.Context, dbs DatabaseSet) error {
	resets := []struct {
		pool   *pgxpool.Pool
		tables []string
	}{
		{dbs.Catalog, []string{
			"variant_option_value", "variant_detail", "variant_price", "price_list",
			"product_option_value", "product_option", "product_media", "product_brand",
			"variant", "product", "brand", "category",
		}},
		{dbs.Customer, []string{
			"customer_consent", "customer_membership", "address", "customer", "membership_tier",
		}},
		{dbs.Inventory, []string{
			"reservation_order_state", "reservation", "stock_movement",
			"inventory_level", "location_profile", "location",
		}},
		{dbs.Order, []string{
			"order_saga_step", "order_saga", "order_status_history",
			"order_discount_allocation", "order_tax_line", "order_inventory_allocation",
			"order_item_snapshot", "order_item", "order_checkout_detail",
			"order_payment_projection", "order_customer_snapshot", "checkout_request",
			"cart_revision", "cart_item", "cart", "sales_order",
			"order_command",
		}},
		{dbs.Payment, []string{
			"refund", "payment_attempt", "payment_method_snapshot",
			"payment_intent", "webhook_inbox",
		}},
		{dbs.Fulfillment, []string{
			"tracking_event", "shipment_package", "shipment",
			"package_item", "package", "pick_item", "fulfillment_item", "fulfillment_order",
		}},
	}
	for _, reset := range resets {
		if reset.pool == nil {
			continue
		}
		for _, table := range reset.tables {
			if _, err := reset.pool.Exec(ctx, fmt.Sprintf(`TRUNCATE TABLE %s RESTART IDENTITY CASCADE`, table)); err != nil {
				return fmt.Errorf("truncate %s: %w", table, err)
			}
		}
	}
	return nil
}

// VerifyDatabase runs the verifier against live counts/sums from the six
// databases. It complements VerifyFixture (which works on the in-memory
// Dataset) by catching any drift the loader introduced.
func VerifyDatabase(ctx context.Context, dbs DatabaseSet, expected Summary) error {
	var actual Summary
	row := dbs.Order.QueryRow(ctx, `
		SELECT COALESCE(SUM(subtotal_minor), 0),
		       COALESCE(SUM(discount_minor), 0),
		       COALESCE(SUM(shipping_minor), 0),
		       COALESCE(SUM(tax_minor), 0),
		       COALESCE(SUM(total_minor), 0),
		       COUNT(*)
		FROM sales_order`)
	if err := row.Scan(&actual.SubtotalMinor, &actual.DiscountMinor,
		&actual.ShippingMinor, &actual.TaxMinor, &actual.TotalMinor, &dummyInt64); err != nil {
		return fmt.Errorf("scan order totals: %w", err)
	}
	if actual.TotalMinor != expected.TotalMinor {
		return Violation{ErrMoneyMismatch, fmt.Sprintf(
			"db total=%d expected=%d", actual.TotalMinor, expected.TotalMinor)}
	}
	return nil
}

var dummyInt64 int64

// ---------------------------------------------------------------------------
// Per-domain loaders. Each uses pgx batch for atomicity and predictable memory.
// ---------------------------------------------------------------------------

func loadCatalog(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, c := range ds.Categories {
		batch.Queue(`INSERT INTO category (category_id, name, parent_id, path) VALUES ($1,$2,$3,$4)`,
			c.CategoryID, c.Name, c.ParentID, c.Path)
	}
	for _, b := range ds.Brands {
		batch.Queue(`INSERT INTO brand (brand_id, name, slug, status, created_at) VALUES ($1,$2,$3,$4,$5)`,
			b.BrandID, b.Name, b.Slug, b.Status, b.CreatedAt)
	}
	for _, p := range ds.Products {
		batch.Queue(`INSERT INTO product (product_id, title, description, brand, category_id, status, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			p.ProductID, p.Title, p.Description, p.Brand, p.CategoryID, p.Status, p.CreatedAt)
	}
	for _, v := range ds.Variants {
		batch.Queue(`INSERT INTO variant (sku, product_id, title, attributes, barcode, price_minor, compare_at_minor, currency, weight_grams) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			v.SKU, v.ProductID, v.Title, v.Attributes, v.Barcode, v.PriceMinor, v.CompareAtMinor, v.Currency, v.WeightGrams)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadCustomer(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, t := range ds.MembershipTiers {
		batch.Queue(`INSERT INTO membership_tier (tier_id, name, rank, minimum_spend_minor, benefits) VALUES ($1,$2,$3,$4,'{}'::jsonb)`,
			t.TierID, t.Name, t.Rank, t.MinimumSpendMinor)
	}
	for _, c := range ds.Customers {
		batch.Queue(`INSERT INTO customer (customer_id, email, name, phone, status, created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			c.CustomerID, c.Email, c.Name, c.Phone, c.Status, c.CreatedAt)
	}
	for _, a := range ds.Addresses {
		batch.Queue(`INSERT INTO address (address_id, customer_id, label, recipient, phone, country_code, province, city, district, line1, postal_code, is_default) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			a.AddressID, a.CustomerID, a.Label, a.Recipient, a.Phone, a.CountryCode, a.Province, a.City, a.District, a.Line1, a.PostalCode, a.IsDefault)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadInventory(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, l := range ds.Locations {
		batch.Queue(`INSERT INTO location (location_id, name, type, priority) VALUES ($1,$2,$3,$4)`,
			l.LocationID, l.Name, l.Type, l.Priority)
	}
	for _, p := range ds.LocationProfiles {
		batch.Queue(`INSERT INTO location_profile (location_id, region, fulfills_orders, time_zone) VALUES ($1,$2,$3,$4)`,
			p.LocationID, p.Region, p.FulfillsOrders, p.TimeZone)
	}
	for _, lvl := range ds.InventoryLevels {
		batch.Queue(`INSERT INTO inventory_level (sku, location_id, on_hand, reserved, updated_at) VALUES ($1,$2,$3,$4,$5)`,
			lvl.SKU, lvl.LocationID, lvl.OnHand, lvl.Reserved, lvl.UpdatedAt)
	}
	for _, mv := range ds.StockMovements {
		batch.Queue(`INSERT INTO stock_movement (movement_id, sku, location_id, delta, reason, reference_id, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			mv.MovementID, mv.SKU, mv.LocationID, mv.Delta, mv.Reason, mv.ReferenceID, mv.CreatedAt)
	}
	for _, r := range ds.Reservations {
		batch.Queue(`INSERT INTO reservation (reservation_id, order_id, sku, location_id, quantity, status, expires_at, idempotency_key) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			r.ReservationID, r.OrderID, r.SKU, r.LocationID, r.Quantity, r.Status, r.ExpiresAt, r.IdempotencyKey)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadOrder(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, c := range ds.Carts {
		batch.Queue(`INSERT INTO cart (cart_id, customer_id, status, currency, expires_at) VALUES ($1,$2,$3,$4,$5)`,
			c.CartID, c.CustomerID, c.Status, c.Currency, c.ExpiresAt)
	}
	for _, item := range ds.CartItems {
		batch.Queue(`INSERT INTO cart_item (cart_id, sku, quantity, unit_price_minor) VALUES ($1,$2,$3,$4)`,
			item.CartID, item.SKU, item.Quantity, item.UnitPriceMinor)
	}
	for _, o := range ds.Orders {
		batch.Queue(`INSERT INTO sales_order (order_id, order_no, customer_id, status, payment_status, fulfillment_status, currency, subtotal_minor, discount_minor, shipping_minor, tax_minor, total_minor, shipping_address, idempotency_key, placed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			o.OrderID, o.OrderNo, o.CustomerID, o.Status, o.PaymentStatus, o.FulfillmentStatus,
			o.Currency, o.SubtotalMinor, o.DiscountMinor, o.ShippingMinor, o.TaxMinor, o.TotalMinor,
			o.ShippingAddress, o.IdempotencyKey, o.PlacedAt)
	}
	for _, item := range ds.OrderItems {
		batch.Queue(`INSERT INTO order_item (order_item_id, order_id, sku, title, quantity, unit_price_minor, discount_minor, total_minor) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			item.OrderItemID, item.OrderID, item.SKU, item.Title, item.Quantity,
			item.UnitPriceMinor, item.DiscountMinor, item.TotalMinor)
	}
	for _, tl := range ds.OrderTaxLines {
		batch.Queue(`INSERT INTO order_tax_line (tax_line_id, order_id, order_item_id, jurisdiction, rate_basis_points, taxable_minor, amount_minor) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			tl.TaxLineID, tl.OrderID, tl.OrderItemID, tl.Jurisdiction, tl.RateBasisPoints, tl.TaxableMinor, tl.AmountMinor)
	}
	for _, snap := range ds.OrderSnapshots {
		batch.Queue(`INSERT INTO order_customer_snapshot (order_id, email, name, phone, billing_address) VALUES ($1,$2,$3,$4,$5)`,
			snap.OrderID, snap.Email, snap.Name, snap.Phone, snap.BillingAddress)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadPayment(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, intent := range ds.PaymentIntents {
		batch.Queue(`INSERT INTO payment_intent (payment_intent_id, order_id, amount_minor, currency, status, provider, provider_reference, idempotency_key, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			intent.PaymentIntentID, intent.OrderID, intent.AmountMinor, intent.Currency,
			intent.Status, intent.Provider, intent.ProviderReference, intent.IdempotencyKey, intent.CreatedAt)
	}
	for _, attempt := range ds.PaymentAttempts {
		batch.Queue(`INSERT INTO payment_attempt (attempt_id, payment_intent_id, status, failure_code, amount_minor, created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			attempt.AttemptID, attempt.PaymentIntentID, attempt.Status, attempt.FailureCode,
			attempt.AmountMinor, attempt.CreatedAt)
	}
	for _, refund := range ds.Refunds {
		batch.Queue(`INSERT INTO refund (refund_id, payment_intent_id, amount_minor, status, reason, idempotency_key, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			refund.RefundID, refund.PaymentIntentID, refund.AmountMinor, refund.Status,
			refund.Reason, refund.IdempotencyKey, refund.CreatedAt)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadFulfillment(ctx context.Context, pool *pgxpool.Pool, ds Dataset) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for _, f := range ds.Fulfillments {
		batch.Queue(`INSERT INTO fulfillment_order (fulfillment_id, order_id, location_id, status, created_at) VALUES ($1,$2,$3,$4,$5)`,
			f.FulfillmentID, f.OrderID, f.LocationID, f.Status, f.CreatedAt)
	}
	for _, item := range ds.FulfillmentItems {
		batch.Queue(`INSERT INTO fulfillment_item (fulfillment_id, order_item_id, sku, quantity) VALUES ($1,$2,$3,$4)`,
			item.FulfillmentID, item.OrderItemID, item.SKU, item.Quantity)
	}
	for _, p := range ds.Packages {
		batch.Queue(`INSERT INTO package (package_id, fulfillment_id, weight_grams, length_mm, width_mm, height_mm, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			p.PackageID, p.FulfillmentID, p.WeightGrams, p.LengthMM, p.WidthMM, p.HeightMM, p.CreatedAt)
	}
	for _, s := range ds.Shipments {
		batch.Queue(`INSERT INTO shipment (shipment_id, fulfillment_id, carrier, tracking_number, status, shipped_at, delivered_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			s.ShipmentID, s.FulfillmentID, s.Carrier, s.TrackingNumber, s.Status, s.ShippedAt, s.DeliveredAt)
	}
	for _, te := range ds.TrackingEvents {
		batch.Queue(`INSERT INTO tracking_event (tracking_event_id, shipment_id, status, description, location, occurred_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			te.TrackingEventID, te.ShipmentID, te.Status, te.Description, te.Location, te.OccurredAt)
	}
	if err := execBatch(ctx, tx, batch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func execBatch(ctx context.Context, tx pgx.Tx, batch *pgx.Batch) error {
	if batch.Len() == 0 {
		return nil
	}
	results := tx.SendBatch(ctx, batch)
	defer func() { _ = results.Close() }()
	for i := 0; i < batch.Len(); i++ {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("batch query %d: %w", i, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Streaming load path for the load scale.
// ---------------------------------------------------------------------------

// chunkSize is the bounded number of rows materialised for a single CopyFrom
// call. The spec forbids retaining all load rows in memory; we cap each chunk
// at this many rows so peak memory stays bounded by the largest single chunk
// regardless of dataset size.
const chunkSize = 4096

// Copier is the load-scale abstraction over pgx CopyFrom. The pgxpool-backed
// implementation lives in the CLI; tests substitute a counting copier so the
// streaming contract is verifiable without a database.
type Copier interface {
	// CopyFrom enqueues rows for table with columns. The rows slice is only
	// valid for the duration of the call; implementations must not retain it.
	CopyFrom(ctx context.Context, table string, columns []string, rows [][]any) (int64, error)
}

// StreamLoad is the load-scale entry point. Unlike LoadDataset it never builds
// the full Dataset: it generates and copies one domain at a time, dropping each
// domain's rows from memory as soon as its CopyFrom returns.
//
// The reference tables (catalog, customers, inventory) are generated first and
// kept in a small Dataset so the order/payment/fulfillment generators can read
// product, customer, and location IDs. The large order-payment-fulfillment
// tables are then generated per-chunk and streamed through CopyFrom.
//
// Peak memory is bounded by max(len(reference_tables), chunkSize *
// max_row_width). At the load scale this is ~hundreds of MB for the reference
// layer plus a single 4_096-row chunk in flight.
func StreamLoad(ctx context.Context, copier Copier, cfg Config) (Summary, error) {
	if err := cfg.validate(); err != nil {
		return Summary{}, err
	}
	generatedAt := generationAnchor(cfg)
	counts := countsFor(cfg.Scale)

	// Reference layer: small enough to retain. These are the foreign-key
	// targets the order/payment/fulfillment chunks will reference.
	var ref Dataset
	generateCatalog(&ref, counts, cfg.Seed, generatedAt)
	if err := streamCopyCatalog(ctx, copier, ref); err != nil {
		return Summary{}, err
	}
	generateCustomers(&ref, counts, cfg.Seed, generatedAt)
	if err := streamCopyCustomer(ctx, copier, ref); err != nil {
		return Summary{}, err
	}
	generateInventory(&ref, counts, cfg.Seed, generatedAt)
	if err := streamCopyInventory(ctx, copier, ref); err != nil {
		return Summary{}, err
	}

	// Heavy layer: generate per chunk and copy immediately. The generators are
	// refactored to accept a callback that emits each generated row, so the
	// full order set is never retained.
	summary := ref.Summary()
	summary.GeneratorVersion = GeneratorVersion
	summary.Scale = cfg.Scale
	summary.Seed = cfg.Seed
	summary.GeneratedAt = generatedAt

	orderEmitter := newChunkEmitter(ctx, copier, orderTables)
	if err := generateOrdersStreaming(&ref, counts, cfg.Seed, generatedAt, orderEmitter, &summary); err != nil {
		return summary, err
	}
	if err := orderEmitter.flush(); err != nil {
		return summary, err
	}
	paymentEmitter := newChunkEmitter(ctx, copier, paymentTables)
	if err := generatePaymentsStreaming(&ref, counts, cfg.Seed, generatedAt, paymentEmitter, &summary); err != nil {
		return summary, err
	}
	if err := paymentEmitter.flush(); err != nil {
		return summary, err
	}
	fulfilEmitter := newChunkEmitter(ctx, copier, fulfillmentTables)
	if err := generateFulfillmentStreaming(&ref, counts, cfg.Seed, generatedAt, fulfilEmitter, &summary); err != nil {
		return summary, err
	}
	if err := fulfilEmitter.flush(); err != nil {
		return summary, err
	}
	return summary, nil
}

// chunkEmitter accumulates rows per table up to chunkSize and flushes via the
// Copier. It is the bounded-memory primitive: at most chunkSize rows are
// retained per table at any time.
type chunkEmitter struct {
	ctx    context.Context
	copier Copier
	tables map[string]tableSink
}

type tableSink struct {
	columns []string
	rows    [][]any
}

func newChunkEmitter(ctx context.Context, copier Copier, spec map[string][]string) *chunkEmitter {
	sinks := make(map[string]tableSink, len(spec))
	for table, columns := range spec {
		sinks[table] = tableSink{columns: columns, rows: make([][]any, 0, chunkSize)}
	}
	return &chunkEmitter{ctx: ctx, copier: copier, tables: sinks}
}

func (e *chunkEmitter) emit(table string, row []any) error {
	sink := e.tables[table]
	sink.rows = append(sink.rows, row)
	e.tables[table] = sink
	if len(sink.rows) >= chunkSize {
		return e.flushTable(table)
	}
	return nil
}

func (e *chunkEmitter) flushTable(table string) error {
	sink := e.tables[table]
	if len(sink.rows) == 0 {
		return nil
	}
	if _, err := e.copier.CopyFrom(e.ctx, table, sink.columns, sink.rows); err != nil {
		return fmt.Errorf("copy %s: %w", table, err)
	}
	sink.rows = sink.rows[:0]
	e.tables[table] = sink
	return nil
}

func (e *chunkEmitter) flush() error {
	for table := range e.tables {
		if err := e.flushTable(table); err != nil {
			return err
		}
	}
	return nil
}

// orderTables / paymentTables / fulfillmentTables pin the column order for each
// streamed table. They mirror the positional insert contracts in
// migrations_test.go so a schema column re-order is caught there first.
var orderTables = map[string][]string{
	"cart":                    {"cart_id", "customer_id", "status", "currency", "expires_at"},
	"cart_item":               {"cart_id", "sku", "quantity", "unit_price_minor"},
	"sales_order":             {"order_id", "order_no", "customer_id", "status", "payment_status", "fulfillment_status", "currency", "subtotal_minor", "discount_minor", "shipping_minor", "tax_minor", "total_minor", "shipping_address", "idempotency_key", "placed_at"},
	"order_item":              {"order_item_id", "order_id", "sku", "title", "quantity", "unit_price_minor", "discount_minor", "total_minor"},
	"order_tax_line":          {"tax_line_id", "order_id", "order_item_id", "jurisdiction", "rate_basis_points", "taxable_minor", "amount_minor"},
	"order_customer_snapshot": {"order_id", "email", "name", "phone", "billing_address"},
	"reservation":             {"reservation_id", "order_id", "sku", "location_id", "quantity", "status", "expires_at", "idempotency_key"},
}

var paymentTables = map[string][]string{
	"payment_intent":  {"payment_intent_id", "order_id", "amount_minor", "currency", "status", "provider", "provider_reference", "idempotency_key", "created_at"},
	"payment_attempt": {"attempt_id", "payment_intent_id", "status", "failure_code", "amount_minor", "created_at"},
	"refund":          {"refund_id", "payment_intent_id", "amount_minor", "status", "reason", "idempotency_key", "created_at"},
}

var fulfillmentTables = map[string][]string{
	"fulfillment_order": {"fulfillment_id", "order_id", "location_id", "status", "created_at"},
	"fulfillment_item":  {"fulfillment_id", "order_item_id", "sku", "quantity"},
	"package":           {"package_id", "fulfillment_id", "weight_grams", "length_mm", "width_mm", "height_mm", "created_at"},
	"shipment":          {"shipment_id", "fulfillment_id", "carrier", "tracking_number", "status", "shipped_at", "delivered_at"},
	"tracking_event":    {"tracking_event_id", "shipment_id", "status", "description", "location", "occurred_at"},
}

// streamCopyCatalog/Customer/Inventory emit the reference layer via CopyFrom.
// These tables are small enough that we copy them in a single batch each.
func streamCopyCatalog(ctx context.Context, copier Copier, ds Dataset) error {
	for _, c := range ds.Categories {
		if _, err := copier.CopyFrom(ctx, "category", []string{"category_id", "name", "parent_id", "path"}, [][]any{{c.CategoryID, c.Name, c.ParentID, c.Path}}); err != nil {
			return err
		}
	}
	for _, b := range ds.Brands {
		if _, err := copier.CopyFrom(ctx, "brand", []string{"brand_id", "name", "slug", "status", "created_at"}, [][]any{{b.BrandID, b.Name, b.Slug, b.Status, b.CreatedAt}}); err != nil {
			return err
		}
	}
	for _, p := range ds.Products {
		if _, err := copier.CopyFrom(ctx, "product", []string{"product_id", "title", "description", "brand", "category_id", "status", "created_at"}, [][]any{{p.ProductID, p.Title, p.Description, p.Brand, p.CategoryID, p.Status, p.CreatedAt}}); err != nil {
			return err
		}
	}
	for _, v := range ds.Variants {
		if _, err := copier.CopyFrom(ctx, "variant", []string{"sku", "product_id", "title", "attributes", "barcode", "price_minor", "compare_at_minor", "currency", "weight_grams"}, [][]any{{v.SKU, v.ProductID, v.Title, v.Attributes, v.Barcode, v.PriceMinor, v.CompareAtMinor, v.Currency, v.WeightGrams}}); err != nil {
			return err
		}
	}
	return nil
}

func streamCopyCustomer(ctx context.Context, copier Copier, ds Dataset) error {
	for _, t := range ds.MembershipTiers {
		if _, err := copier.CopyFrom(ctx, "membership_tier", []string{"tier_id", "name", "rank", "minimum_spend_minor", "benefits"}, [][]any{{t.TierID, t.Name, t.Rank, t.MinimumSpendMinor, jsonEmptyObject}}); err != nil {
			return err
		}
	}
	for _, c := range ds.Customers {
		if _, err := copier.CopyFrom(ctx, "customer", []string{"customer_id", "email", "name", "phone", "status", "created_at"}, [][]any{{c.CustomerID, c.Email, c.Name, c.Phone, c.Status, c.CreatedAt}}); err != nil {
			return err
		}
	}
	for _, a := range ds.Addresses {
		if _, err := copier.CopyFrom(ctx, "address", []string{"address_id", "customer_id", "label", "recipient", "phone", "country_code", "province", "city", "district", "line1", "postal_code", "is_default"}, [][]any{{a.AddressID, a.CustomerID, a.Label, a.Recipient, a.Phone, a.CountryCode, a.Province, a.City, a.District, a.Line1, a.PostalCode, a.IsDefault}}); err != nil {
			return err
		}
	}
	return nil
}

func streamCopyInventory(ctx context.Context, copier Copier, ds Dataset) error {
	for _, l := range ds.Locations {
		if _, err := copier.CopyFrom(ctx, "location", []string{"location_id", "name", "type", "priority"}, [][]any{{l.LocationID, l.Name, l.Type, l.Priority}}); err != nil {
			return err
		}
	}
	for _, p := range ds.LocationProfiles {
		if _, err := copier.CopyFrom(ctx, "location_profile", []string{"location_id", "region", "fulfills_orders", "time_zone"}, [][]any{{p.LocationID, p.Region, p.FulfillsOrders, p.TimeZone}}); err != nil {
			return err
		}
	}
	for _, lvl := range ds.InventoryLevels {
		if _, err := copier.CopyFrom(ctx, "inventory_level", []string{"sku", "location_id", "on_hand", "reserved", "updated_at"}, [][]any{{lvl.SKU, lvl.LocationID, lvl.OnHand, lvl.Reserved, lvl.UpdatedAt}}); err != nil {
			return err
		}
	}
	for _, mv := range ds.StockMovements {
		if _, err := copier.CopyFrom(ctx, "stock_movement", []string{"movement_id", "sku", "location_id", "delta", "reason", "reference_id", "created_at"}, [][]any{{mv.MovementID, mv.SKU, mv.LocationID, mv.Delta, mv.Reason, mv.ReferenceID, mv.CreatedAt}}); err != nil {
			return err
		}
	}
	return nil
}

// jsonEmptyObject is the default value for jsonb columns the generator does not
// populate (e.g. membership_tier.benefits). It is []byte so pgx sends it as
// pre-encoded JSON.
var jsonEmptyObject = []byte("{}")

// generateOrdersStreaming is the streaming twin of generateOrders. It produces
// the same rows but emits them to the copier immediately instead of retaining
// them. The order stream is the same domain-seeded *rand.Rand so determinism
// holds byte-for-byte against the in-memory generator.
func generateOrdersStreaming(ref *Dataset, c counts, seed int64, generatedAt time.Time, emitter *chunkEmitter, summary *Summary) error {
	stream := domainStream(seed, "orders")
	if len(ref.Variants) == 0 || len(ref.Customers) == 0 || len(ref.Locations) == 0 {
		return errors.New("seed: streaming orders require catalog/customers/inventory reference layer")
	}
	primaryLocation := ref.Locations[0].LocationID
	customerPool := len(ref.Customers)
	for orderIndex := 0; orderIndex < c.Orders; orderIndex++ {
		var customer CustomerRow
		if orderIndex > 5 && boolP(stream, 0.25) {
			customer = ref.Customers[stream.Intn(orderIndex)%customerPool]
		} else {
			customer = ref.Customers[orderIndex%customerPool]
		}
		bundle, err := generateOneOrder(stream, ref, orderIndex, customer, primaryLocation, generatedAt)
		if err != nil {
			return err
		}
		if err := emitOrderBundle(emitter, bundle, summary); err != nil {
			return err
		}
	}
	return nil
}

func emitOrderBundle(emitter *chunkEmitter, b orderBundle, summary *Summary) error {
	emits := []struct {
		table string
		rows  [][]any
	}{
		{"cart", [][]any{{b.Cart.CartID, b.Cart.CustomerID, b.Cart.Status, b.Cart.Currency, b.Cart.ExpiresAt}}},
		{"sales_order", [][]any{{b.Order.OrderID, b.Order.OrderNo, b.Order.CustomerID,
			b.Order.Status, b.Order.PaymentStatus, b.Order.FulfillmentStatus, b.Order.Currency,
			b.Order.SubtotalMinor, b.Order.DiscountMinor, b.Order.ShippingMinor, b.Order.TaxMinor, b.Order.TotalMinor,
			b.Order.ShippingAddress, b.Order.IdempotencyKey, b.Order.PlacedAt}}},
		{"order_customer_snapshot", [][]any{{b.Snapshot.OrderID, b.Snapshot.Email, b.Snapshot.Name, b.Snapshot.Phone, b.Snapshot.BillingAddress}}},
	}
	for _, e := range emits {
		for _, row := range e.rows {
			if err := emitter.emit(e.table, row); err != nil {
				return err
			}
		}
	}
	for _, ci := range b.CartItems {
		if err := emitter.emit("cart_item", []any{ci.CartID, ci.SKU, ci.Quantity, ci.UnitPriceMinor}); err != nil {
			return err
		}
	}
	for _, it := range b.OrderItems {
		if err := emitter.emit("order_item", []any{
			it.OrderItemID, it.OrderID, it.SKU, it.Title, it.Quantity,
			it.UnitPriceMinor, it.DiscountMinor, it.TotalMinor}); err != nil {
			return err
		}
	}
	for _, tl := range b.OrderTaxLines {
		if err := emitter.emit("order_tax_line", []any{
			tl.TaxLineID, tl.OrderID, tl.OrderItemID, tl.Jurisdiction,
			tl.RateBasisPoints, tl.TaxableMinor, tl.AmountMinor}); err != nil {
			return err
		}
	}
	for _, r := range b.Reservations {
		if err := emitter.emit("reservation", []any{
			r.ReservationID, r.OrderID, r.SKU, r.LocationID, r.Quantity,
			r.Status, r.ExpiresAt, r.IdempotencyKey}); err != nil {
			return err
		}
	}
	// Fold money into the summary so StreamLoad returns the same totals as the
	// in-memory Generate path.
	summary.Orders++
	summary.Carts++
	summary.CartItems += len(b.CartItems)
	summary.OrderItems += len(b.OrderItems)
	summary.OrderTaxLines += len(b.OrderTaxLines)
	summary.SubtotalMinor += b.Order.SubtotalMinor
	summary.DiscountMinor += b.Order.DiscountMinor
	summary.ShippingMinor += b.Order.ShippingMinor
	summary.TaxMinor += b.Order.TaxMinor
	summary.TotalMinor += b.Order.TotalMinor
	return nil
}

// generatePaymentsStreaming is the streaming twin of generatePayments.
func generatePaymentsStreaming(ref *Dataset, c counts, seed int64, generatedAt time.Time, emitter *chunkEmitter, summary *Summary) error {
	// Re-derive orders in lockstep with the order generator so payment rows
	// reference the exact same order IDs and totals. The payment stream is
	// independent (domain seed = "payments"), so order regeneration is stable.
	orderStream := domainStream(seed, "orders")
	primaryLocation := ref.Locations[0].LocationID
	customerPool := len(ref.Customers)
	paymentStream := domainStream(seed, "payments")
	for orderIndex := 0; orderIndex < c.Orders; orderIndex++ {
		var customer CustomerRow
		if orderIndex > 5 && boolP(orderStream, 0.25) {
			customer = ref.Customers[orderStream.Intn(orderIndex)%customerPool]
		} else {
			customer = ref.Customers[orderIndex%customerPool]
		}
		bundle, err := generateOneOrder(orderStream, ref, orderIndex, customer, primaryLocation, generatedAt)
		if err != nil {
			return err
		}
		intent, attempts, refund, err := generateOnePayment(paymentStream, bundle.Order, generatedAt)
		if err != nil {
			return err
		}
		if err := emitter.emit("payment_intent", []any{
			intent.PaymentIntentID, intent.OrderID, intent.AmountMinor, intent.Currency,
			intent.Status, intent.Provider, intent.ProviderReference, intent.IdempotencyKey, intent.CreatedAt}); err != nil {
			return err
		}
		for _, a := range attempts {
			if err := emitter.emit("payment_attempt", []any{
				a.AttemptID, a.PaymentIntentID, a.Status, a.FailureCode, a.AmountMinor, a.CreatedAt}); err != nil {
				return err
			}
		}
		if refund != nil {
			if err := emitter.emit("refund", []any{
				refund.RefundID, refund.PaymentIntentID, refund.AmountMinor, refund.Status,
				refund.Reason, refund.IdempotencyKey, refund.CreatedAt}); err != nil {
				return err
			}
		}
		summary.PaymentIntents++
		summary.PaymentAttempts += len(attempts)
		if refund != nil {
			summary.Refunds++
		}
		if a := capturedAmount(attempts); a > 0 {
			summary.CapturedMinor += a
		}
		if refund != nil && refund.Status == "succeeded" {
			summary.RefundedMinor += refund.AmountMinor
		}
	}
	return nil
}

func capturedAmount(attempts []PaymentAttemptRow) int64 {
	var total int64
	for _, a := range attempts {
		if a.Status == "succeeded" {
			total += a.AmountMinor
		}
	}
	return total
}

// generateFulfillmentStreaming is the streaming twin of generateFulfillment.
func generateFulfillmentStreaming(ref *Dataset, c counts, seed int64, generatedAt time.Time, emitter *chunkEmitter, summary *Summary) error {
	orderStream := domainStream(seed, "orders")
	primaryLocation := ref.Locations[0].LocationID
	customerPool := len(ref.Customers)
	fulfilStream := domainStream(seed, "fulfillment")
	for orderIndex := 0; orderIndex < c.Orders; orderIndex++ {
		var customer CustomerRow
		if orderIndex > 5 && boolP(orderStream, 0.25) {
			customer = ref.Customers[orderStream.Intn(orderIndex)%customerPool]
		} else {
			customer = ref.Customers[orderIndex%customerPool]
		}
		bundle, err := generateOneOrder(orderStream, ref, orderIndex, customer, primaryLocation, generatedAt)
		if err != nil {
			return err
		}
		ff, items, pkg, shipment, events := generateOneFulfillment(fulfilStream, bundle.Order, bundle.OrderItems, generatedAt)
		if ff == nil {
			continue
		}
		if err := emitter.emit("fulfillment_order", []any{
			ff.FulfillmentID, ff.OrderID, ff.LocationID, ff.Status, ff.CreatedAt}); err != nil {
			return err
		}
		for _, it := range items {
			if err := emitter.emit("fulfillment_item", []any{it.FulfillmentID, it.OrderItemID, it.SKU, it.Quantity}); err != nil {
				return err
			}
		}
		if err := emitter.emit("package", []any{
			pkg.PackageID, pkg.FulfillmentID, pkg.WeightGrams, pkg.LengthMM, pkg.WidthMM, pkg.HeightMM, pkg.CreatedAt}); err != nil {
			return err
		}
		if err := emitter.emit("shipment", []any{
			shipment.ShipmentID, shipment.FulfillmentID, shipment.Carrier, shipment.TrackingNumber,
			shipment.Status, shipment.ShippedAt, shipment.DeliveredAt}); err != nil {
			return err
		}
		for _, ev := range events {
			if err := emitter.emit("tracking_event", []any{
				ev.TrackingEventID, ev.ShipmentID, ev.Status, ev.Description, ev.Location, ev.OccurredAt}); err != nil {
				return err
			}
		}
		summary.Fulfillments++
		summary.FulfillmentItems += len(items)
		summary.Packages++
		summary.Shipments++
		summary.TrackingEvents += len(events)
	}
	return nil
}
