package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"
)

func TestRabbitPublisherConsumesReturnedPublishConfirmBeforeNextPublish(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.outcomes = []fakePublishOutcome{
		{returned: true, ack: true},
		{ack: false},
	}
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}

	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "returned") {
		t.Fatalf("first Publish() error=%v, want returned-message error", err)
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "negatively confirmed") {
		t.Fatalf("second Publish() error=%v, want its own nack", err)
	}
	if channel.publishCalls != 2 || channel.closed {
		t.Fatalf("publishCalls=%d closed=%v, want two aligned publishes on open channel", channel.publishCalls, channel.closed)
	}
}

func TestRabbitPublisherNegativeConfirmationIsFailure(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.outcomes = []fakePublishOutcome{{ack: false}}
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "negatively confirmed") {
		t.Fatalf("Publish() error=%v, want nack error", err)
	}
}

func TestRabbitPublisherCancellationRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	ctx, cancel := context.WithCancel(context.Background())
	channel.onPublish = cancel
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(ctx, testEvent()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish() error=%v, want context.Canceled", err)
	} else if !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatalf("Publish() error=%v, want ErrPublisherUnavailable", err)
	}
	if !channel.closed {
		t.Fatal("channel was not retired after ambiguous cancellation")
	}
	if publisher.Available() {
		t.Fatal("publisher still reports available after channel retirement")
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Publish() after retirement error=%v", err)
	}
}

func TestRabbitPublisherReportsAvailabilityBeforeRetirement(t *testing.T) {
	publisher, err := newRabbitPublisher(newFakeRabbitChannel(), "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if !publisher.Available() {
		t.Fatal("new publisher reports unavailable")
	}
}

func TestRabbitPublisherConfirmationClosureRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.closeConfirmOnPublish = true
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "confirmation") || !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatalf("Publish() error=%v, want confirmation closure error", err)
	}
	if !channel.closed {
		t.Fatal("channel was not retired after confirmation stream closure")
	}
}

func TestRabbitPublisherReturnClosureRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.closeReturnOnPublish = true
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "return") || !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatalf("Publish() error=%v, want return closure error", err)
	}
	if !channel.closed {
		t.Fatal("channel was not retired after return stream closure")
	}
}

func TestRabbitPublisherWrongConfirmationSequenceRetiresChannel(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.wrongSequenceOnPublish = true
	channel.outcomes = []fakePublishOutcome{{ack: true}}
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), testEvent()); err == nil || !strings.Contains(err.Error(), "correlation lost") || !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatalf("Publish() error=%v, want correlation error", err)
	}
	if !channel.closed {
		t.Fatal("channel was not retired after sequence mismatch")
	}
}

func TestRelayRecordsTerminalRabbitFailureThenStopsBatch(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.closeConfirmOnPublish = true
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	store := &relayStore{claims: []outboxClaim{{Event: testEvent()}, {Event: testEvent()}}}

	_, err = relayOnce(context.Background(), store, publisher, RelayConfig{BatchSize: 2})
	if !errors.Is(err, ErrPublisherUnavailable) {
		t.Fatalf("relayOnce() error=%v, want ErrPublisherUnavailable", err)
	}
	if store.failed != 1 || channel.publishCalls != 1 {
		t.Fatalf("failed=%d publishCalls=%d, want one recorded failure and immediate stop", store.failed, channel.publishCalls)
	}
	if len(store.failureErrors) != 1 || !errors.Is(store.failureErrors[0], ErrPublisherUnavailable) {
		t.Fatalf("recorded failure=%v, want ErrPublisherUnavailable", store.failureErrors)
	}
}

func TestRelayContinuesBatchAfterAlignedReturnAndNack(t *testing.T) {
	channel := newFakeRabbitChannel()
	channel.outcomes = []fakePublishOutcome{
		{returned: true, ack: true},
		{ack: false},
	}
	publisher, err := newRabbitPublisher(channel, "commerce.events")
	if err != nil {
		t.Fatal(err)
	}
	store := &relayStore{claims: []outboxClaim{{Event: testEvent()}, {Event: testEvent()}}}

	if _, err := relayOnce(context.Background(), store, publisher, RelayConfig{BatchSize: 2}); err != nil {
		t.Fatalf("relayOnce() error=%v, want event-local failures only", err)
	}
	if store.failed != 2 || channel.publishCalls != 2 || channel.closed {
		t.Fatalf("failed=%d publishCalls=%d closed=%v, want two recorded event failures on usable publisher", store.failed, channel.publishCalls, channel.closed)
	}
}

type fakePublishOutcome struct {
	returned bool
	ack      bool
}

type fakeRabbitChannel struct {
	nextSeq                uint64
	confirms               chan amqp.Confirmation
	returns                chan amqp.Return
	outcomes               []fakePublishOutcome
	publishCalls           int
	closed                 bool
	closeConfirmOnPublish  bool
	closeReturnOnPublish   bool
	wrongSequenceOnPublish bool
	onPublish              func()
}

func newFakeRabbitChannel() *fakeRabbitChannel {
	return &fakeRabbitChannel{nextSeq: 1}
}

func (channel *fakeRabbitChannel) Confirm(bool) error { return nil }
func (channel *fakeRabbitChannel) NotifyPublish(confirm chan amqp.Confirmation) chan amqp.Confirmation {
	channel.confirms = confirm
	return confirm
}
func (channel *fakeRabbitChannel) NotifyReturn(returned chan amqp.Return) chan amqp.Return {
	channel.returns = returned
	return returned
}
func (channel *fakeRabbitChannel) GetNextPublishSeqNo() uint64 { return channel.nextSeq }
func (channel *fakeRabbitChannel) PublishWithContext(_ context.Context, _, _ string, _, _ bool, message amqp.Publishing) error {
	channel.publishCalls++
	sequence := channel.nextSeq
	channel.nextSeq++
	if channel.onPublish != nil {
		channel.onPublish()
	}
	if channel.closeConfirmOnPublish {
		close(channel.confirms)
		channel.closeConfirmOnPublish = false
		return nil
	}
	if channel.closeReturnOnPublish {
		close(channel.returns)
		channel.closeReturnOnPublish = false
		return nil
	}
	if len(channel.outcomes) == 0 {
		return nil
	}
	outcome := channel.outcomes[0]
	channel.outcomes = channel.outcomes[1:]
	if outcome.returned {
		channel.returns <- amqp.Return{ReplyCode: 312, ReplyText: "NO_ROUTE", MessageId: message.MessageId}
	}
	if channel.wrongSequenceOnPublish {
		sequence++
		channel.wrongSequenceOnPublish = false
	}
	channel.confirms <- amqp.Confirmation{DeliveryTag: sequence, Ack: outcome.ack}
	return nil
}
func (channel *fakeRabbitChannel) Close() error {
	channel.closed = true
	return nil
}
