package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"commerce/internal/fulfillment"
	platformclient "commerce/internal/platform/client"
	"commerce/internal/platform/config"
	"commerce/internal/platform/httpx"
	"commerce/internal/platform/messaging"
	"commerce/internal/platform/postgres"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	eventExchange        = "commerce.events"
	fulfillmentQueue     = "fulfillment.workflow"
	retryExchange        = "commerce.events.retry"
	retryQueue           = "fulfillment.retry"
	retryRoute           = "fulfillment.retry"
	retryReturnRoute     = "fulfillment.retry.return"
	deadExchange         = "commerce.events.dlx"
	deadQueue            = "fulfillment.dlq"
	deadRoute            = "fulfillment.dead"
)

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

	settings, err := config.Load("fulfillment")
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
		return fmt.Errorf("open fulfillment broker: %w", err)
	}
	defer connection.Close()
	publisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open fulfillment publisher channel: %w", err)
	}
	if err := publisherChannel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		_ = publisherChannel.Close()
		return fmt.Errorf("declare fulfillment event exchange: %w", err)
	}
	publisher, err := messaging.NewRabbitPublisher(publisherChannel, eventExchange)
	if err != nil {
		_ = publisherChannel.Close()
		return err
	}
	defer publisher.Close()

	consumerChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open fulfillment consumer channel: %w", err)
	}
	defer consumerChannel.Close()
	if err := declareFulfillmentConsumerTopology(consumerChannel); err != nil {
		return err
	}
	retryPublisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open fulfillment retry publisher channel: %w", err)
	}
	retryRouter, err := messaging.NewConfirmedRouter(retryPublisherChannel)
	if err != nil {
		_ = retryPublisherChannel.Close()
		return err
	}
	defer retryRouter.Close()

	resilient := platformclient.New(platformclient.Config{
		HTTPClient:     &http.Client{},
		TotalTimeout:   settings.Clients.RequestTimeout,
		AttemptTimeout: settings.Clients.AttemptTimeout,
	})
	store := fulfillment.NewPostgresStore(pool, nil)
	inventoryClient := fulfillment.NewInventoryHTTPClient(environment("INVENTORY_URL", "http://inventory:8080"), resilient)
	service := fulfillment.NewService(store, inventoryClient, fulfillment.ServiceOptions{})
	consumer := fulfillment.NewConsumer(store, service, messaging.RetryPolicy{MaxAttempts: 3}).WithRetryQueue(retryQueue)

	relay := fulfillment.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return messaging.RunRelay(ctx, pool, publisher, messaging.RelayConfig{
			BatchSize: settings.Outbox.BatchSize, PollInterval: settings.Outbox.PollInterval,
		})
	})
	consumerWorker := fulfillment.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return fulfillment.RunConsumer(ctx, consumerChannel, retryRouter, fulfillmentQueue, consumer,
			fulfillment.DeliveryRouting{
				RetryExchange: retryExchange, RetryRoutingKey: retryRoute,
				DeadExchange: deadExchange, DeadRoutingKey: deadRoute,
			})
	})

	server := httpx.NewServer(httpx.ServerConfig{
		Service:  settings.Service,
		Instance: settings.Instance,
		Addr:     settings.HTTP.Addr,
		Handler:  fulfillment.NewHandler(store),
		Ready: fulfillment.NewRuntimeReadinessWithDependencies(
			pool.Ping, fulfillment.CombinePublisherAvailability(publisher, retryRouter),
			connection.IsClosed,
			[]func(context.Context) error{inventoryClient.Ready},
			relay, consumerWorker),
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
			runError = fmt.Errorf("run fulfillment outbox relay: %w", relay.ErrIfStopped())
		}
	case <-consumerWorker.Done():
		if signalContext.Err() == nil {
			runError = fmt.Errorf("run fulfillment consumer: %w", consumerWorker.ErrIfStopped())
		}
	}

	cancelProcess()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.Shutdown.Timeout)
	defer cancelShutdown()
	shutdownError := server.Shutdown(shutdownContext)
	relayError := relay.Wait(shutdownContext)
	consumerError := consumerWorker.Wait(shutdownContext)
	return errors.Join(runError, shutdownError, relayError, consumerError)
}

func declareFulfillmentConsumerTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare fulfillment event exchange: %w", err)
	}
	if err := channel.ExchangeDeclare(deadExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare fulfillment dead-letter exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare fulfillment dead-letter queue: %w", err)
	}
	if err := channel.QueueBind(deadQueue, "#", deadExchange, false, nil); err != nil {
		return fmt.Errorf("bind fulfillment dead-letter queue: %w", err)
	}
	if err := channel.ExchangeDeclare(retryExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare fulfillment retry exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(retryQueue, true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(2000),
		"x-dead-letter-exchange":    eventExchange,
		"x-dead-letter-routing-key": retryReturnRoute,
	}); err != nil {
		return fmt.Errorf("declare fulfillment retry queue: %w", err)
	}
	if err := channel.QueueBind(retryQueue, retryRoute, retryExchange, false, nil); err != nil {
		return fmt.Errorf("bind fulfillment retry queue: %w", err)
	}
	if _, err := channel.QueueDeclare(fulfillmentQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare fulfillment workflow queue: %w", err)
	}
	if err := channel.QueueBind(fulfillmentQueue, retryReturnRoute, eventExchange, false, nil); err != nil {
		return fmt.Errorf("bind fulfillment retry return: %w", err)
	}
	for _, binding := range fulfillmentEventBindings() {
		if err := channel.QueueBind(fulfillmentQueue, binding, eventExchange, false, nil); err != nil {
			return fmt.Errorf("bind fulfillment workflow queue %s: %w", binding, err)
		}
	}
	return nil
}

func fulfillmentEventBindings() []string {
	return []string{
		"order.paid.v1",
		"order.cancelled.v1",
		"fulfillment.cancel-requested.v1",
	}
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
