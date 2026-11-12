package mq

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// brokerURL returns the broker under test, defaulting to the local compose address.
func brokerURL() string {
	if u := os.Getenv("DCN_TEST_AMQP_URL"); u != "" {
		return u
	}
	return "amqp://guest:guest@127.0.0.1:5672/"
}

// dialOrSkip probes the broker with a short-timeout dial and skips the test
// when it is not reachable. Dial retries for up to 60s, so it must only run
// against a broker that already answers.
func dialOrSkip(t *testing.T) *Conn {
	t.Helper()
	probe, err := amqp.DialConfig(brokerURL(), amqp.Config{
		Dial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout(network, addr, 2*time.Second)
		},
	})
	if err != nil {
		t.Skipf("amqp broker not reachable (%v); skipping", err)
	}
	_ = probe.Close()

	c := Dial(brokerURL())
	t.Cleanup(func() { _ = c.conn.Close() })
	return c
}

// uniqueSuffix avoids collisions with leftovers from earlier runs.
func uniqueSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 10)
}

func TestPublishConsumeRoundtrip(t *testing.T) {
	c := dialOrSkip(t)
	suffix := uniqueSuffix()
	exchange := "t_test_" + suffix
	queue := "q_test_" + suffix

	c.DeclareTopicExchange(exchange)
	c.DeclareQueue(queue)
	c.Bind(queue, exchange, "#")

	body := []byte(fmt.Sprintf(`{"run":%q}`, suffix))
	if err := c.Publish(exchange, "test", body); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := make(chan []byte, 1)
	c.Consume(queue, func(b []byte) error {
		select {
		case got <- b:
			return nil
		default:
			return fmt.Errorf("unexpected extra delivery: %s", b)
		}
	})

	select {
	case b := <-got:
		if string(b) != string(body) {
			t.Fatalf("received body %q, want %q", b, body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestConsumeNackRequeues(t *testing.T) {
	c := dialOrSkip(t)
	suffix := uniqueSuffix()
	exchange := "t_test_" + suffix
	queue := "q_test_" + suffix

	c.DeclareTopicExchange(exchange)
	c.DeclareQueue(queue)
	c.Bind(queue, exchange, "#")

	body := []byte(fmt.Sprintf(`{"run":%q}`, suffix))
	if err := c.Publish(exchange, "test", body); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var mu sync.Mutex
	var seen [][]byte
	c.Consume(queue, func(b []byte) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, b)
		if len(seen) == 1 {
			// First delivery fails, forcing a nack with requeue.
			return errors.New("transient infrastructure error")
		}
		return nil
	})

	// Poll until the requeued delivery arrives.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("handler saw %d deliveries, want >= 2 (nack must requeue)", len(seen))
	}
	if string(seen[0]) != string(body) || string(seen[1]) != string(body) {
		t.Fatalf("requeued bodies %q, %q; want %q twice", seen[0], seen[1], body)
	}
}

func TestDeclareFanoutExchange(t *testing.T) {
	c := dialOrSkip(t)
	c.DeclareFanoutExchange("f_test_" + uniqueSuffix())
}
