package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"bank/internal/platform/messaging"
)

// fakeDefinition is a minimal Definition implementation sufficient for
// exercising Registry validation in this package's tests.
type fakeDefinition struct {
	workflowType string
	version      int
	actions      []Action
}

func (d fakeDefinition) Type() string { return d.workflowType }
func (d fakeDefinition) Version() int { return d.version }
func (d fakeDefinition) Prepare(context.Context, json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (d fakeDefinition) Actions() []Action { return d.actions }

// fakeAction is a minimal Action implementation used only for registry tests.
type fakeAction struct {
	name string
}

func (a fakeAction) Name() string { return a.name }
func (a fakeAction) Execute(context.Context, View) (Dispatch, error) {
	return Dispatch{}, nil
}
func (a fakeAction) ApplyResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	return Outcome{}, nil
}
func (a fakeAction) Compensate(context.Context, View) (Dispatch, error) {
	return Dispatch{}, nil
}
func (a fakeAction) ApplyCompensationResult(context.Context, View, messaging.Envelope) (Outcome, error) {
	return Outcome{}, nil
}

func TestRegistryRegisterAndGetSucceeds(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{workflowType: "payment-transfer", version: 1}
	if err := registry.Register(definition); err != nil {
		t.Fatalf("register: error=%v", err)
	}

	got, ok := registry.Get("payment-transfer", 1)
	if !ok {
		t.Fatal("Get returned ok=false for registered definition")
	}
	if got.Type() != "payment-transfer" || got.Version() != 1 {
		t.Fatalf("Get returned Type=%q Version=%d, want payment-transfer/1", got.Type(), got.Version())
	}

	if _, ok := registry.Get("payment-transfer", 2); ok {
		t.Fatal("Get returned ok=true for unregistered version")
	}
	if _, ok := registry.Get("other-type", 1); ok {
		t.Fatal("Get returned ok=true for unregistered type")
	}
}

func TestRegistryRejectsDuplicateVersion(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{workflowType: "payment-transfer", version: 1}
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(definition); !errors.Is(err, ErrDuplicateDefinition) {
		t.Fatalf("error=%v", err)
	}
}

func TestRegistryRejectsEmptyWorkflowType(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{workflowType: "", version: 1}
	if err := registry.Register(definition); !errors.Is(err, ErrEmptyWorkflowType) {
		t.Fatalf("error=%v", err)
	}
}

func TestRegistryRejectsVersionBelowOne(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{workflowType: "payment-transfer", version: 0}
	if err := registry.Register(definition); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("error=%v", err)
	}
}

func TestRegistryRejectsEmptyActionName(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{
		workflowType: "payment-transfer",
		version:      1,
		actions: []Action{
			fakeAction{name: "book-transfer"},
			fakeAction{name: ""},
		},
	}
	if err := registry.Register(definition); !errors.Is(err, ErrEmptyActionName) {
		t.Fatalf("error=%v", err)
	}
}

func TestRegistryRejectsDuplicateActionName(t *testing.T) {
	registry := NewRegistry()
	definition := fakeDefinition{
		workflowType: "payment-transfer",
		version:      1,
		actions: []Action{
			fakeAction{name: "book-transfer"},
			fakeAction{name: "book-transfer"},
		},
	}
	if err := registry.Register(definition); !errors.Is(err, ErrDuplicateActionName) {
		t.Fatalf("error=%v", err)
	}
}
