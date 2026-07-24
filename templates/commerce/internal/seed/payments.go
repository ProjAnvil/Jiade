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
	case "refunded":
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          "succeeded",
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
		r := RefundRow{
			RefundID:        fmt.Sprintf("%s-rf1", intentID),
			PaymentIntentID: intentID,
			AmountMinor:     intent.AmountMinor,
			Status:          "succeeded",
			Reason:          pick(stream, refundReasons),
			IdempotencyKey:  fmt.Sprintf("refund-%s", intentID),
			CreatedAt:       created.Add(48 * time.Hour),
		}
		refund = &r
	case "partially_refunded":
		attempts = append(attempts, PaymentAttemptRow{
			AttemptID:       fmt.Sprintf("%s-a1", intentID),
			PaymentIntentID: intentID,
			Status:          "succeeded",
			AmountMinor:     intent.AmountMinor,
			CreatedAt:       created.Add(time.Second),
		})
		refundAmount := intent.AmountMinor / 2
		if refundAmount <= 0 {
			refundAmount = 1
		}
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

// paymentIntentStatus maps an order's payment_status onto a payment_intent
// status. They are intentionally the same vocabulary except for orders that
// are still pending (no intent captured yet).
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
	case "partially_refunded":
		return "partially_refunded"
	case "refunded":
		return "refunded"
	default:
		return "requires_method"
	}
}
