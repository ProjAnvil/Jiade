// Package mq 封装 RabbitMQ：幂等拓扑声明、持久化发布、手动 ack 消费。
package mq

import (
	"context"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Conn 持有连接与一个带锁的发布通道。
type Conn struct {
	conn *amqp.Connection
	mu   sync.Mutex
	pub  *amqp.Channel
}

// Dial 建立连接（最多等待 60s，容忍 compose 启动顺序）。
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

// DeclareTopicExchange 幂等声明 durable topic exchange。
func (c *Conn) DeclareTopicExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "topic", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareFanoutExchange 幂等声明 durable fanout exchange。
func (c *Conn) DeclareFanoutExchange(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.ExchangeDeclare(name, "fanout", true, false, false, false, nil); err != nil {
		log.Fatalf("declare exchange %s: %v", name, err)
	}
}

// DeclareQueue 幂等声明 durable 队列。
func (c *Conn) DeclareQueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.pub.QueueDeclare(name, true, false, false, false, nil); err != nil {
		log.Fatalf("declare queue %s: %v", name, err)
	}
}

// Bind 幂等绑定队列到 exchange。
func (c *Conn) Bind(queue, exchange, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.pub.QueueBind(queue, key, exchange, false, nil); err != nil {
		log.Fatalf("bind %s -> %s: %v", queue, exchange, err)
	}
}

// Publish 以持久化模式发布消息。
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

// Consume 消费队列：handler 返回 nil 才 ack；返回 error 则 nack 并延迟重新入队
// （用于数据库暂不可用等可重试基础设施错误）。
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
				time.Sleep(time.Second) // 避免热循环
				continue
			}
			_ = d.Ack(false)
		}
	}()
}
