package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestPaymentQueueBindsOrderLifecycleEvents(t *testing.T) {
	got := paymentEventBindings()
	want := []string{
		"order.placed.v1",
		"order.cancelled.v1",
		"payment.refund-requested.v1",
	}
	if len(got) != len(want) {
		t.Fatalf("bindings=%v, want exactly %v", got, want)
	}
	for _, binding := range want {
		if !slices.Contains(got, binding) {
			t.Fatalf("bindings=%v, missing %q", got, binding)
		}
	}
}

func TestPaymentMainSupervisesRelayConsumerAndMapsHTTPTimeouts(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, contract := range []string{
		"payment.NewWorkerLifecycle",
		"payment.NewRuntimeReadinessWithDependencies",
		"payment.CombinePublisherAvailability",
		"relay.Done()",
		"relay.Wait(shutdownContext)",
		"consumerWorker.Done()",
		"consumerWorker.Wait(shutdownContext)",
		"defer publisher.Close()",
		`"x-message-ttl":             int32(2000)`,
		`"x-dead-letter-routing-key": retryReturnRoute`,
		`RetryExchange: retryExchange`,
		`DeadExchange: deadExchange`,
		"ReadHeaderTimeout: settings.HTTP.ReadHeaderTimeout",
		"ReadTimeout:       settings.HTTP.ReadTimeout",
		"WriteTimeout:      settings.HTTP.WriteTimeout",
		"IdleTimeout:       settings.HTTP.IdleTimeout",
	} {
		if !strings.Contains(text, contract) {
			t.Errorf("payment main.go missing contract %q", contract)
		}
	}
}
