package payment

import (
	"errors"
	"testing"
)

func TestPaymentInvariantTransitionsFollowLifecycle(t *testing.T) {
	tests := []struct {
		from  State
		event Event
		want  State
	}{
		{from: StateRequiresMethod, event: EventMethodAttached, want: StateProcessing},
		{from: StateProcessing, event: EventAuthorize, want: StateAuthorized},
		{from: StateAuthorized, event: EventCapture, want: StateSucceeded},
		{from: StateProcessing, event: EventFail, want: StateFailed},
		{from: StateRequiresMethod, event: EventCancel, want: StateCancelled},
		{from: StateAuthorized, event: EventCancel, want: StateCancelled},
		{from: StateSucceeded, event: EventPartialRefund, want: StatePartiallyRefunded},
		{from: StateSucceeded, event: EventRefund, want: StateRefunded},
		{from: StatePartiallyRefunded, event: EventRefund, want: StateRefunded},
	}
	for _, test := range tests {
		got, err := Transition(test.from, test.event)
		if err != nil {
			t.Fatalf("Transition(%q, %q): %v", test.from, test.event, err)
		}
		if got != test.want {
			t.Fatalf("state=%q, want %q", got, test.want)
		}
	}
}

func TestPaymentInvariantTransitionsAreIdempotentForDuplicateEvents(t *testing.T) {
	tests := []struct {
		state State
		event Event
	}{
		{state: StateProcessing, event: EventMethodAttached},
		{state: StateAuthorized, event: EventAuthorize},
		{state: StateSucceeded, event: EventCapture},
		{state: StateFailed, event: EventFail},
		{state: StateCancelled, event: EventCancel},
		{state: StatePartiallyRefunded, event: EventPartialRefund},
		{state: StateRefunded, event: EventRefund},
	}
	for _, test := range tests {
		got, err := Transition(test.state, test.event)
		if err != nil {
			t.Fatalf("Transition(%q, %q): %v", test.state, test.event, err)
		}
		if got != test.state {
			t.Fatalf("state=%q, want unchanged %q", got, test.state)
		}
	}
}

func TestPaymentInvariantRejectsBackwardAndInvalidTransitions(t *testing.T) {
	tests := []struct {
		from  State
		event Event
	}{
		{from: StateFailed, event: EventCapture},
		{from: StateCancelled, event: EventAuthorize},
		{from: StateProcessing, event: EventRefund},
		{from: State("unknown"), event: EventFail},
		{from: StateProcessing, event: Event("unknown")},
	}
	for _, test := range tests {
		if _, err := Transition(test.from, test.event); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("Transition(%q, %q) error=%v", test.from, test.event, err)
		}
	}
}

func TestPaymentInvariantRefundedIgnoresLateCaptureAndRefundEvents(t *testing.T) {
	for _, event := range []Event{EventCapture, EventPartialRefund, EventRefund} {
		got, err := Transition(StateRefunded, event)
		if err != nil {
			t.Fatalf("event=%q error=%v", event, err)
		}
		if got != StateRefunded {
			t.Fatalf("event=%q state=%q, want refunded", event, got)
		}
	}
}

func TestPaymentInvariantPartiallyRefundedIgnoresLateCapture(t *testing.T) {
	got, err := Transition(StatePartiallyRefunded, EventCapture)
	if err != nil {
		t.Fatal(err)
	}
	if got != StatePartiallyRefunded {
		t.Fatalf("state=%q, want partially_refunded", got)
	}
}
