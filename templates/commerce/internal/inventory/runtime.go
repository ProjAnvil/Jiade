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
