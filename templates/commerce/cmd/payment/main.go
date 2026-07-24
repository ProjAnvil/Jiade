package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/signal"
	"syscall"

	"commerce/internal/payment"
	"commerce/internal/platform/config"
	"commerce/internal/platform/httpx"
	"commerce/internal/platform/messaging"
	"commerce/internal/platform/postgres"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	eventExchange    = "commerce.events"
	paymentQueue     = "payment.workflow"
	retryExchange    = "commerce.events.retry"
	retryQueue       = "payment.retry"
	retryRoute       = "payment.retry"
	retryReturnRoute = "payment.retry.return"
	deadExchange     = "commerce.events.dlx"
	deadQueue        = "payment.dlq"
	deadRoute        = "payment.dead"
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

	settings, err := config.Load("payment")
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
		return fmt.Errorf("open payment broker: %w", err)
	}
	defer connection.Close()
	publisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open payment publisher channel: %w", err)
	}
	if err := publisherChannel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		_ = publisherChannel.Close()
		return fmt.Errorf("declare payment event exchange: %w", err)
	}
	publisher, err := messaging.NewRabbitPublisher(publisherChannel, eventExchange)
	if err != nil {
		_ = publisherChannel.Close()
		return err
	}
	defer publisher.Close()

	consumerChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open payment consumer channel: %w", err)
	}
	defer consumerChannel.Close()
	if err := declarePaymentConsumerTopology(consumerChannel); err != nil {
		return err
	}
	retryPublisherChannel, err := connection.Channel()
	if err != nil {
		return fmt.Errorf("open payment retry publisher channel: %w", err)
	}
	retryRouter, err := messaging.NewConfirmedRouter(retryPublisherChannel)
	if err != nil {
		_ = retryPublisherChannel.Close()
		return err
	}
	defer retryRouter.Close()

	store := payment.NewPostgresStore(pool, nil)
	provider := payment.NewSimulator(payment.ScenarioProviderTimeoutThenSuccess)
	service := payment.NewService(store, provider, payment.ServiceOptions{})
	consumer := payment.NewConsumer(store, service, messaging.RetryPolicy{MaxAttempts: 3}).WithRetryQueue(retryQueue)

	relay := payment.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return messaging.RunRelay(ctx, pool, publisher, messaging.RelayConfig{
			BatchSize: settings.Outbox.BatchSize, PollInterval: settings.Outbox.PollInterval,
		})
	})
	consumerWorker := payment.NewWorkerLifecycle(processContext, func(ctx context.Context) error {
		return payment.RunConsumer(ctx, consumerChannel, retryRouter, paymentQueue, consumer,
			payment.DeliveryRouting{
				RetryExchange: retryExchange, RetryRoutingKey: retryRoute,
				DeadExchange: deadExchange, DeadRoutingKey: deadRoute,
			})
	})

	server := httpx.NewServer(httpx.ServerConfig{
		Service:  settings.Service,
		Instance: settings.Instance,
		Addr:     settings.HTTP.Addr,
		Handler:  payment.NewHandler(service, store),
		Ready: payment.NewRuntimeReadinessWithDependencies(
			pool.Ping, payment.CombinePublisherAvailability(publisher, retryRouter),
			connection.IsClosed, nil, relay, consumerWorker),
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
			runError = fmt.Errorf("run payment outbox relay: %w", relay.ErrIfStopped())
		}
	case <-consumerWorker.Done():
		if signalContext.Err() == nil {
			runError = fmt.Errorf("run payment consumer: %w", consumerWorker.ErrIfStopped())
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

func declarePaymentConsumerTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(eventExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare payment event exchange: %w", err)
	}
	if err := channel.ExchangeDeclare(deadExchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare payment dead-letter exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(deadQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare payment dead-letter queue: %w", err)
	}
	if err := channel.QueueBind(deadQueue, "#", deadExchange, false, nil); err != nil {
		return fmt.Errorf("bind payment dead-letter queue: %w", err)
	}
	if err := channel.ExchangeDeclare(retryExchange, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare payment retry exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(retryQueue, true, false, false, false, amqp.Table{
		"x-message-ttl":             int32(2000),
		"x-dead-letter-exchange":    eventExchange,
		"x-dead-letter-routing-key": retryReturnRoute,
	}); err != nil {
		return fmt.Errorf("declare payment retry queue: %w", err)
	}
	if err := channel.QueueBind(retryQueue, retryRoute, retryExchange, false, nil); err != nil {
		return fmt.Errorf("bind payment retry queue: %w", err)
	}
	if _, err := channel.QueueDeclare(paymentQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare payment workflow queue: %w", err)
	}
	if err := channel.QueueBind(paymentQueue, retryReturnRoute, eventExchange, false, nil); err != nil {
		return fmt.Errorf("bind payment retry return: %w", err)
	}
	for _, binding := range paymentEventBindings() {
		if err := channel.QueueBind(paymentQueue, binding, eventExchange, false, nil); err != nil {
			return fmt.Errorf("bind payment workflow queue %s: %w", binding, err)
		}
	}
	return nil
}

func paymentEventBindings() []string {
	return []string{
		"order.placed.v1",
		"order.cancelled.v1",
		"payment.refund-requested.v1",
	}
}
