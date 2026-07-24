// Package payment contains pure payment-intent lifecycle rules.
package payment

import (
	"errors"
	"fmt"
)

var ErrInvalidTransition = errors.New("invalid payment transition")

type State string

const (
	StateRequiresMethod    State = "requires_method"
	StateProcessing        State = "processing"
	StateAuthorized        State = "authorized"
	StateSucceeded         State = "succeeded"
	StateFailed            State = "failed"
	StateCancelled         State = "cancelled"
	StatePartiallyRefunded State = "partially_refunded"
	StateRefunded          State = "refunded"
)

type Event string

const (
	EventMethodAttached Event = "method_attached"
	EventAuthorize      Event = "authorize"
	EventCapture        Event = "capture"
	EventFail           Event = "fail"
	EventCancel         Event = "cancel"
	EventPartialRefund  Event = "partial_refund"
	EventRefund         Event = "refund"
)

// Transition advances an intent without permitting late or duplicate events
// to move it backward. Re-delivery of the establishing event is idempotent.
func Transition(from State, event Event) (State, error) {
	if duplicatePaymentEvent(from, event) {
		return from, nil
	}
	switch {
	case from == StateRequiresMethod && event == EventMethodAttached:
		return StateProcessing, nil
	case from == StateProcessing && event == EventAuthorize:
		return StateAuthorized, nil
	case from == StateAuthorized && event == EventCapture:
		return StateSucceeded, nil
	case from == StateProcessing && event == EventFail:
		return StateFailed, nil
	case (from == StateRequiresMethod || from == StateProcessing || from == StateAuthorized) && event == EventCancel:
		return StateCancelled, nil
	case from == StateSucceeded && event == EventPartialRefund:
		return StatePartiallyRefunded, nil
	case (from == StateSucceeded || from == StatePartiallyRefunded) && event == EventRefund:
		return StateRefunded, nil
	default:
		return from, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, from, event)
	}
}

func duplicatePaymentEvent(state State, event Event) bool {
	switch event {
	case EventMethodAttached:
		return state == StateProcessing
	case EventAuthorize:
		return state == StateAuthorized
	case EventCapture:
		return state == StateSucceeded || state == StatePartiallyRefunded || state == StateRefunded
	case EventFail:
		return state == StateFailed
	case EventCancel:
		return state == StateCancelled
	case EventPartialRefund:
		return state == StatePartiallyRefunded || state == StateRefunded
	case EventRefund:
		return state == StateRefunded
	default:
		return false
	}
}
