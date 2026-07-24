package order

import (
	"errors"
	"fmt"
)

var ErrInvalidTransition = errors.New("invalid order transition")

type State string

const (
	StateNew       State = "new"
	StatePending   State = "pending"
	StateConfirmed State = "confirmed"
	StateCancelled State = "cancelled"
	StateCompleted State = "completed"
)

type Event string

const (
	EventCheckoutStarted       Event = "checkout_started"
	EventPaymentSucceeded      Event = "payment_succeeded"
	EventPaymentFailed         Event = "payment_failed"
	EventCancellationRequested Event = "cancellation_requested"
	EventFulfillmentCompleted  Event = "fulfillment_completed"
)

// Transition applies a conditional order lifecycle event. Re-delivery of the
// event that established the current state is idempotent.
func Transition(from State, event Event) (State, error) {
	if duplicateOrderEvent(from, event) {
		return from, nil
	}
	switch {
	case from == StateNew && event == EventCheckoutStarted:
		return StatePending, nil
	case from == StatePending && event == EventPaymentSucceeded:
		return StateConfirmed, nil
	case from == StatePending && event == EventPaymentFailed:
		return StateCancelled, nil
	case (from == StatePending || from == StateConfirmed) && event == EventCancellationRequested:
		return StateCancelled, nil
	case from == StateConfirmed && event == EventFulfillmentCompleted:
		return StateCompleted, nil
	default:
		return from, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, from, event)
	}
}

func duplicateOrderEvent(state State, event Event) bool {
	switch event {
	case EventCheckoutStarted:
		return state == StatePending
	case EventPaymentSucceeded:
		return state == StateConfirmed || state == StateCompleted
	case EventPaymentFailed, EventCancellationRequested:
		return state == StateCancelled
	case EventFulfillmentCompleted:
		return state == StateCompleted
	default:
		return false
	}
}
