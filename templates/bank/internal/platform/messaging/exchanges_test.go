package messaging

import "testing"

func TestExchangeForRoutingKey(t *testing.T) {
	cases := map[string]string{
		// Versioned saga commands → bank.commands.
		"risk.authorize-payment.v1":            ExchangeCommands,
		"risk.void-payment-authorization.v1":   ExchangeCommands,
		"core.place-hold.v1":                   ExchangeCommands,
		"core.release-hold.v1":                 ExchangeCommands,
		"core.post-held-transfer.v1":           ExchangeCommands,
		"core.reverse-transfer.v1":             ExchangeCommands,
		"risk.authorize-payment.v2":            ExchangeCommands,
		"core.post-held-transfer.v10":          ExchangeCommands,
		// Unversioned result events → bank.events.
		"risk.payment.authorized":          ExchangeEvents,
		"risk.payment.rejected":            ExchangeEvents,
		"risk.payment.authorization.voided": ExchangeEvents,
		"risk.command.rejected":            ExchangeEvents,
		"core.hold.placed":                 ExchangeEvents,
		"core.hold.failed":                 ExchangeEvents,
		"core.hold.released":               ExchangeEvents,
		"core.hold.release_failed":         ExchangeEvents,
		"core.transfer.posted":             ExchangeEvents,
		"core.transfer.failed":             ExchangeEvents,
		"core.transfer.reversed":           ExchangeEvents,
		"core.transfer.reverse_failed":     ExchangeEvents,
		"core.command.rejected":            ExchangeEvents,
		"payment.completed":                ExchangeEvents,
	}
	for routingKey, want := range cases {
		got := ExchangeForRoutingKey(routingKey)
		if got != want {
			t.Errorf("ExchangeForRoutingKey(%q) = %q, want %q", routingKey, got, want)
		}
	}
}

func TestIsVersionedRoutingKey(t *testing.T) {
	cases := map[string]bool{
		"risk.authorize-payment.v1": true,
		"core.v2":                   true,
		"a.b.c.v99":                 true,
		"payment.completed":         false,
		"core.hold.placed":          false,
		"v1":                        false,
		"core.v":                    false,
		"core.vx":                   false,
		"core.v1x":                  false,
		"":                          false,
		"core":                      false,
		"core.1":                    false,
	}
	for routingKey, want := range cases {
		got := isVersionedRoutingKey(routingKey)
		if got != want {
			t.Errorf("isVersionedRoutingKey(%q) = %v, want %v", routingKey, got, want)
		}
	}
}
