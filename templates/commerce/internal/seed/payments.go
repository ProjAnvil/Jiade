package seed

import (
	"fmt"
	"math/rand"
	"time"
)

// generatePayments builds one payment intent per order plus the attempts and
// refunds that justify each intent's status. The state machine in
// internal/payment/state.go is the source of truth for valid statuses; we emit
// final statuses directly (the seed does not replay events) but every emitted
// status is in the schema's CHECK list.
func generatePayments(ds *Dataset, c counts, seed int64, generatedAt time.Time) error {
	stream := domainStream(seed, "payments")

	ds.PaymentIntents = make([]PaymentIntentRow, 0, len(ds.Orders))
	ds.PaymentAttempts = make([]PaymentAttemptRow, 0, len(ds.Orders)*2)
	ds.Refunds = make([]RefundRow, 0, len(ds.Orders)/5)

	for _, order := range ds.Orders {
		intent, attempts, refund, err := generateOnePayment(stream, order, generatedAt)
		if err != nil {
			return err
		}
		ds.PaymentIntents = append(ds.PaymentIntents, intent)
		ds.PaymentAttempts = append(ds.PaymentAttempts, attempts...)
		if refund != nil {
			ds.Refunds = append(ds.Refunds, *refund)
		}
	}
	return nil
}

// generateOnePayment is the canonical per-order payment generator. It returns
// the intent, its attempts, and an optional refund. Both the batch
// (generatePayments) and streaming (generatePaymentsStreaming) paths share it
// so payment state stays consistent.
//
// Load-order invariant: the seed writes a captured intent (status
// "succeeded" or "partially_refunded"), then a succeeded attempt, then the
// refund. The PostgreSQL validate_refund trigger accepts a refund row only
// while the intent status is "succeeded" or "partially_refunded"; the seed
// never emits a terminal "refunded" intent status because the refund is
// written after the intent, and COPY/INSERT do not replay the post-refund
// transition. A fully-refunded order therefore maps to the captured intent
// status "partially_refunded"; the refund amount (full vs. half) preserves
// the full/partial distinction in the data.
func generateOnePayment(stream *rand.Rand, order SalesOrderRow, generatedAt time.Time) (PaymentIntentRow, []PaymentAttemptRow, *RefundRow, error) {
	intentID := fmt.Sprintf("%s-pi", order.OrderID)
	provider := pick(stream, paymentProviders)
	providerRef := fmt.Sprintf("ref-%s", intentID)
	created := order.PlacedAt.Add(2 * time.Second)
	intent := PaymentIntentRow{
		PaymentIntentID:   intentID,
		OrderID:           order.OrderID,
		AmountMinor:       order.TotalMinor,
		Currency:          order.Currency,
		Status:            paymentIntentStatus(order.PaymentStatus),
		Provider:          provider.Name,
		ProviderReference: &providerRef,
		IdempotencyKey:    fmt.Sprintf("pay-%s", order.OrderID),
		CreatedAt:         created,
	}
	var attempts []PaymentAttemptRow
	var refund *RefundRow

	switch intent.Status {
	case "requires_method":
		// No attempts yet.
	case "failed":
		fc := pick(stream, failureReasons)
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          "failed",
			FailureCode:     &fc,
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
	case "succeeded":
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          "succeeded",
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
	case "partially_refunded":
		// Captured intent with a refund. The intent status stays at
		// "partially_refunded" (the trigger-safe captured state) so the
		// validate_refund trigger accepts the refund row written next. Both
		// fully- and partially-refunded orders land here; the refund amount
		// distinguishes them (full vs. half).
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          "succeeded",
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
		refundAmount := refundAmountFor(order.PaymentStatus, intent.AmountMinor)
		r := RefundRow{
			RefundID:        fmt.Sprintf("%s-rf1", intentID),
			PaymentIntentID: intentID,
			AmountMinor:     refundAmount,
			Status:          "succeeded",
			Reason:          pick(stream, refundReasons),
			IdempotencyKey:  fmt.Sprintf("refund-%s", intentID),
			CreatedAt:       created.Add(48 * time.Hour),
		}
		refund = &r
	default:
		attemptStatus := map[string]string{
			"authorized": "authorized",
			"processing": "processing",
			"cancelled":  "cancelled",
		}[intent.Status]
		if attemptStatus == "" {
			attemptStatus = "processing"
		}
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          attemptStatus,
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
	}
	return intent, attempts, refund, nil
}

// refundAmountFor returns the refund amount for a captured intent given the
// order's payment_status. A fully-refunded order refunds the full intent
// amount; a partially-refunded order refunds half (minimum 1 minor unit).
func refundAmountFor(orderPaymentStatus string, intentAmount int64) int64 {
	if orderPaymentStatus == "refunded" {
		return intentAmount
	}
	refundAmount := intentAmount / 2
	if refundAmount <= 0 {
		refundAmount = 1
	}
	return refundAmount
}

// paymentIntentStatus maps an order's payment_status onto the payment_intent
// status the seed writes. The mapping is intentionally conservative: every
// refund-family order status maps to the captured intent status
// "partially_refunded" rather than "refunded", because the seed writes the
// intent in its final state before the refund row, and the PostgreSQL
// validate_refund trigger accepts refunds only while the intent status is
// "succeeded" or "partially_refunded". "partially_refunded" is the captured
// state that remains valid after the refund is applied, so it is the safe
// terminal status for any refunded intent.
func paymentIntentStatus(orderPaymentStatus string) string {
	switch orderPaymentStatus {
	case "pending":
		return "requires_method"
	case "authorized":
		return "authorized"
	case "paid":
		return "succeeded"
	case "failed":
		return "failed"
	case "partially_refunded", "refunded":
		return "partially_refunded"
	default:
		return "requires_method"
	}
}
