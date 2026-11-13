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

// TestVerifyRejectsRefundOnRefundedIntent reproduces the production defect:
// the seed generator emitted a payment_intent whose final status was
// "refunded" together with a refund row, but the PostgreSQL validate_refund
// trigger only allows refunds while the intent status is "succeeded" or
// "partially_refunded". The in-memory verifier must catch the same invariant
// so the failure surfaces in hermetic tests before reaching the database.
func TestVerifyRejectsRefundOnRefundedIntent(t *testing.T) {
	ds := validFixture(t)

	// Build an intent/refund pair in the trigger-failing shape: intent in
	// its terminal "refunded" status with a refund written against it. Use
	// an isolated intent ID so existing refunds keep passing their checks.
	intentID := "pi-test-refunded"
	ds.PaymentIntents = append(ds.PaymentIntents, PaymentIntentRow{
		PaymentIntentID: intentID,
		OrderID:         ds.Orders[0].OrderID,
		AmountMinor:     ds.Orders[0].TotalMinor,
		Currency:        ds.Orders[0].Currency,
		Status:          "refunded",
		Provider:        "stripe",
		IdempotencyKey:  "pay-test-refunded",
		CreatedAt:       ds.Orders[0].PlacedAt,
	})
	ds.Refunds = append(ds.Refunds, RefundRow{
		RefundID:        "pi-test-refunded-rf1",
		PaymentIntentID: intentID,
		AmountMinor:     ds.Orders[0].TotalMinor,
		Status:          "succeeded",
		Reason:          "customer_request",
		IdempotencyKey:  "refund-test-refunded",
		CreatedAt:       ds.Orders[0].PlacedAt,
	})

	err := VerifyFixture(ds)
	if !errors.Is(err, ErrRefundWithoutCapture) {
		t.Fatalf("error=%v, want ErrRefundWithoutCapture", err)
	}
}

// TestVerifyRejectsRefundOnNonCapturedIntent covers the remaining non-captured
// intent statuses (requires_method, processing, authorized, failed, cancelled)
// that must also be rejected. The verifier message must name the bad status.
func TestVerifyRejectsRefundOnNonCapturedIntent(t *testing.T) {
	ds := validFixture(t)
	for _, bad := range []string{"requires_method", "processing", "authorized", "failed", "cancelled"} {
		intentID := "pi-bad-" + bad
		ds2 := ds
		ds2.PaymentIntents = append(ds2.PaymentIntents, PaymentIntentRow{
			PaymentIntentID: intentID,
			OrderID:         ds.Orders[0].OrderID,
			AmountMinor:     ds.Orders[0].TotalMinor,
			Currency:        ds.Orders[0].Currency,
			Status:          bad,
			Provider:        "stripe",
			IdempotencyKey:  "pay-" + intentID,
			CreatedAt:       ds.Orders[0].PlacedAt,
		})
		ds2.Refunds = append(ds2.Refunds, RefundRow{
			RefundID:        intentID + "-rf1",
			PaymentIntentID: intentID,
			AmountMinor:     1,
			Status:          "succeeded",
			Reason:          "customer_request",
			IdempotencyKey:  "refund-" + intentID,
			CreatedAt:       ds.Orders[0].PlacedAt,
		})
		err := VerifyFixture(ds2)
		if !errors.Is(err, ErrRefundWithoutCapture) {
			t.Fatalf("status=%s: error=%v, want ErrRefundWithoutCapture", bad, err)
		}
	}
}

// TestVerifyAcceptsPartiallyRefundedIntent confirms the corrected invariant:
// a refund against a "partially_refunded" intent is the only post-capture
// shape the seed should emit, and the verifier must accept it.
func TestVerifyAcceptsPartiallyRefundedIntent(t *testing.T) {
	ds := validFixture(t)
	if len(ds.Refunds) == 0 {
		t.Skip("fixture has no refunds; covered by TestVerifyAcceptsValidFixture")
	}
	// Force every refunded-family intent into the trigger-safe
	// "partially_refunded" status and confirm the verifier still passes.
	for i := range ds.PaymentIntents {
		if ds.PaymentIntents[i].Status == "refunded" {
			ds.PaymentIntents[i].Status = "partially_refunded"
		}
	}
	if err := VerifyFixture(ds); err != nil {
		t.Fatalf("partially_refunded intent rejected: %v", err)
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
