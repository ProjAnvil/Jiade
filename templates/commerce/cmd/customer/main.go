package main

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"

	"commerce/internal/customer"
	"commerce/internal/platform/config"
	"commerce/internal/platform/httpx"
	"commerce/internal/platform/postgres"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	processContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	settings, err := config.Load("customer")
	if err != nil {
		return err
	}
	pool, err := postgres.Open(processContext, settings.Database)
	if err != nil {
		return err
	}
	defer pool.Close()
	handler := customer.NewHandler(customer.NewService(customer.NewPostgresStore(pool)))
	server := httpx.NewServer(httpx.ServerConfig{
		Service:           settings.Service,
		Instance:          settings.Instance,
		Addr:              settings.HTTP.Addr,
		Handler:           handler,
		Ready:             pool.Ping,
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
	case <-processContext.Done():
	case err := <-serverError:
		if !httpx.IsClosed(err) {
			runError = err
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), settings.Shutdown.Timeout)
	defer cancel()
	err = server.Shutdown(shutdownContext)
	if runError != nil {
		return errors.Join(runError, err)
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
