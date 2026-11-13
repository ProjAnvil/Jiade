// Package seed generates deterministic, reproducible commerce data across all
// six service databases (catalog, customer, inventory, order, payment,
// fulfillment) plus an in-memory integrity verifier.
//
// Money is expressed in int64 minor units throughout; no binary floating point
// is used to compute or store monetary values.
package seed

import (
	"fmt"
	"time"
)

// Scale selects the dataset volume produced by Generate.
type Scale string

const (
	// Dev produces a small, golden-stable dataset (80 products, 100 orders).
	Dev Scale = "dev"
	// Demo produces a mid-sized dataset suitable for staging demos.
	Demo Scale = "demo"
	// Load produces a large dataset via streaming CopyFrom (never retained in
	// memory in full).
	Load Scale = "load"
)

// ParseScale converts a CLI string into a Scale, returning an error for unknown
// values so the CLI surface rejects typos before any work is done.
func ParseScale(text string) (Scale, error) {
	switch Scale(text) {
	case Dev, Demo, Load:
		return Scale(text), nil
	default:
		return "", fmt.Errorf("unknown scale %q (want dev, demo, or load)", text)
	}
}

// Config controls a single seed run. The same Seed+Scale+GeneratorVersion must
// always produce identical data.
type Config struct {
	Scale Scale
	Seed  int64
	Reset bool
}

// GeneratorVersion is bumped whenever the generator algorithm changes in a way
// that invalidates previously issued golden summaries.
const GeneratorVersion = "2026-07-24.v1"

// Counts derived from the parent spec. Centralising them here keeps the
// generators, the CLI, and the verifier in lockstep.
type counts struct {
	Products        int
	Customers       int
	Locations       int
	MembershipTiers int
	Brands          int
	Categories      int
	Orders          int
	CartsPerOrder   int
}

func countsFor(scale Scale) counts {
	switch scale {
	case Dev:
		return counts{
			Products:        80,
			Customers:       60,
			Locations:       3,
			MembershipTiers: 3,
			Brands:          12,
			Categories:      8,
			Orders:          100,
			CartsPerOrder:   1,
		}
	case Demo:
		return counts{
			Products:        1_500,
			Customers:       5_000,
			Locations:       6,
			MembershipTiers: 3,
			Brands:          40,
			Categories:      24,
			Orders:          10_000,
			CartsPerOrder:   1,
		}
	case Load:
		return counts{
			Products:        12_000,
			Customers:       250_000,
			Locations:       12,
			MembershipTiers: 3,
			Brands:          120,
			Categories:      48,
			Orders:          1_000_000,
			CartsPerOrder:   1,
		}
	default:
		return counts{}
	}
}

// Summary is the deterministic, golden-stable roll-up of a generated dataset.
// GenerateSummary is the only surface the unit tests depend on; it must never
// touch a database.
type Summary struct {
	GeneratorVersion string    `json:"generator_version"`
	Scale            Scale     `json:"scale"`
	Seed             int64     `json:"seed"`
	GeneratedAt      time.Time `json:"generated_at"`

	Products        int `json:"products"`
	Variants        int `json:"variants"`
	Brands          int `json:"brands"`
	Categories      int `json:"categories"`
	Customers       int `json:"customers"`
	Addresses       int `json:"addresses"`
	MembershipTiers int `json:"membership_tiers"`
	Locations       int `json:"locations"`
	InventoryLevels int `json:"inventory_levels"`
	StockMovements  int `json:"stock_movements"`

	Orders        int `json:"orders"`
	OrderItems    int `json:"order_items"`
	OrderTaxLines int `json:"order_tax_lines"`
	Carts         int `json:"carts"`
	CartItems     int `json:"cart_items"`

	PaymentIntents  int `json:"payment_intents"`
	PaymentAttempts int `json:"payment_attempts"`
	Refunds         int `json:"refunds"`

	Fulfillments     int `json:"fulfillments"`
	FulfillmentItems int `json:"fulfillment_items"`
	Packages         int `json:"packages"`
	Shipments        int `json:"shipments"`
	TrackingEvents   int `json:"tracking_events"`

	// Money totals in int64 minor units. They are the sum of the matching
	// per-row fields and are verified by the integrity verifier.
	SubtotalMinor int64 `json:"subtotal_minor"`
	DiscountMinor int64 `json:"discount_minor"`
	ShippingMinor int64 `json:"shipping_minor"`
	TaxMinor      int64 `json:"tax_minor"`
	TotalMinor    int64 `json:"total_minor"`
	CapturedMinor int64 `json:"captured_minor"`
	RefundedMinor int64 `json:"refunded_minor"`
}

// catalogSummary, customerSummary, etc. live in their domain files; the master
// Summary is composed by GenerateSummary from a fully generated Dataset.
