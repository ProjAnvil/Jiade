package seed

import (
	"errors"
	"fmt"
)

// Verifier errors. Callers use errors.Is to classify violations.
var (
	// ErrMoneyMismatch signals a broken money equation: an order/item/tax
	// total that does not reconcile with its component fields.
	ErrMoneyMismatch = errors.New("seed: money mismatch")
	// ErrInvalidTransition signals a status combination forbidden by the
	// schema CHECK constraints or the payment/fulfilment state machines.
	ErrInvalidTransition = errors.New("seed: invalid status transition")
	// ErrOrphanedReference signals a foreign key that points at a missing
	// parent row (e.g. an order_item referencing an unknown order_id).
	ErrOrphanedReference = errors.New("seed: orphaned reference")
)

// Violation is a single integrity problem. The Verifier returns the first
// violation as the wrapped error; tests usually only need errors.Is.
type Violation struct {
	Code    error
	Message string
}

func (v Violation) Error() string { return fmt.Sprintf("%v: %s", v.Code, v.Message) }
func (v Violation) Unwrap() error { return v.Code }

// VerifyFixture runs every integrity check against an in-memory Dataset. The
// database-side CHECK constraints already enforce most of these at insert time;
// the verifier exists to catch generator bugs before they reach the database
// and to support hermetic unit tests (TestVerifyRejectsBrokenOrderEquation).
//
// The function returns the first violation encountered. Order of checks is
// stable so test failures point at the same root cause across runs.
func VerifyFixture(ds Dataset) error {
	for _, err := range []error{
		verifyOrderEquations(ds),
		verifyOrderStateMatrix(ds),
		verifyItemEquations(ds),
		verifyTaxLines(ds),
		verifyPaymentAttempts(ds),
		verifyRefunds(ds),
		verifyFulfillmentShipments(ds),
		verifyForeignKeys(ds),
	} {
		if err != nil {
			return err
		}
	}
	return nil
}

// verifyOrderEquations re-derives every sales_order.total_minor from its
// components. A single off-by-one must surface as ErrMoneyMismatch.
func verifyOrderEquations(ds Dataset) error {
	for _, order := range ds.Orders {
		want := order.SubtotalMinor - order.DiscountMinor + order.ShippingMinor + order.TaxMinor
		if order.TotalMinor != want {
			return Violation{
				Code: ErrMoneyMismatch,
				Message: fmt.Sprintf(
					"order %s total=%d but subtotal-discount+shipping+tax=%d",
					order.OrderID, order.TotalMinor, want),
			}
		}
		if order.DiscountMinor > order.SubtotalMinor {
			return Violation{
				Code: ErrMoneyMismatch,
				Message: fmt.Sprintf(
					"order %s discount=%d exceeds subtotal=%d",
					order.OrderID, order.DiscountMinor, order.SubtotalMinor),
			}
		}
		if order.SubtotalMinor < 0 || order.DiscountMinor < 0 ||
			order.ShippingMinor < 0 || order.TaxMinor < 0 || order.TotalMinor < 0 {
			return Violation{
				Code:    ErrMoneyMismatch,
				Message: fmt.Sprintf("order %s has a negative money field", order.OrderID),
			}
		}
	}
	return nil
}

