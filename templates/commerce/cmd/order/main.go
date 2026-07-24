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

	"commerce/internal/order"
	platformclient "commerce/internal/platform/client"
	"commerce/internal/platform/config"
	"commerce/internal/platform/httpx"
	"commerce/internal/platform/messaging"
	"commerce/internal/platform/postgres"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	eventExchange    = "commerce.events"
	orderQueue       = "order.saga"
	retryExchange    = "commerce.events.retry"
	retryQueue       = "order.saga.retry"
	retryRoute       = "order.retry"
	retryReturnRoute = "order.retry.return"
	deadExchange     = "commerce.events.dlx"
	deadQueue        = "order.saga.dlq"
	deadRoute        = "order.dead"
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

	settings, err := config.Load("order")
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
		return fmt.Errorf("open order broker: %w", err)
	}
	defer connection.Close()
	publisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open order publisher channel: %w", err)
	}
	if err := publisherChannel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		_ = publisherChannel.Close()
		return fmt.Errorf("declare order event exchange: %w", err)
	}
	publisher, err := messaging.NewRabbitPublisher(publisherChannel, eventExchange)
	if err != nil {
		_ = publisherChannel.Close()
		return err
	}
	defer publisher.Close()

	consumerChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open order consumer channel: %w", err)
	}
	defer consumerChannel.Close()
	if err := declareOrderConsumerTopology(consumerChannel); err != nil {
		return err
	}
	retryPublisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open order retry publisher channel: %w", err)
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
	store := order.NewPostgresStore(pool, nil)
	customerClient := order.NewCustomerHTTPClient(environment("CUSTOMER_URL", "http://customer:8080"), resilient)
	catalogClient := order.NewCatalogHTTPClient(environment("CATALOG_URL", "http://catalog:8080"), resilient)
	inventoryClient := order.NewInventoryHTTPClient(environment("INVENTORY_URL", "http://inventory:8080"), resilient)
	service := order.NewService(
		store,
		customerClient,
		catalogClient,
		inventoryClient,
		order.ServiceOptions{},
	)
	relay := order.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return messaging.RunRelay(ctx, pool, publisher, messaging.RelayConfig{
			BatchSize: settings.Outbox.BatchSize, PollInterval: settings.Outbox.PollInterval,
		})
	})
	consumer := order.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return order.RunConsumer(ctx, consumerChannel, retryRouter, orderQueue,
			order.NewConsumer(store, messaging.RetryPolicy{MaxAttempts: 3}).WithRetryQueue(retryQueue),
			order.DeliveryRouting{
				RetryExchange: retryExchange, RetryRoutingKey: retryRoute,
				DeadExchange: deadExchange, DeadRoutingKey: deadRoute,
			})
	})

	server := httpx.NewServer(httpx.ServerConfig{
		Service:  settings.Service,
		Instance: settings.Instance,
		Addr:     settings.HTTP.Addr,
		Handler:  order.NewHandler(service),
		Ready: order.NewRuntimeReadinessWithDependencies(
			pool.Ping, order.CombinePublisherAvailability(publisher, retryRouter),
			connection.IsClosed,
			[]func(context.Context) error{
				customerClient.Ready, catalogClient.Ready, inventoryClient.Ready,
			},
			relay, consumer),
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
			runError = fmt.Errorf("run order outbox relay: %w", relay.ErrIfStopped())
		}
	case <-consumer.Done():
		if signalContext.Err() == nil {
			runError = fmt.Errorf("run order saga consumer: %w", consumer.ErrIfStopped())
		}
	}

	cancelProcess()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), settings.Shutdown.Timeout)
	defer cancelShutdown()
	shutdownError := server.Shutdown(shutdownContext)
	relayError := relay.Wait(shutdownContext)
	consumerError := consumer.Wait(shutdownContext)
	return errors.Join(runError, shutdownError, relayError, consumerError)
}

func declareOrderConsumerTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare order event exchange: %w", err)
	}
	if err := channel.ExchangeDeclare(deadExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare order dead-letter exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare order dead-letter queue: %w", err)
	}
	if err := channel.QueueBind(deadQueue, "#", deadExchange, false, nil); err != nil {
		return fmt.Errorf("bind order dead-letter queue: %w", err)
	}
	if err := channel.ExchangeDeclare(retryExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare order retry exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(retryQueue, true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(2000),
		"x-dead-letter-exchange":    eventExchange,
		"x-dead-letter-routing-key": retryReturnRoute,
	}); err != nil {
		return fmt.Errorf("declare order retry queue: %w", err)
	}
	if err := channel.QueueBind(retryQueue, retryRoute, retryExchange, false, nil); err != nil {
		return fmt.Errorf("bind order retry queue: %w", err)
	}
	if _, err := channel.QueueDeclare(orderQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare order saga queue: %w", err)
	}
	if err := channel.QueueBind(orderQueue, retryReturnRoute, eventExchange, false, nil); err != nil {
		return fmt.Errorf("bind order retry return: %w", err)
	}
	for _, binding := range orderEventBindings() {
		if err := channel.QueueBind(orderQueue, binding, eventExchange, false, nil); err != nil {
			return fmt.Errorf("bind order saga queue %s: %w", binding, err)
		}
	}
	return nil
}

func orderEventBindings() []string {
	return []string{
		"payment.failed.v1",
		"payment.captured.v1",
		"payment.succeeded.v1",
		"payment.paid.v1",
		"payment.refunded.v1",
		"refund.succeeded.v1",
		"inventory.committed.v1",
		"inventory.reservation-committed.v1",
		"inventory.released.v1",
		"inventory.reservation-released.v1",
		"fulfillment.cancelled.v1",
		"fulfillment.cancellation-succeeded.v1",
		"fulfillment.completed.v1",
		"fulfillment.delivered.v1",
	}
}

func environment(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
