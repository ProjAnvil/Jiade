package inventory

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeReadinessFailsForPublisherOrRelayLoss(t *testing.T) {
	publisher := &availabilityStub{}
	publisher.available.Store(true)
	relayContext, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relay := NewRelayLifecycle(relayContext, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	ready := NewRuntimeReadiness(
		func(context.Context) error { return nil },
		publisher,
		func() bool { return false },
		relay,
	)
	if err := ready(context.Background()); err != nil {
		t.Fatalf("initial readiness: %v", err)
	}

	publisher.available.Store(false)
	if err := ready(context.Background()); err == nil || !strings.Contains(err.Error(), "publisher") {
		t.Fatalf("publisher readiness error=%v", err)
	}
	publisher.available.Store(true)
	cancelRelay()
	waitContext, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := relay.Wait(waitContext); err != nil {
		t.Fatalf("wait relay: %v", err)
	}
	if err := ready(context.Background()); err == nil || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("stopped relay readiness error=%v", err)
	}
}

func TestRuntimeReadinessIncludesDatabaseAndConnection(t *testing.T) {
	publisher := &availabilityStub{}
	publisher.available.Store(true)
	relayContext, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relay := NewRelayLifecycle(relayContext, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	databaseErr := errors.New("database unavailable")
	ready := NewRuntimeReadiness(
		func(context.Context) error { return databaseErr },
		publisher,
		func() bool { return false },
		relay,
	)
	if err := ready(context.Background()); !errors.Is(err, databaseErr) {
		t.Fatalf("database readiness error=%v", err)
	}
	ready = NewRuntimeReadiness(
		func(context.Context) error { return nil },
		publisher,
		func() bool { return true },
		relay,
	)
	if err := ready(context.Background()); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("connection readiness error=%v", err)
	}
}

func TestRelayLifecycleRetainsErrorAndWaitsAfterCancellation(t *testing.T) {
	relayError := errors.New("publisher retired")
	exited := NewRelayLifecycle(context.Background(), func(context.Context) error {
		return relayError
	})
	waitContext, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := exited.Wait(waitContext); !errors.Is(err, relayError) {
		t.Fatalf("exited relay error=%v", err)
	}
	if err := exited.ErrIfStopped(); !errors.Is(err, relayError) {
		t.Fatalf("retained relay error=%v", err)
	}

	relayContext, cancelRelay := context.WithCancel(context.Background())
	entered := make(chan struct{})
	cancelled := NewRelayLifecycle(relayContext, func(ctx context.Context) error {
		close(entered)
		<-ctx.Done()
		return nil
	})
	<-entered
	cancelRelay()
	if err := cancelled.Wait(waitContext); err != nil {
		t.Fatalf("cancelled relay wait=%v", err)
	}
	select {
	case <-cancelled.Done():
	default:
		t.Fatal("cancelled relay did not close Done")
	}
}

type availabilityStub struct{ available atomic.Bool }

func (publisher *availabilityStub) Available() bool { return publisher.available.Load() }
