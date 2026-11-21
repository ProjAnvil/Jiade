package keeper

import (
	"reflect"
	"testing"
)

func TestParseSetsSpec(t *testing.T) {
	sets, err := ParseSetsSpec("dcn01:23301:dcn01-db:3306,dcn01-db-replica:3306")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []SetState{{
		Name: "dcn01", Port: 23301,
		Nodes:   []string{"dcn01-db:3306", "dcn01-db-replica:3306"},
		Primary: "dcn01-db:3306",
	}}
	if !reflect.DeepEqual(sets, want) {
		t.Fatalf("got %+v want %+v", sets, want)
	}
}

func TestParseSetsSpecRejectsGarbage(t *testing.T) {
	if _, err := ParseSetsSpec("no-ports-here"); err == nil {
		t.Fatal("expected error for spec without port field")
	}
	if _, err := ParseSetsSpec("dcn01:notaport:a,b"); err == nil {
		t.Fatal("expected error for non-numeric port")
	}
}

func TestPendingSwitch(t *testing.T) {
	events := []Event{
		{Type: "init"},
		{Type: "switch", Set: "dcn01", From: "a", To: "b"},
	}
	if got := pendingSwitch(events, "dcn01"); got != "b" {
		t.Fatalf("pending = %q, want b", got)
	}
	events = append(events, Event{Type: "switch-done", Set: "dcn01", From: "a", To: "b"})
	if got := pendingSwitch(events, "dcn01"); got != "" {
		t.Fatalf("pending after done = %q, want empty", got)
	}
	if got := pendingSwitch(events, "dcn99"); got != "" {
		t.Fatalf("unknown set pending = %q, want empty", got)
	}
	old, new := pendingFromTo(events, "dcn01")
	if old != "a" || new != "b" {
		t.Fatalf("from/to = %q/%q", old, new)
	}
}
