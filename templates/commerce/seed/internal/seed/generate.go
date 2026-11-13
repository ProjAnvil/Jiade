package seed

import (
	"fmt"
	"time"
)

// Generate builds the full in-memory Dataset for the given Config. It is the
// single deterministic entry point: the same Config always yields byte-identical
// non-database state. Loaders consume the Dataset (or stream it for the load
// scale) into PostgreSQL.
//
// Dev and demo scales materialise the whole Dataset. The load scale streams
// rows through CopyFrom and never retains the full Dataset; see load.go.
func Generate(cfg Config) (Dataset, error) {
	if err := cfg.validate(); err != nil {
		return Dataset{}, err
	}
	// generatedAt is fixed per (seed, scale) so timestamps are reproducible.
	// We anchor to the Unix epoch + seed seconds so different seeds shift the
	// whole calendar without perturbing inter-row offsets.
	generatedAt := generationAnchor(cfg)

	counts := countsFor(cfg.Scale)
	ds := Dataset{Config: cfg}

	// Domain order matters: catalog and customers are leaves; inventory
	// depends on variants; orders depend on customers+variants+inventory;
	// payments depend on orders; fulfillment depends on orders+items.
	generateCatalog(&ds, counts, cfg.Seed, generatedAt)
	generateCustomers(&ds, counts, cfg.Seed, generatedAt)
	generateInventory(&ds, counts, cfg.Seed, generatedAt)
	if err := generateOrders(&ds, counts, cfg.Seed, generatedAt); err != nil {
		return ds, err
	}
	if err := generatePayments(&ds, counts, cfg.Seed, generatedAt); err != nil {
		return ds, err
	}
	generateFulfillment(&ds, counts, cfg.Seed, generatedAt)
	return ds, nil
}

// GenerateSummary is the deterministic, database-free roll-up used by the
// golden-summary unit test. It never opens a connection.
func GenerateSummary(cfg Config) (Summary, error) {
	ds, err := Generate(cfg)
	if err != nil {
		return Summary{}, err
	}
	return ds.Summary(), nil
}

// generationAnchor returns a stable timestamp for a Config. We deliberately
// avoid time.Now() so two runs with the same seed produce identical sums.
func generationAnchor(cfg Config) time.Time {
	// Anchor: 2024-01-01T00:00:00Z plus the seed in seconds. Capped so a huge
	// seed does not overflow time arithmetic.
	epoch := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	shift := time.Duration(cfg.Seed%86400) * time.Second
	return epoch.Add(shift)
}

// validate rejects unknown scales and negative seeds before any allocation.
func (c Config) validate() error {
	switch c.Scale {
	case Dev, Demo, Load:
	default:
		return fmt.Errorf("seed: unknown scale %q", c.Scale)
	}
	if c.Seed < 0 {
		return fmt.Errorf("seed: negative seed %d", c.Seed)
	}
	return nil
}

// Summary rolls up the dataset into the golden-stable shape persisted to
// testdata/dev-summary.json.
func (ds Dataset) Summary() Summary {
	s := Summary{
		GeneratorVersion: GeneratorVersion,
		Scale:            ds.Config.Scale,
		Seed:             ds.Config.Seed,
		GeneratedAt:      generationAnchor(ds.Config),

		Products:        len(ds.Products),
		Variants:        len(ds.Variants),
		Brands:          len(ds.Brands),
		Categories:      len(ds.Categories),
		Customers:       len(ds.Customers),
		Addresses:       len(ds.Addresses),
		MembershipTiers: len(ds.MembershipTiers),
		Locations:       len(ds.Locations),
		InventoryLevels: len(ds.InventoryLevels),
		StockMovements:  len(ds.StockMovements),

		Orders:        len(ds.Orders),
		OrderItems:    len(ds.OrderItems),
		OrderTaxLines: len(ds.OrderTaxLines),
		Carts:         len(ds.Carts),
		CartItems:     len(ds.CartItems),

		PaymentIntents:  len(ds.PaymentIntents),
		PaymentAttempts: len(ds.PaymentAttempts),
		Refunds:         len(ds.Refunds),

		Fulfillments:     len(ds.Fulfillments),
		FulfillmentItems: len(ds.FulfillmentItems),
		Packages:         len(ds.Packages),
		Shipments:        len(ds.Shipments),
		TrackingEvents:   len(ds.TrackingEvents),
	}
	for _, order := range ds.Orders {
		s.SubtotalMinor += order.SubtotalMinor
		s.DiscountMinor += order.DiscountMinor
		s.ShippingMinor += order.ShippingMinor
		s.TaxMinor += order.TaxMinor
		s.TotalMinor += order.TotalMinor
	}
	for _, intent := range ds.PaymentIntents {
		// Captured = succeeded + refunded amounts net of refunds. We compute
		// the gross captured sum from succeeded attempts to stay robust.
		_ = intent
	}
	for _, attempt := range ds.PaymentAttempts {
		if attempt.Status == "succeeded" {
			s.CapturedMinor += attempt.AmountMinor
		}
	}
	for _, refund := range ds.Refunds {
		if refund.Status == "succeeded" {
			s.RefundedMinor += refund.AmountMinor
		}
	}
	return s
}
