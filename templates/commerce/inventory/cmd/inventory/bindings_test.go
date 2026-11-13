package main

import (
	"slices"
	"testing"
)

func TestInventoryQueueBindsSagaTransitionRequests(t *testing.T) {
	got := inventoryEventBindings()
	want := []string{
		"inventory.commit-requested.v1",
		"inventory.release-requested.v1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("bindings=%v, want exactly %v (order owns the saga request contract)", got, want)
	}
}
