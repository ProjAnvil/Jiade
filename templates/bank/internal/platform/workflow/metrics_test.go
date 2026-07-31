package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// requiredMetricNames is the verbatim list of metric family names the durable
// workflow engine MUST expose, per Task 8's brief. The tests in this file
// assert each name is present in a Prometheus Gather after the engine is
// exercised end-to-end.
var requiredMetricNames = []string{
	"workflow_instances",
	"workflow_action_duration_seconds",
	"workflow_action_attempts_total",
	"workflow_compensation_total",
	"workflow_compensation_failures_total",
	"workflow_waiting_age_seconds",
}

// TestMetricsExposeRequiredNames exercises the engine through every kind of
// state transition (start, prepare, advance, terminal failure, compensation
// success, compensation exhaust, timed-out redispatch) with Metrics wired
// into a fresh Prometheus registry, then gathers and asserts every required
// metric family name is present.
//
// The test deliberately uses a single shared Metrics/registry so the gathered
// families reflect a realistic mix of concurrent instances; labels distinguish
// the per-instance values.
func TestMetricsExposeRequiredNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	// Phase 1: forward happy path. A 1-action workflow that succeeds touches
	// workflow_instances (preparing/running/succeeded) plus the forward
	// action_duration and action_attempts.
	happyEngine, happyStore := engineWithMetrics(t, metrics, linearDef{
		workflowType: "happy-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "happy-step", forwardDisp: fwdDispatch("happy-step"), forwardOutcome: okOutcome(), compDisp: compDispatch("happy-step")},
		},
	}, nil)
	startAndPrepare(t, happyEngine, happyStore, "wf-happy", "happy-flow")
	// Forward success on the last action → instance succeeded.
	deliverForward(t, happyEngine, happyStore, "wf-happy", "happy-step", 0)

	// Phase 2: compensation success. A 2-action workflow whose step-2 fails
	// terminally triggers reverse compensation of step-1; the compensation
	// result succeeds → instance compensated. This exercises the compensation
	// dispatch path (workflow_compensation_total + action_duration[attempts]
	// for direction=compensation).
	compEngine, compStore := engineWithMetrics(t, metrics, linearDef{
		workflowType: "comp-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "comp-a", forwardDisp: fwdDispatch("comp-a"), forwardOutcome: okOutcome(), compDisp: compDispatch("comp-a")},
			&scriptedAction{name: "comp-b", forwardDisp: fwdDispatch("comp-b"), forwardOutcome: rejectedOutcome("insufficient funds")},
		},
	}, nil)
	startAndPrepare(t, compEngine, compStore, "wf-comp", "comp-flow")
	deliverForward(t, compEngine, compStore, "wf-comp", "comp-a", 0)
	deliverForwardFailure(t, compEngine, compStore, "wf-comp", "comp-b", 1) // terminal failure → begin compensation
	deliverCompensation(t, compEngine, compStore, "wf-comp", "comp-a", 0)   // compensation success → compensated

	// Phase 3: compensation exhaust. step-1 compensation always fails
	// transiently; after CompensationMaxAttempts (default 5) the action and
	// instance transition to compensation_failed, firing the deferred
	// workflow_compensation_failures_total counter.
	exhaustEngine, exhaustStore := engineWithMetrics(t, metrics, linearDef{
		workflowType: "exhaust-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{
				name:           "exh-a",
				forwardDisp:    fwdDispatch("exh-a"),
				forwardOutcome: okOutcome(),
				compDisp:       compDispatch("exh-a"),
				// Six transient failures — more than the default max of 5.
				compOutcomes: []Outcome{
					transientOutcome("f1"), transientOutcome("f2"),
					transientOutcome("f3"), transientOutcome("f4"),
					transientOutcome("f5"), transientOutcome("f6"),
				},
			},
			&scriptedAction{name: "exh-b", forwardDisp: fwdDispatch("exh-b"), forwardOutcome: rejectedOutcome("nope")},
		},
	}, nil)
	startAndPrepare(t, exhaustEngine, exhaustStore, "wf-exh", "exhaust-flow")
	deliverForward(t, exhaustEngine, exhaustStore, "wf-exh", "exh-a", 0)
	deliverForwardFailure(t, exhaustEngine, exhaustStore, "wf-exh", "exh-b", 1)
	const defaultCompMax = 5
	for i := 0; i < defaultCompMax; i++ {
		deliverCompensation(t, exhaustEngine, exhaustStore, "wf-exh", "exh-a", 0)
	}
	got := exhaustStore.instance("wf-exh")
	assertStatus(t, got, StatusCompensationFailed)

	// Phase 4: waiting-action age. A running instance whose current action has
	// passed its DeadlineAt is re-dispatched; the engine records the waiting
	// age (now - DeadlineAt) on the workflow_waiting_age_seconds gauge.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	waitEngine, waitStore := engineWithMetrics(t, metrics, linearDef{
		workflowType: "wait-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "wait-step", forwardDisp: fwdDispatch("wait-step"), forwardOutcome: okOutcome(), compDisp: compDispatch("wait-step")},
		},
	}, func() time.Time { return clock })
	startAndPrepare(t, waitEngine, waitStore, "wf-wait", "wait-flow")
	// Advance the injected clock past the 30s action deadline (linearAction
	// uses 30s; scriptedAction copies fwdDispatch which sets 30s).
	clock = base.Add(31 * time.Second)
	if err := waitEngine.Redispatch(context.Background(), "wf-wait"); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}

	// Assert every required family name is present in a gather of the registry.
	gathered, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	have := make(map[string]bool, len(gathered))
	for _, mf := range gathered {
		have[mf.GetName()] = true
	}
	for _, name := range requiredMetricNames {
		if !have[name] {
			t.Errorf("required metric %q not present in gathered families; have: %v", name, have)
		}
	}
}

