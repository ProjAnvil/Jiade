package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestFulfillmentQueueBindsOrderLifecycleEvents(t *testing.T) {
	got := fulfillmentEventBindings()
	want := []string{
		"order.paid.v1",
		"order.cancelled.v1",
		"fulfillment.cancel-requested.v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("bindings=%v, want exactly %v (order owns the lifecycle contract)", got, want)
	}
}

func TestFulfillmentEnvironmentFallsBackWhenUnset(t *testing.T) {
	const name = "FULFILLMENT_TEST_ENV_VAR"
	t.Setenv(name, "")
	if got := environment(name, "fallback"); got != "fallback" {
		t.Fatalf("environment=%q, want fallback", got)
	}
	t.Setenv(name, "  override  ")
	if got := environment(name, "fallback"); got != "override" {
		t.Fatalf("environment=%q, want trimmed override", got)
	}
	if got := environment(name, "fallback"); got != "override" {
		t.Fatalf("environment=%q, want override to stick", got)
	}
	os.Unsetenv(name)
	if got := environment(name, "fallback"); got != "fallback" {
		t.Fatalf("environment=%q, want fallback after unset", got)
	}
}

func TestFulfillmentTopologyHasDelayedRetryAndDistinctDeadLetterRoutes(t *testing.T) {
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
