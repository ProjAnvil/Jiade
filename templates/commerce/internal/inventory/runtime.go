package inventory

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

type publisherAvailability interface {
	Available() bool
}

// RelayLifecycle retains a relay's terminal result and exposes a reusable
// completion signal for readiness, process supervision, and graceful shutdown.
type RelayLifecycle struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func NewRelayLifecycle(ctx context.Context, run func(context.Context) error) *RelayLifecycle {
	lifecycle := &RelayLifecycle{done: make(chan struct{})}
	go func() {
		err := run(ctx)
		lifecycle.mu.Lock()
		lifecycle.err = err
		lifecycle.mu.Unlock()
		close(lifecycle.done)
	}()
	return lifecycle
}

func (lifecycle *RelayLifecycle) Done() <-chan struct{} { return lifecycle.done }

func (lifecycle *RelayLifecycle) Wait(ctx context.Context) error {
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		return lifecycle.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (lifecycle *RelayLifecycle) ErrIfStopped() error {
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		if lifecycle.err != nil {
			return lifecycle.err
		}
		return errors.New("inventory outbox relay stopped")
	default:
		return nil
	}
}

func NewRuntimeReadiness(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	relay *RelayLifecycle,
) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := database(ctx); err != nil {
			return err
		}
		if brokerClosed != nil && brokerClosed() {
			return errors.New("inventory broker connection is closed")
		}
		if publisher == nil || !publisher.Available() {
			return errors.New("inventory publisher is unavailable")
		}
		if relay == nil {
			return errors.New("inventory outbox relay is unavailable")
		}
		if err := relay.ErrIfStopped(); err != nil {
			return fmt.Errorf("inventory outbox relay stopped: %w", err)
		}
		return nil
	}
}

type combinedAvailability []publisherAvailability

func (publishers combinedAvailability) Available() bool {
	if len(publishers) == 0 {
		return false
	}
	for _, publisher := range publishers {
		if publisher == nil || !publisher.Available() {
			return false
		}
	}
	return true
}

// CombinePublisherAvailability returns a single readiness predicate over a set
// of publishers.
func CombinePublisherAvailability(publishers ...publisherAvailability) publisherAvailability {
	return combinedAvailability(publishers)
}

// workerLifecycle is the minimal completion-signal surface NewRuntimeReadiness
// probes for both RelayLifecycle and Consumer's WorkerLifecycle.
type workerLifecycle interface {
	ErrIfStopped() error
}

// NewRuntimeReadinessWithDependencies additionally probes a list of external
// dependency readiness functions and a set of workers (relay + consumer) before
// reporting ready. Mirrors internal/payment.NewRuntimeReadinessWithDependencies
// and internal/fulfillment.NewRuntimeReadinessWithDependencies.
func NewRuntimeReadinessWithDependencies(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	dependencies []func(context.Context) error,
	workers ...workerLifecycle,
) func(context.Context) error {
	return func(ctx context.Context) error {
		if database == nil {
			return errors.New("inventory database readiness is unavailable")
		}
		if err := database(ctx); err != nil {
			return err
		}
		if brokerClosed != nil && brokerClosed() {
			return errors.New("inventory broker connection is closed")
		}
		if publisher == nil || !publisher.Available() {
			return errors.New("inventory publisher is unavailable")
		}
		for index, dependency := range dependencies {
			if dependency == nil {
				return fmt.Errorf("inventory dependency %d readiness is unavailable", index)
			}
			if err := dependency(ctx); err != nil {
				return fmt.Errorf("inventory dependency %d is unavailable: %w", index, err)
			}
		}
		if len(workers) == 0 {
			return errors.New("inventory workers are unavailable")
		}
		for _, worker := range workers {
			if err := worker.ErrIfStopped(); err != nil {
				return fmt.Errorf("inventory worker stopped: %w", err)
			}
		}
		return nil
	}
}