// TestMetricsRecordCompensationFailureOnExhaust verifies the Task 5 deferred
// failure metric — workflow_compensation_failures_total — increments exactly
// once when an action and instance transition to compensation_failed after
// exhausting CompensationMaxAttempts transient failures.
func TestMetricsRecordCompensationFailureOnExhaust(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	engine, store := engineWithMetrics(t, metrics, linearDef{
		workflowType: "fail-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{
				name:           "fail-a",
				forwardDisp:    fwdDispatch("fail-a"),
				forwardOutcome: okOutcome(),
				compDisp:       compDispatch("fail-a"),
				compOutcomes: []Outcome{
					transientOutcome("f1"), transientOutcome("f2"),
					transientOutcome("f3"), transientOutcome("f4"),
					transientOutcome("f5"), transientOutcome("f6"),
				},
			},
			&scriptedAction{name: "fail-b", forwardDisp: fwdDispatch("fail-b"), forwardOutcome: rejectedOutcome("nope")},
		},
	}, nil)
	startAndPrepare(t, engine, store, "wf-fail", "fail-flow")
	deliverForward(t, engine, store, "wf-fail", "fail-a", 0)
	deliverForwardFailure(t, engine, store, "wf-fail", "fail-b", 1)

	// Pre-condition: no failure recorded yet.
	before := testutil.ToFloat64(metrics.compensationFailures.WithLabelValues("fail-a"))
	if before != 0 {
		t.Fatalf("compensation_failures_total before exhaust = %v, want 0", before)
	}

	// Drive CompensationMaxAttempts transient failures → exhaust → failed.
	for i := 0; i < 5; i++ {
		deliverCompensation(t, engine, store, "wf-fail", "fail-a", 0)
	}
	got := store.instance("wf-fail")
	assertStatus(t, got, StatusCompensationFailed)

	after := testutil.ToFloat64(metrics.compensationFailures.WithLabelValues("fail-a"))
	if after != 1 {
		t.Errorf("compensation_failures_total{action=fail-a} = %v, want 1 after exhaust", after)
	}
}

// TestMetricsRecordWaitingAgeOnRedispatch verifies the waiting-age gauge is set
// to a positive value (now - DeadlineAt, in seconds) when Redispatch observes a
// timed-out waiting action.
func TestMetricsRecordWaitingAgeOnRedispatch(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics := NewMetrics(reg)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	engine, store := engineWithMetrics(t, metrics, linearDef{
		workflowType: "age-flow",
		version:      1,
		actions: []Action{
			&scriptedAction{name: "age-step", forwardDisp: fwdDispatch("age-step"), forwardOutcome: okOutcome(), compDisp: compDispatch("age-step")},
		},
	}, func() time.Time { return clock })
	startAndPrepare(t, engine, store, "wf-age", "age-flow")

	// Advance 31s past the 30s action deadline.
	clock = base.Add(31 * time.Second)
	if err := engine.Redispatch(context.Background(), "wf-age"); err != nil {
		t.Fatalf("Redispatch: %v", err)
	}

	// workflow_waiting_age_seconds should be ~1s (now - DeadlineAt = 31s - 30s).
	if got := testutil.ToFloat64(metrics.waitingAge); got <= 0 {
		t.Errorf("workflow_waiting_age_seconds = %v, want > 0 after timed-out redispatch", got)
	}
}

// TestMetricsNilSafeOnDefaultEngine verifies that an Engine constructed without
// SetMetrics (the backward-compatible default) does not panic when driving the
// full lifecycle. This guards the nil-safe contract that keeps Tasks 1-7
// tests unchanged.
func TestMetricsNilSafeOnDefaultEngine(t *testing.T) {
	store := newMemoryStore()
	engine := NewEngine(store, registryWith(linearDefinition()), EngineConfig{})
	// engine.metrics is nil by default; no SetMetrics call.
	if engine.metrics != nil {
		t.Fatalf("engine.metrics = %v, want nil by default", engine.metrics)
	}

	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: "wf-nil", Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatal(err)
	}
	// Drive a forward success; status transitions must not panic on nil metrics.
	deliverForward(t, engine, store, "wf-nil", "book-transfer", 0)
}

// ---------------------------------------------------------------------------
// Helpers shared by the metrics tests.
// ---------------------------------------------------------------------------

// engineWithMetrics constructs an Engine wired up with the given Metrics and
// Definition. If now is non-nil it is used as the engine clock (for timeout
// tests); otherwise the default time.Now is used.
func engineWithMetrics(t *testing.T, metrics *Metrics, def linearDef, now func() time.Time) (*Engine, *memoryStore) {
	t.Helper()
	store := newMemoryStore()
	cfg := EngineConfig{}
	if now != nil {
		cfg.Now = now
	}
	engine := NewEngine(store, registryWith(def), cfg)
	engine.SetMetrics(metrics)
	return engine, store
}

// startAndPrepare creates an instance of the given workflow type and drives it
// through Prepare, leaving it in StatusRunning with action[0] waiting.
func startAndPrepare(t *testing.T, engine *Engine, store *memoryStore, id, workflowType string) {
	t.Helper()
	instance, err := engine.Start(context.Background(), StartRequest{
		WorkflowID: id, Type: workflowType, Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Start %q: %v", id, err)
	}
	if err := engine.Prepare(context.Background(), instance.ID); err != nil {
		t.Fatalf("Prepare %q: %v", id, err)
	}
}
