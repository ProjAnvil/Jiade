package seed

import (
	"testing"
)

// TestConstraintAuditNoZeroAmounts is a constraint-driven sanity test: the
// schema rejects payment_intent.amount_minor <= 0 and
// payment_attempt.amount_minor <= 0, so the generator must never emit a
// zero-total order (which would force a zero intent amount).
func TestConstraintAuditNoZeroAmounts(t *testing.T) {
	for _, scale := range []Scale{Dev, Demo} {
		ds, err := Generate(Config{Scale: scale, Seed: 42})
		if err != nil {
			t.Fatalf("scale=%s generate: %v", scale, err)
		}
		zeroOrders := 0
		for _, o := range ds.Orders {
			if o.TotalMinor <= 0 {
				zeroOrders++
			}
		}
		if zeroOrders > 0 {
			t.Errorf("scale=%s: %d orders with total<=0 (would violate amount_minor>0)",
				scale, zeroOrders)
		}
		// Intent amount must equal order total and therefore also be > 0.
		zeroIntents := 0
		for _, pi := range ds.PaymentIntents {
			if pi.AmountMinor <= 0 {
				zeroIntents++
			}
		}
		if zeroIntents > 0 {
			t.Errorf("scale=%s: %d intents with amount<=0", scale, zeroIntents)
		}
		// Refunds must be > 0.
		zeroRefunds := 0
		for _, r := range ds.Refunds {
			if r.AmountMinor <= 0 {
				zeroRefunds++
			}
		}
		if zeroRefunds > 0 {
			t.Errorf("scale=%s: %d refunds with amount<=0", scale, zeroRefunds)
		}
	}
}

// TestConstraintAuditInventoryReconciles confirms on_hand >= reserved for
// every level (the inventory_db.sql CHECK constraint).
func TestConstraintAuditInventoryReconciles(t *testing.T) {
	ds, err := Generate(Config{Scale: Dev, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, lvl := range ds.InventoryLevels {
		if lvl.OnHand < 0 {
			t.Errorf("level %s/%s on_hand=%d < 0", lvl.SKU, lvl.LocationID, lvl.OnHand)
		}
		if lvl.Reserved < 0 {
			t.Errorf("level %s/%s reserved=%d < 0", lvl.SKU, lvl.LocationID, lvl.Reserved)
		}
		if lvl.Reserved > lvl.OnHand {
			t.Errorf("level %s/%s reserved=%d > on_hand=%d",
				lvl.SKU, lvl.LocationID, lvl.Reserved, lvl.OnHand)
		}
	}
}

// TestConstraintAuditMovementLedgerReconciles confirms sum(deltas by reason
// replenishment) per (sku, location) equals on_hand. The verifier rechecks
// this against the live ledger; this test pins the in-memory contract.
func TestConstraintAuditMovementLedgerReconciles(t *testing.T) {
	ds, err := Generate(Config{Scale: Dev, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	ledger := make(map[string]int)
	for _, mv := range ds.StockMovements {
		ledger[mv.SKU+"|"+mv.LocationID] += mv.Delta
	}
	for _, lvl := range ds.InventoryLevels {
		got := ledger[lvl.SKU+"|"+lvl.LocationID]
		if got != lvl.OnHand {
			t.Errorf("level %s/%s ledger=%d but on_hand=%d",
				lvl.SKU, lvl.LocationID, got, lvl.OnHand)
		}
	}
}

// TestConstraintAuditCompareAtGePrice confirms the variant compare_at_minor >=
// price_minor CHECK (and that compare_at_minor, when set, is non-negative).
func TestConstraintAuditCompareAtGePrice(t *testing.T) {
	ds, err := Generate(Config{Scale: Dev, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ds.Variants {
		if v.PriceMinor < 0 {
			t.Errorf("variant %s price=%d < 0", v.SKU, v.PriceMinor)
		}
		if v.CompareAtMinor != nil && *v.CompareAtMinor < v.PriceMinor {
			t.Errorf("variant %s compare_at=%d < price=%d",
				v.SKU, *v.CompareAtMinor, v.PriceMinor)
		}
	}
}

// TestConstraintAuditAddressDefaults confirms at most one default address per
// customer (matches idx_address_one_default partial unique index).
func TestConstraintAuditAddressDefaults(t *testing.T) {
	ds, err := Generate(Config{Scale: Dev, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	defaults := make(map[string]int)
	for _, a := range ds.Addresses {
		if a.IsDefault {
			defaults[a.CustomerID]++
		}
	}
	for customer, count := range defaults {
		if count > 1 {
			t.Errorf("customer %s has %d default addresses", customer, count)
		}
	}
}