// verifyOrderStateMatrix mirrors the CHECK constraints in order_db.sql. The
// matrix is small and explicit so the failure message names the offending row.
func verifyOrderStateMatrix(ds Dataset) error {
	for _, order := range ds.Orders {
		validStatus := map[string]bool{
			"pending": true, "confirmed": true, "cancelled": true, "completed": true,
		}
		validPayment := map[string]bool{
			"pending": true, "authorized": true, "paid": true,
			"failed": true, "partially_refunded": true, "refunded": true,
		}
		validFulfil := map[string]bool{
			"unfulfilled": true, "partial": true, "fulfilled": true,
		}
		if !validStatus[order.Status] {
			return Violation{ErrInvalidTransition, fmt.Sprintf("order %s status=%q", order.OrderID, order.Status)}
		}
		if !validPayment[order.PaymentStatus] {
			return Violation{ErrInvalidTransition, fmt.Sprintf("order %s payment_status=%q", order.OrderID, order.PaymentStatus)}
		}
		if !validFulfil[order.FulfillmentStatus] {
			return Violation{ErrInvalidTransition, fmt.Sprintf("order %s fulfillment_status=%q", order.OrderID, order.FulfillmentStatus)}
		}
		// completed requires paid-family payment + fulfilled shipping.
		if order.Status == "completed" && !(validPaidFamily(order.PaymentStatus) && order.FulfillmentStatus == "fulfilled") {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"order %s completed but payment=%s fulfillment=%s",
				order.OrderID, order.PaymentStatus, order.FulfillmentStatus)}
		}
		// failed payment requires cancelled status.
		if order.PaymentStatus == "failed" && order.Status != "cancelled" {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"order %s payment=failed but status=%s", order.OrderID, order.Status)}
		}
		// Non-unfulfilled requires confirmed/completed/cancelled + paid family.
		if order.FulfillmentStatus != "unfulfilled" {
			if !(order.Status == "confirmed" || order.Status == "completed" || order.Status == "cancelled") {
				return Violation{ErrInvalidTransition, fmt.Sprintf(
					"order %s fulfillment=%s but status=%s",
					order.OrderID, order.FulfillmentStatus, order.Status)}
			}
			if !validPaidFamily(order.PaymentStatus) {
				return Violation{ErrInvalidTransition, fmt.Sprintf(
					"order %s fulfillment=%s but payment=%s",
					order.OrderID, order.FulfillmentStatus, order.PaymentStatus)}
			}
		}
	}
	return nil
}

func validPaidFamily(payment string) bool {
	switch payment {
	case "paid", "partially_refunded", "refunded":
		return true
	}
	return false
}

// verifyItemEquations checks order_item.total = unit*quantity - discount and
// that discount does not exceed unit*quantity.
func verifyItemEquations(ds Dataset) error {
	for _, item := range ds.OrderItems {
		gross := item.UnitPriceMinor * int64(item.Quantity)
		if item.DiscountMinor > gross {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"item %s discount=%d exceeds gross=%d", item.OrderItemID, item.DiscountMinor, gross)}
		}
		if item.TotalMinor != gross-item.DiscountMinor {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"item %s total=%d but gross-discount=%d", item.OrderItemID, item.TotalMinor, gross-item.DiscountMinor)}
		}
		if item.Quantity <= 0 || item.UnitPriceMinor < 0 || item.TotalMinor < 0 {
			return Violation{ErrMoneyMismatch, fmt.Sprintf("item %s has invalid money/quantity", item.OrderItemID)}
		}
	}
	return nil
}

// verifyTaxLines checks every tax line's amount is rate * taxable / 10000,
// rounded down, matching order.RegionalTax.
func verifyTaxLines(ds Dataset) error {
	for _, line := range ds.OrderTaxLines {
		want := line.TaxableMinor * int64(line.RateBasisPoints) / 10000
		if line.AmountMinor != want {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"tax %s amount=%d but expected=%d", line.TaxLineID, line.AmountMinor, want)}
		}
	}
	return nil
}

// verifyPaymentAttempts enforces the trigger invariants from payment_db.sql:
// failed attempts need a failure_code, non-failed attempts must not have one,
// and attempt amount cannot exceed intent amount.
func verifyPaymentAttempts(ds Dataset) error {
	intentAmount := make(map[string]int64)
	intentStatus := make(map[string]string)
	for _, intent := range ds.PaymentIntents {
		intentAmount[intent.PaymentIntentID] = intent.AmountMinor
		intentStatus[intent.PaymentIntentID] = intent.Status
	}
	for _, attempt := range ds.PaymentAttempts {
		if attempt.AmountMinor > intentAmount[attempt.PaymentIntentID] {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"attempt %s amount=%d exceeds intent amount=%d",
				attempt.AttemptID, attempt.AmountMinor, intentAmount[attempt.PaymentIntentID])}
		}
		if attempt.Status == "failed" && attempt.FailureCode == nil {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"attempt %s failed without failure_code", attempt.AttemptID)}
		}
		if attempt.Status != "failed" && attempt.FailureCode != nil {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"attempt %s status=%s with failure_code=%s",
				attempt.AttemptID, attempt.Status, *attempt.FailureCode)}
		}
	}
	return nil
}

