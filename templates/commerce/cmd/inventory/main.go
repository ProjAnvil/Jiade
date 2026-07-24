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
	relayError := make(chan error, 1)
	go func() {
		relayError <- messaging.RunRelay(processContext, pool, publisher, messaging.RelayConfig{
			BatchSize: settings.Outbox.BatchSize, PollInterval: settings.Outbox.PollInterval,
		})
	}()

	handler := inventory.NewHandler(inventory.NewService(inventory.NewPostgresStore(pool), nil))
	server := httpx.NewServer(httpx.ServerConfig{
		Service:  settings.Service,
		Instance: settings.Instance,
		Addr:     settings.HTTP.Addr,
		Handler:  handler,
		Ready: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return err
			}
			if connection.IsClosed() {
				return errors.New("inventory broker connection is closed")
			}
			return nil
		},
		ShutdownTimeout:  settings.Shutdown.Timeout,
		RequestBodyLimit: settings.HTTP.RequestBodyLimit,
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
	case err := <-relayError:
		if err != nil {
			runError = fmt.Errorf("run inventory outbox relay: %w", err)
		}
	}
	cancelProcess()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.Shutdown.Timeout)
	defer cancelShutdown()
	err = server.Shutdown(shutdownContext)
	if runError != nil {
		return errors.Join(runError, err)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
