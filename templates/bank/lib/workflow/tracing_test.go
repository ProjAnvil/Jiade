package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// spanSnapshot is a minimal view over a recorded span for assertions.
type spanSnapshot struct {
	name       string
	attributes map[string]string
}

func snapshotSpans(exporter *tracetest.InMemoryExporter) []spanSnapshot {
	stubs := exporter.GetSpans()
	out := make([]spanSnapshot, 0, len(stubs))
	for _, stub := range stubs {
		attrs := make(map[string]string, len(stub.Attributes))
		for _, kv := range stub.Attributes {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		out = append(out, spanSnapshot{name: stub.Name, attributes: attrs})
	}
	return out
}

// findSpan returns the first recorded span with the given name, or false.
func findSpan(spans []spanSnapshot, name string) (spanSnapshot, bool) {
	for _, span := range spans {
		if span.name == name {
			return span, true
		}
	}
	return spanSnapshot{}, false
}

// installTraceRecorder wires an in-memory exporter as the global tracer
// provider so the engine's otel.Tracer(...) calls land in the recorder. It
// returns the exporter (for span assertions) and a shutdown func.
func installTraceRecorder(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSyncer(exporter),
	)
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return exporter
}

// TestWorkflowPrepareEmitsPrepareSpan drives a workflow through Start → Prepare
// and asserts that a span named exactly "workflow.prepare" was recorded with
// the required workflow.id and workflow.type attributes.
func TestWorkflowPrepareEmitsPrepareSpan(t *testing.T) {
	exporter := installTraceRecorder(t)

	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-trace-1", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	spans := snapshotSpans(exporter)
	prepare, ok := findSpan(spans, "workflow.prepare")
	if !ok {
		t.Fatalf("no workflow.prepare span recorded; spans=%v", spanNames(spans))
	}
	if got := prepare.attributes["workflow.id"]; got != "wf-trace-1" {
		t.Fatalf("workflow.prepare workflow.id=%q, want wf-trace-1", got)
	}
	if got := prepare.attributes["workflow.type"]; got != "payment-transfer" {
		t.Fatalf("workflow.prepare workflow.type=%q, want payment-transfer", got)
	}
}

// TestWorkflowForwardDispatchEmitsExecuteAndWaitSpans asserts that dispatching
// the first action produces both a workflow.action.execute span (around
// action.Execute) and a workflow.action.wait span (marking the wait for the
// result event), each carrying the brief-mandated attributes for the forward
// direction.
func TestWorkflowForwardDispatchEmitsExecuteAndWaitSpans(t *testing.T) {
	exporter := installTraceRecorder(t)

	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-trace-2", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	inst := store.instance("wf-trace-2")
	commandID := inst.Actions[0].CommandID
	if commandID == "" {
		t.Fatal("first action has no command id")
	}

	spans := snapshotSpans(exporter)

	execute, ok := findSpan(spans, "workflow.action.execute")
	if !ok {
		t.Fatalf("no workflow.action.execute span; spans=%v", spanNames(spans))
	}
	assertWorkflowAttrs(t, "workflow.action.execute", execute, "wf-trace-2", "payment-transfer", "book-transfer", directionForward)
	// command.id on execute reflects the dispatch's CommandID.
	if got := execute.attributes["command.id"]; got != commandID {
		t.Fatalf("workflow.action.execute command.id=%q, want %q", got, commandID)
	}

	wait, ok := findSpan(spans, "workflow.action.wait")
	if !ok {
		t.Fatalf("no workflow.action.wait span; spans=%v", spanNames(spans))
	}
	assertWorkflowAttrs(t, "workflow.action.wait", wait, "wf-trace-2", "payment-transfer", "book-transfer", directionForward)
	if got := wait.attributes["command.id"]; got != commandID {
		t.Fatalf("workflow.action.wait command.id=%q, want %q", got, commandID)
	}
}

// TestWorkflowCompensationEmitsCompensateSpan drives a 2-step workflow to a
// terminal failure on step 2 and asserts that workflow.action.compensate is
// recorded for the undo of step 1, with workflow.direction=compensation.
func TestWorkflowCompensationEmitsCompensateSpan(t *testing.T) {
	exporter := installTraceRecorder(t)

	store := newMemoryStore()
	def := linearDef{
		workflowType: "compensatable-trace-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "first", forwardDisp: fwdDispatch("first"), forwardOutcome: okOutcome(), compDisp: compDispatch("first")},
			&scriptedAction{name: "second", forwardDisp: fwdDispatch("second"), forwardOutcome: rejectedOutcome("domain rejected")},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-trace-comp", Type: "compensatable-trace-flow", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}

	// Step 1 succeeds, step 2 fails terminally → compensation begins on step 1.
	deliverForward(t, engine, store, "wf-trace-comp", "first", 0)
	deliverForwardFailure(t, engine, store, "wf-trace-comp", "second", 1)

	inst := store.instance("wf-trace-comp")
	if inst.Status != StatusCompensating {
		t.Fatalf("instance status=%q, want compensating", inst.Status)
	}
	commandID := inst.Actions[0].CommandID
	if commandID == "" {
		t.Fatal("compensated action has no command id")
	}

	spans := snapshotSpans(exporter)
	compensate, ok := findSpan(spans, "workflow.action.compensate")
	if !ok {
		t.Fatalf("no workflow.action.compensate span; spans=%v", spanNames(spans))
	}
	assertWorkflowAttrs(t, "workflow.action.compensate", compensate, "wf-trace-comp", "compensatable-trace-flow", "first", directionCompensation)
	if got := compensate.attributes["command.id"]; got != commandID {
		t.Fatalf("workflow.action.compensate command.id=%q, want %q", got, commandID)
	}
}

func assertWorkflowAttrs(t *testing.T, spanName string, span spanSnapshot, workflowID, workflowType, actionName, direction string) {
	t.Helper()
	checks := []struct{ key, want string }{
		{"workflow.id", workflowID},
		{"workflow.type", workflowType},
		{"workflow.action", actionName},
		{"workflow.direction", direction},
	}
	for _, c := range checks {
		if got := span.attributes[c.key]; got != c.want {
			t.Fatalf("%s %s=%q, want %q", spanName, c.key, got, c.want)
		}
	}
}

func spanNames(spans []spanSnapshot) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		names = append(names, span.name)
	}
	return names
}

// Compile-time guard: ensure we treat attribute keys consistently with the
// engine implementation.
var _ = attribute.Key("workflow.id")
