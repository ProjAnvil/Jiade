package messaging

// Direct tests for routeReplacement's guard branches (nil router, missing
// exchange/routing key) and its success path, which returns the original
// cause after the confirmed route + source ack.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func routedDelivery() amqp.Delivery {
	return amqp.Delivery{
		Body:           []byte(`{"body":true}`),
		Headers:        amqp.Table{"x-retry": int64(0)},
		Acknowledger:   recordingAcknowledger{recorder: &txRecorder{}},
		DeliveryMode:   amqp.Persistent,
		CorrelationId:  "corr-1",
		MessageId:      "m-1",
		Timestamp:      time.Now(),
	}
}

func TestRouteReplacement_NilRouterFailsClosed(t *testing.T) {
	cause := errors.New("handle failed")
	err := routeReplacement(context.Background(), cause, routedDelivery(), nil, "bank.retry", "rk")
	if err == nil || !strings.Contains(err.Error(), "confirmed routing dependency is required") {
		t.Fatalf("want missing-router error joined with cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "handle failed") {
		t.Errorf("original cause must be preserved: %v", err)
	}
}

func TestRouteReplacement_RequiresExchangeAndKey(t *testing.T) {
	router := &recordingRouter{recorder: &txRecorder{}}
	err := routeReplacement(context.Background(), errors.New("cause"), routedDelivery(), router, "", "rk")
	if err == nil || !strings.Contains(err.Error(), "routing exchange and key are required") {
		t.Fatalf("want missing-exchange error, got %v", err)
	}
	err = routeReplacement(context.Background(), errors.New("cause"), routedDelivery(), router, "bank.retry", "")
	if err == nil || !strings.Contains(err.Error(), "routing exchange and key are required") {
		t.Fatalf("want missing-key error, got %v", err)
	}
}

func TestRouteReplacement_SuccessReturnsCauseAndAcks(t *testing.T) {
	router := &recordingRouter{recorder: &txRecorder{}}
	cause := errors.New("handle failed")
	delivery := routedDelivery()
	err := routeReplacement(context.Background(), cause, delivery, router, "bank.retry", "payment.retry")
	if !errors.Is(err, cause) {
		t.Fatalf("success must return the original cause, got %v", err)
	}
	if router.exchange != "bank.retry" || router.key != "payment.retry" {
		t.Errorf("routed to %s/%s", router.exchange, router.key)
	}
	if string(router.message.Body) != `{"body":true}` {
		t.Errorf("body must be copied verbatim, got %s", router.message.Body)
	}
	if router.message.Headers == nil || router.message.Headers["x-retry"] != int64(0) {
		t.Errorf("headers must be cloned, got %#v", router.message.Headers)
	}
}

func TestRouteReplacement_RouterErrorJoins(t *testing.T) {
	router := &recordingRouter{recorder: &txRecorder{}, err: errors.New("broker down")}
	err := routeReplacement(context.Background(), errors.New("cause"), routedDelivery(), router, "bank.retry", "payment.retry")
	if err == nil || !strings.Contains(err.Error(), "broker down") || !strings.Contains(err.Error(), "cause") {
		t.Fatalf("want joined errors, got %v", err)
	}
}
