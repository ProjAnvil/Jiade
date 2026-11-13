package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestOrderQueueBindsPaymentFulfillmentAndRefundResults(t *testing.T) {
	got := orderEventBindings()
	want := []string{
		"payment.failed.v1", "payment.captured.v1", "payment.succeeded.v1",
		"payment.paid.v1", "payment.refunded.v1", "refund.succeeded.v1",
		"fulfillment.completed.v1", "fulfillment.delivered.v1",
	}
	for _, binding := range want {
		if !slices.Contains(got, binding) {
			t.Fatalf("bindings=%v, missing %q", got, binding)
		}
	}
	for _, binding := range got {
		if binding == "payment.*" || binding == "fulfillment.*" || binding == "refund.*" {
			t.Fatalf("binding %q consumes request events produced by order", binding)
		}
	}
}

func TestOrderTopologyHasDelayedRetryAndDistinctDeadLetterRoutes(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, fragment := range []string{
		`"x-message-ttl"`,
		`"x-dead-letter-routing-key": retryReturnRoute`,
		`RetryExchange: retryExchange`,
		`DeadExchange: deadExchange`,
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("main.go missing retry/DLQ topology %q", fragment)
		}
	}
}
