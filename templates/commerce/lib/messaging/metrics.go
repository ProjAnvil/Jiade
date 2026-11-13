package messaging

import "github.com/prometheus/client_golang/prometheus"

type outboxMetrics struct {
	oldestAge *prometheus.GaugeVec
}

func newOutboxMetrics(registry prometheus.Registerer) *outboxMetrics {
	candidate := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "outbox_oldest_age_seconds",
		Help: "Age in seconds of the oldest unpublished Commerce outbox event.",
	}, []string{"service"})
	if err := registry.Register(candidate); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			panic(err)
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.GaugeVec)
		if !ok {
			panic(err)
		}
		candidate = existing
	}
	return &outboxMetrics{oldestAge: candidate}
}

func (metrics *outboxMetrics) setOldestAge(service string, age float64) {
	if metrics == nil {
		return
	}
	if age < 0 {
		age = 0
	}
	metrics.oldestAge.WithLabelValues(service).Set(age)
}
