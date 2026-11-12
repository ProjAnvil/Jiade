// Package mq wraps RabbitMQ: idempotent topology declaration, persistent
// publishing, and manual-ack consuming.
package mq

import (
	"context"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Conn holds the connection and a mutex-guarded publish channel.
type Conn struct {
	conn *amqp.Connection
	mu   sync.Mutex
	pub  *amqp.Channel
}

// Dial establishes a connection (waits up to 60s, tolerating compose startup order).
func Dial(url string) *Conn {
	var conn *amqp.Connection
	var err error
	for i := 0; i < 60; i++ {
		conn, err = amqp.Dial(url)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("amqp dial: %v", err)
	}
	pub, err := conn.Channel()
	if err != nil {
		log.Fatalf("amqp channel: %v", err)
	}
	return &Conn{conn: conn, pub: pub}
}

// DeclareTopicExchange idempotently declares a durable topic exchange.
func (c *Conn) DeclareTopicExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "topic", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareFanoutExchange idempotently declares a durable fanout exchange.
func (c *Conn) DeclareFanoutExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "fanout", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareQueue idempotently declares a durable queue.
func (c *Conn) DeclareQueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.pub.QueueDeclare(name, true, false, false, false, nil); err != nil {
		log.Fatalf("declare queue %s: %v", name, err)
	}
}

// Bind idempotently binds a queue to an exchange.
func (c *Conn) Bind(queue, exchange, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.QueueBind(queue, key, exchange, false, nil); err != nil {
		log.Fatalf("bind %s -> %s: %v", queue, exchange, err)
	}
}

// Publish publishes a message in persistent mode.
func (c *Conn) Publish(exchange, key string, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pub.PublishWithContext(context.Background(), exchange, key, false, false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})
}

// Consume consumes a queue: ack only when the handler returns nil; on error,
// nack and requeue after a delay (for retryable infrastructure errors such as
// a temporarily unavailable database).
func (c *Conn) Consume(queue string, handler func([]byte) error) {
	ch, err := c.conn.Channel()
	if err != nil {
		log.Fatalf("consume channel: %v", err)
	}
	msgs, err := ch.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("consume %s: %v", queue, err)
	}
	go func() {
		for d := range msgs {
			if err := handler(d.Body); err != nil {
				log.Printf("handler error on %s, requeue: %v", queue, err)
				_ = d.Nack(false, true)
				time.Sleep(time.Second) // avoid a hot loop
				continue
			}
			_ = d.Ack(false)
		}
	}()
}
