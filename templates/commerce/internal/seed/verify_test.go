package seed

import (
	"errors"
	"testing"
)

// TestVerifyRejectsBrokenOrderEquation is the canonical verifier test from
// the spec: bumping an order's total by one must surface as ErrMoneyMismatch.
func TestVerifyRejectsBrokenOrderEquation(t *testing.T) {
	fixture := validFixture(t)
	fixture.Orders[0].TotalMinor++
	if err := VerifyFixture(fixture); !errors.Is(err, ErrMoneyMismatch) {
		t.Fatalf("error=%v, want ErrMoneyMismatch", err)
	}
}

// TestVerifyAcceptsValidFixture confirms a freshly generated dev dataset
// passes every check. This pins the generator's correctness between releases.
func TestVerifyAcceptsValidFixture(t *testing.T) {
	ds := validFixture(t)
	if err := VerifyFixture(ds); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}

// TestVerifyRejectsBadStatusTransition exercises the state-matrix branch.
func TestVerifyRejectsBadStatusTransition(t *testing.T) {
	ds := validFixture(t)
	// completed requires paid-family payment + fulfilled shipping. Force an
	// invalid combo and confirm the verifier flags it.
	ds.Orders[0].Status = "completed"
	ds.Orders[0].PaymentStatus = "pending"
	ds.Orders[0].FulfillmentStatus = "unfulfilled"
	err := VerifyFixture(ds)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error=%v, want ErrInvalidTransition", err)
	}
}

// TestVerifyRejectsOrphanedOrderItem exercises the orphaned-FK branch.
func TestVerifyRejectsOrphanedOrderItem(t *testing.T) {
	ds := validFixture(t)
	ds.OrderItems = append(ds.OrderItems, OrderItemRow{
		OrderItemID:    "ghost-item",
		OrderID:        "no-such-order",
		SKU:            "sku-ghost",
		Title:          "Ghost",
		Quantity:       1,
		UnitPriceMinor: 100,
		DiscountMinor:  0,
		TotalMinor:     100,
	})
	err := VerifyFixture(ds)
	if !errors.Is(err, ErrOrphanedReference) {
		t.Fatalf("error=%v, want ErrOrphanedReference", err)
	}
}

// TestVerifyRejectsFailedAttemptWithoutCode exercises the payment-trigger
// invariant in the verifier.
func TestVerifyRejectsFailedAttemptWithoutCode(t *testing.T) {
	ds := validFixture(t)
	ds.PaymentAttempts[0].Status = "failed"
	ds.PaymentAttempts[0].FailureCode = nil
	err := VerifyFixture(ds)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error=%v, want ErrInvalidTransition", err)
	}
}

// validFixture returns a hermetic, fully-valid dev dataset for the verifier
// tests. It uses seed 42 so the fixture is reproducible.
func validFixture(t *testing.T) Dataset {
	t.Helper()
	ds, err := Generate(Config{Scale: Dev, Seed: 42})
	if err != nil {
		t.Fatalf("build valid fixture: %v", err)
	}
	// Sanity: the generator must produce at least one order to mutate.
	if len(ds.Orders) == 0 || len(ds.PaymentAttempts) == 0 {
		t.Fatalf("fixture too small: orders=%d attempts=%d",
			len(ds.Orders), len(ds.PaymentAttempts))
	}
	return ds
}