// verifyRefunds enforces: refund only against captured intents, cumulative
// refunds do not exceed intent amount, and refunded <= captured.
func verifyRefunds(ds Dataset) error {
	intentAmount := make(map[string]int64)
	intentStatus := make(map[string]string)
	for _, intent := range ds.PaymentIntents {
		intentAmount[intent.PaymentIntentID] = intent.AmountMinor
		intentStatus[intent.PaymentIntentID] = intent.Status
	}
	capturedByIntent := make(map[string]int64)
	for _, attempt := range ds.PaymentAttempts {
		if attempt.Status == "succeeded" {
			capturedByIntent[attempt.PaymentIntentID] += attempt.AmountMinor
		}
	}
	refundedByIntent := make(map[string]int64)
	for _, refund := range ds.Refunds {
		if refund.Status != "succeeded" && refund.Status != "pending" {
			continue
		}
		if intentStatus[refund.PaymentIntentID] != "succeeded" &&
			intentStatus[refund.PaymentIntentID] != "partially_refunded" &&
			intentStatus[refund.PaymentIntentID] != "refunded" {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"refund %s against non-captured intent %s",
				refund.RefundID, refund.PaymentIntentID)}
		}
		refundedByIntent[refund.PaymentIntentID] += refund.AmountMinor
	}
	for intent, refunded := range refundedByIntent {
		if refunded > intentAmount[intent] {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"intent %s refunded=%d exceeds amount=%d", intent, refunded, intentAmount[intent])}
		}
		if refunded > capturedByIntent[intent] {
			return Violation{ErrMoneyMismatch, fmt.Sprintf(
				"intent %s refunded=%d exceeds captured=%d",
				intent, refunded, capturedByIntent[intent])}
		}
	}
	return nil
}

// verifyFulfillmentShipments mirrors the shipment CHECK constraints: only
// label_created may have shipped_at=nil; delivered requires delivered_at;
// delivered_at >= shipped_at when both present.
func verifyFulfillmentShipments(ds Dataset) error {
	for _, shipment := range ds.Shipments {
		if shipment.Status != "label_created" && shipment.ShippedAt == nil {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"shipment %s status=%s but shipped_at is nil",
				shipment.ShipmentID, shipment.Status)}
		}
		if shipment.Status == "delivered" && shipment.DeliveredAt == nil {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"shipment %s delivered without delivered_at", shipment.ShipmentID)}
		}
		if shipment.Status != "delivered" && shipment.DeliveredAt != nil {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"shipment %s status=%s but delivered_at is set",
				shipment.ShipmentID, shipment.Status)}
		}
		if shipment.ShippedAt != nil && shipment.DeliveredAt != nil &&
			shipment.DeliveredAt.Before(*shipment.ShippedAt) {
			return Violation{ErrInvalidTransition, fmt.Sprintf(
				"shipment %s delivered_at before shipped_at", shipment.ShipmentID)}
		}
	}
	return nil
}

// verifyForeignKeys sanity-checks the cross-domain references the generator
// emits. It catches orphaned order_items, payments for unknown orders, etc.
func verifyForeignKeys(ds Dataset) error {
	orderIDs := make(map[string]bool)
	for _, order := range ds.Orders {
		orderIDs[order.OrderID] = true
	}
	for _, item := range ds.OrderItems {
		if !orderIDs[item.OrderID] {
			return Violation{ErrOrphanedReference, fmt.Sprintf(
				"order_item %s references unknown order %s", item.OrderItemID, item.OrderID)}
		}
	}
	for _, intent := range ds.PaymentIntents {
		if !orderIDs[intent.OrderID] {
			return Violation{ErrOrphanedReference, fmt.Sprintf(
				"payment_intent %s references unknown order %s", intent.PaymentIntentID, intent.OrderID)}
		}
	}
	for _, fulfil := range ds.Fulfillments {
		if !orderIDs[fulfil.OrderID] {
			return Violation{ErrOrphanedReference, fmt.Sprintf(
				"fulfillment %s references unknown order %s", fulfil.FulfillmentID, fulfil.OrderID)}
		}
	}
	for _, reservation := range ds.Reservations {
		if !orderIDs[reservation.OrderID] {
			return Violation{ErrOrphanedReference, fmt.Sprintf(
				"reservation %s references unknown order %s", reservation.ReservationID, reservation.OrderID)}
		}
	}
	return nil
}
