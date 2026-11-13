// Package messaging exposes the bank broker topology constants and the
// routing-key → exchange mapping the OutboxRelay uses to publish saga
// messages. The definitions.json file in deploy/rabbitmq declares the same
// exchanges and bindings; these constants keep the Go wiring in sync.
package messaging

import "strings"

// Bank broker exchanges. definitions.json declares these as durable exchanges:
// bank.commands/bank.events are topic exchanges that route saga messages, while
// bank.retry/bank.dlx are direct exchanges that back the retry-with-dead-letter
// -back and terminal-DLQ lanes.
const (
	ExchangeCommands   = "bank.commands"
	ExchangeEvents     = "bank.events"
	ExchangeRetry      = "bank.retry"
	ExchangeDeadLetter = "bank.dlx"
)

// ExchangeForRoutingKey maps a bank outbox routing key to the topic exchange
// whose bindings deliver it.
//
// Conventions encoded here are load-bearing and match the bindings declared in
// deploy/rabbitmq/definitions.json:
//
//   - Command routing keys are versioned (*.v1, *.v2, …) and route to
//     bank.commands, e.g. risk.authorize-payment.v1, core.place-hold.v1,
//     core.reverse-transfer.v1.
//   - Result event routing keys are unversioned dot keys and route to
//     bank.events, e.g. risk.payment.authorized, core.hold.placed,
//     core.transfer.failed, payment.completed, and the *.command.rejected
//     failure events.
//
// Every saga command routing key today ends in ".v<digits>", while no result
// event does, so the version-suffix check is the single deterministic signal.
// The function returns bank.events for any key that does not match the command
// convention so unknown future events still reach the events exchange (whose
// wildcard bindings are broader than the commands exchange's).
func ExchangeForRoutingKey(routingKey string) string {
	if isVersionedRoutingKey(routingKey) {
		return ExchangeCommands
	}
	return ExchangeEvents
}

// isVersionedRoutingKey reports whether routingKey ends in a ".v<digits>"
// suffix, the convention that distinguishes versioned saga commands from
// unversioned result events.
func isVersionedRoutingKey(routingKey string) bool {
	dot := strings.LastIndexByte(routingKey, '.')
	if dot < 0 || dot == len(routingKey)-1 {
		return false
	}
	rest := routingKey[dot+1:]
	if len(rest) < 2 || rest[0] != 'v' {
		return false
	}
	for _, r := range rest[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
