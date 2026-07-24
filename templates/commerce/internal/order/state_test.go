package order

import (
	"errors"
	"testing"
)

func TestOrderInvariantTransitionsFollowLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		from  State
		event Event
		want  State
	}{
		{name: "checkout", from: StateNew, event: EventCheckoutStarted, want: StatePending},
		{name: "payment succeeds", from: StatePending, event: EventPaymentSucceeded, want: StateConfirmed},
		{name: "payment fails", from: StatePending, event: EventPaymentFailed, want: StateCancelled},
		{name: "cancel pending", from: StatePending, event: EventCancellationRequested, want: StateCancelled},
		{name: "cancel confirmed", from: StateConfirmed, event: EventCancellationRequested, want: StateCancelled},
		{name: "fulfill", from: StateConfirmed, event: EventFulfillmentCompleted, want: StateCompleted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Transition(test.from, test.event)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("state=%q, want %q", got, test.want)
			}
		})
	}
}

func TestOrderInvariantTransitionsAreIdempotentForDuplicateEvents(t *testing.T) {
	tests := []struct {
		state State
		event Event
	}{
		{state: StatePending, event: EventCheckoutStarted},
		{state: StateConfirmed, event: EventPaymentSucceeded},
		{state: StateCancelled, event: EventPaymentFailed},
		{state: StateCancelled, event: EventCancellationRequested},
		{state: StateCompleted, event: EventFulfillmentCompleted},
	}
	for _, test := range tests {
		got, err := Transition(test.state, test.event)
		if err != nil {
			t.Fatalf("Transition(%q, %q): %v", test.state, test.event, err)
		}
		if got != test.state {
			t.Fatalf("Transition(%q, %q)=%q", test.state, test.event, got)
		}
	}
}

func TestOrderInvariantConfirmedOrderCannotTransitionBackToPending(t *testing.T) {
	if _, err := Transition(StateConfirmed, EventCheckoutStarted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error=%v, want ErrInvalidTransition", err)
	}
}

func TestOrderInvariantRejectsUnknownStateAndEvent(t *testing.T) {
	if _, err := Transition(State("unknown"), EventCheckoutStarted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown state error=%v", err)
	}
	if _, err := Transition(StatePending, Event("unknown")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unknown event error=%v", err)
	}
}
