package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"commerce/internal/inventory"
	"commerce/internal/platform/config"
	"commerce/internal/platform/httpx"
	"commerce/internal/platform/messaging"
	"commerce/internal/platform/postgres"
	amqp "github.com/rabbitmq/amqp091-go"
)

const eventExchange = "commerce.events"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	signalContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	processContext, cancelProcess := context.WithCancel(signalContext)
	defer cancelProcess()
	settings, err := config.Load("inventory")
	if err != nil {
		return err
	}
	pool, err := postgres.Open(processContext, settings.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	connection, err := amqp.Dial(settings.Broker.URL)
	if err != nil {
		return fmt.Errorf("open inventory broker: %w", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open inventory publisher channel: %w", err)
	}
	if err := channel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		_ = channel.Close()
		return fmt.Errorf("declare inventory event exchange: %w", err)
	}
	publisher, err := messaging.NewRabbitPublisher(channel, eventExchange)
	if err != nil {
		_ = channel.Close()
		return err
	}
	defer publisher.Close()
	relay := inventory.NewRelayLifecycle(processContext, func(ctx context.Context) error {
		return messaging.RunRelay(ctx, pool, publisher, messaging.RelayConfig{
			BatchSize: settings.Outbox.BatchSize, PollInterval: settings.Outbox.PollInterval,
		})
	})

	handler := inventory.NewHandler(inventory.NewService(inventory.NewPostgresStore(pool), nil))
	server := httpx.NewServer(httpx.ServerConfig{
		Service:           settings.Service,
		Instance:          settings.Instance,
		Addr:              settings.HTTP.Addr,
		Handler:           handler,
		Ready:             inventory.NewRuntimeReadiness(pool.Ping, publisher, connection.IsClosed, relay),
		ShutdownTimeout:   settings.Shutdown.Timeout,
		RequestBodyLimit:  settings.HTTP.RequestBodyLimit,
		ReadHeaderTimeout: settings.HTTP.ReadHeaderTimeout,
		ReadTimeout:       settings.HTTP.ReadTimeout,
		WriteTimeout:      settings.HTTP.WriteTimeout,
		IdleTimeout:       settings.HTTP.IdleTimeout,
	})
	serverError := make(chan error, 1)
	go func() { serverError <- server.ListenAndServe() }()
	var runError error
	select {
	case <-signalContext.Done():
	case err := <-serverError:
		if !httpx.IsClosed(err) {
			runError = err
		}
	case <-relay.Done():
		if signalContext.Err() == nil {
			runError = fmt.Errorf("run inventory outbox relay: %w", relay.ErrIfStopped())
		}
	}
	cancelProcess()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.Shutdown.Timeout)
	defer cancelShutdown()
	shutdownError := server.Shutdown(shutdownContext)
	relayError := relay.Wait(shutdownContext)
	if runError == nil && errors.Is(shutdownError, context.Canceled) && relayError == nil {
		return nil
	}
	return errors.Join(runError, shutdownError, relayError)
}
