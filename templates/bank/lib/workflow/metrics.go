package workflow

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds the Prometheus collectors the durable workflow engine updates
// at its state transitions. The zero value is NOT usable; construct one with
// NewMetrics, which registers every collector with the supplied Registerer.
//
// All recording methods are nil-safe: a nil *Metrics receiver is a no-op. This
// keeps the Engine backward-compatible — Engines constructed without
// SetMetrics (the Tasks 1-7 default) incur zero metric overhead and never
// panic, so existing tests do not need to be rewritten.
//
// Collector labels:
//
//   - workflow_instances{status}                     — GaugeVec, current count
//     of instances in each InstanceStatus. The Engine increments the new
//     status label and decrements the old one on every transition.
//   - workflow_action_duration_seconds{action,direction} — HistogramVec, the
//     wall-clock duration the engine spent inside Action.Execute (forward) or
//     Action.Compensate (compensation). The engine observes one value per
//     dispatch attempt, including re-dispatch via Redispatch.
//   - workflow_action_attempts_total{action,direction} — CounterVec, total
//     dispatch attempts the engine has made per action and direction. Every
//     persistActionDispatch / persistCompensationDispatch / Redispatch
//     increments the relevant child by one.
//   - workflow_compensation_total{action}           — CounterVec, total
//     compensation dispatches emitted per action.
//   - workflow_compensation_failures_total{action}  — CounterVec, the deferred
//     Task 5 failure metric. Incremented by exactly one when an action and its
//     instance transition to compensation_failed after exhausting
//     CompensationMaxAttempts transient failures.
//   - workflow_waiting_age_seconds                  — Gauge, the most recently
//     observed waiting age (now - DeadlineAt, in seconds) for a timed-out
//     waiting action seen by the recovery path.
type Metrics struct {
	instances *prometheus.GaugeVec
	// actionDuration observes the synchronous Action.Execute/Compensate call
	// duration, labeled by action name and direction ("forward"/"compensation").
	actionDuration *prometheus.HistogramVec
	// actionAttempts counts dispatch attempts per action+direction.
	actionAttempts *prometheus.CounterVec
	// compensation counts compensation dispatches per action.
	compensation *prometheus.CounterVec
	// compensationFailures counts compensation exhaust events per action.
	// Task 5 deferred the failure metric to Task 8; this is it.
	compensationFailures *prometheus.CounterVec
	// waitingAge is the most recently observed waiting age for a timed-out
	// action seen by Redispatch. A plain Gauge (no labels) is deliberate:
	// cardinality is bounded by "last observed" rather than per-instance.
	waitingAge prometheus.Gauge
}

// NewMetrics constructs a Metrics value whose collectors are registered with
// reg. Pass a *prometheus.Registry in production (or a fresh
// prometheus.NewRegistry() in tests). A nil reg is treated as
// prometheus.DefaultRegisterer; tests should always pass a private registry to
// avoid cross-test interference.
//
// The returned *Metrics is safe for concurrent use: every collector it holds
// is itself concurrency-safe, and the struct is immutable after construction.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	m := &Metrics{
		instances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "workflow_instances",
			Help: "Current number of workflow instances by lifecycle status.",
		}, []string{"status"}),
		actionDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "workflow_action_duration_seconds",
			Help:    "Wall-clock duration the engine spent inside Action.Execute or Action.Compensate, by action and direction.",
			Buckets: prometheus.DefBuckets,
		}, []string{"action", "direction"}),
		actionAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "workflow_action_attempts_total",
			Help: "Total workflow action dispatch attempts, by action and direction.",
		}, []string{"action", "direction"}),
		compensation: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "workflow_compensation_total",
			Help: "Total compensation dispatches emitted, by action.",
		}, []string{"action"}),
		compensationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "workflow_compensation_failures_total",
			Help: "Total actions that exhausted CompensationMaxAttempts and transitioned the instance to compensation_failed.",
		}, []string{"action"}),
		waitingAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "workflow_waiting_age_seconds",
			Help: "Most recently observed waiting age (now minus DeadlineAt, seconds) of a timed-out waiting action seen by the recovery path.",
		}),
	}
	for _, c := range [...]prometheus.Collector{
		m.instances,
		m.actionDuration,
		m.actionAttempts,
		m.compensation,
		m.compensationFailures,
		m.waitingAge,
	} {
		reg.MustRegister(c)
	}
	return m
}

// enterStatus increments the gauge for status s. Used when a brand-new instance
// is created (no prior status to decrement). Nil-safe.
func (m *Metrics) enterStatus(s InstanceStatus) {
	if m == nil {
		return
	}
	m.instances.WithLabelValues(string(s)).Inc()
}

// changeStatus atomically moves one instance's contribution from the old
// status label to the new one. Nil-safe.
func (m *Metrics) changeStatus(from, to InstanceStatus) {
	if m == nil {
		return
	}
	m.instances.WithLabelValues(string(from)).Dec()
	m.instances.WithLabelValues(string(to)).Inc()
}

// observeAction records the duration of an Execute/Compensate call and bumps
// the per-attempt counter. direction is directionForward or
// directionCompensation. Nil-safe.
func (m *Metrics) observeAction(name, direction string, took time.Duration) {
	if m == nil {
		return
	}
	m.actionDuration.WithLabelValues(name, direction).Observe(took.Seconds())
	m.actionAttempts.WithLabelValues(name, direction).Inc()
}

// observeCompensationDispatch bumps the compensation counter for an action.
// Called on every compensation dispatch (initial + retries). Nil-safe.
func (m *Metrics) observeCompensationDispatch(name string) {
	if m == nil {
		return
	}
	m.compensation.WithLabelValues(name).Inc()
}

// recordCompensationFailure increments the deferred failure counter for an
// action whose compensation exhausted CompensationMaxAttempts. Nil-safe.
func (m *Metrics) recordCompensationFailure(name string) {
	if m == nil {
		return
	}
	m.compensationFailures.WithLabelValues(name).Inc()
}

// recordWaitingAge sets the waiting-age gauge to ageSeconds. Called from the
// recovery path (Redispatch) when a timed-out waiting action is observed.
// Nil-safe.
func (m *Metrics) recordWaitingAge(ageSeconds float64) {
	if m == nil {
		return
	}
	m.waitingAge.Set(ageSeconds)
}
