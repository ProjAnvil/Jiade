// Package main is the payment service entry: read-only HTTP API + the
// payment-transfer saga driver (workflow engine + result-event consumer +
// outbox relay + recovery loop).
//
// Composition (Task 7):
//   - HTTP server: /api/v1/payments/... routes (read-only + workflow create /
//     status / reverse) and /readyz, /livez, /metrics.
//   - Workflow engine + registry: the payment-transfer v1 definition.
//   - Outbox relay: drains outbox_message rows the engine's AppendOutbox wrote
//     (saga dispatch commands) and publishes them to RabbitMQ.
//   - Result-event consumer: subscribes to the payment.results queue and feeds
//     each envelope to Engine.ApplyResult, then syncs payment_intent.status.
//   - Recovery scheduler: claims preparing/timed-out instances and advances
//     them via Engine.Prepare / Engine.Redispatch.
//
// All four (HTTP, relay, consumer, recovery) run under runx.Serve; if any one
// exits, the internal context is cancelled and the process shuts down. The
// /readyz probe pings the DB — a worker crash surfaces as a process restart by
// the orchestrator.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "bank/gen/bank/core/v1"
	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/payment"
	"bank/internal/payment/api"
	"bank/internal/payment/repo"
	"bank/internal/payment/service"
	"bank/internal/payment/workflows"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/runx"
	"bank/internal/platform/serviceclient"
	"bank/internal/platform/workflow"

	amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbName := getenv("DB_NAME", "pay_db")
	db, err := pg.Open(dbName)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", dbName, err)
	}
	defer db.Close()

	// Start retry: pay_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		return fmt.Errorf("连 %s 失败（请先 make up 再 make seed）: %w", dbName, err)
	}
	coreConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CORE_BANKING_GRPC_TARGET", "dns:///core-banking:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("连接 core-banking gRPC 失败: %w", err)
	}
	defer coreConn.Close()
	customerConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CUSTOMER_GRPC_TARGET", "dns:///customer:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("连接 customer gRPC 失败: %w", err)
	}
	defer customerConn.Close()

	accountReader := serviceclient.NewAccountReader(corev1.NewAccountQueryServiceClient(coreConn))
	customerReader := serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn))

	// --- Workflow engine + definition registry ---
	registry := workflow.NewRegistry()
	preparation := workflows.NewPreparation(customerReader, accountReader)
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		return fmt.Errorf("register payment-transfer definition: %w", err)
	}
	wfStore := workflow.NewPostgresStore(db)
	engine := workflow.NewEngine(wfStore, registry, workflow.EngineConfig{})
	recovery := workflow.NewRecovery(wfStore, engine, registry, workflow.RecoveryConfig{
		Owner: getenv("INSTANCE_ID", "payment-1"),
	})

	// --- Payment runtime: intent repo + atomic starter ---
	intentRepo := payment.NewPaymentIntentRepo(db)
	statusRepo := payment.NewInstanceStatusRepo(db)
	starter := payment.NewWorkflowStarter(db, intentRepo, wfStore, time.Now)
	workflowAPI := &paymentWorkflowAPI{
		starter: starter,
		intents: intentRepo,
		newUUID: newWorkflowUUID,
	}

	// --- HTTP handlers: read-only service + workflow REST API ---
	handlers := &api.Handlers{
		Svc:      service.NewPaymentService(repo.NewPaymentRepo(db, accountReader, customerReader)),
		Workflows: workflowAPI,
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")

	// Readiness: the HTTP /readyz probe pings the DB. The four workers run
	// under runx.Serve; if any one exits, the internal context is cancelled so
	// HTTP shuts down — the orchestrator restarts the process and /readyz
	// therefore covers every worker.
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "payment",
		Instance: getenv("INSTANCE_ID", "payment-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	// --- Result-event consumer + outbox relay + recovery ---
	amqpURL := getenv("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	resultQueue := getenv("PAYMENT_RESULT_QUEUE", "payment.results")
	retryPolicy := messaging.RetryPolicy{
		MaxAttempts:          3,
		RetryRoutingKey:      getenv("PAYMENT_RETRY_KEY", "payment.retry"),
		DeadLetterRoutingKey: getenv("PAYMENT_DLQ_KEY", "payment.dlq"),
	}
	consumer := payment.NewConsumer(db, engine, statusRepo, intentRepo, retryPolicy)

	// Outbox relay: eagerly dial the broker so a broker-down condition fails
	// the process at startup rather than silently skipping event delivery.
	relayConn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("payment outbox relay 连接 broker 失败: %w", err)
	}
	defer relayConn.Close()
	relayCh, err := relayConn.Channel()
	if err != nil {
		return fmt.Errorf("payment outbox relay 打开 channel 失败: %w", err)
	}
	relayPublisher, err := messaging.NewRabbitPublisher(relayCh, "")
	if err != nil {
		return fmt.Errorf("payment outbox relay publisher 失败: %w", err)
	}
	relay := payment.NewOutboxRelay(db, relayPublisher)

	// Workers: each runs under the signal context via runx.Serve. If any one
	// exits, runx.Serve cancels the context so the others and HTTP shut down.
	workers := []runx.Worker{
		// Result-event consumer: feeds saga result events to Engine.ApplyResult.
		runx.WorkerFunc(func(ctx context.Context) error {
			return consumer.Run(ctx, amqpURL, resultQueue)
		}),
		// Outbox relay: publishes workflow dispatch commands.
		runx.WorkerFunc(func(ctx context.Context) error {
			defer relayPublisher.Close()
			return relay.Run(ctx)
		}),
		// Recovery scheduler: advances preparing + timed-out instances.
		runx.WorkerFunc(func(ctx context.Context) error {
			return recovery.Run(ctx)
		}),
	}

	log.Printf("payment HTTP 监听 %s (db=%s), 结果队列=%s", httpAddr, dbName, resultQueue)
	return runx.Serve(signalCtx, srv, nil, 5*time.Second, workers...)
}

// ---------------------------------------------------------------------------
// paymentWorkflowAPI adapts *payment.WorkflowStarter to api.WorkflowAPI.
//
// The adapter lives in the composition root (cmd/payment) because:
//   - api.WorkflowAPI's method signatures use api DTOs, so any concrete impl
//     must import api.
//   - payment.WorkflowStarter must not import api (api imports payment for the
//     sentinel errors; that direction is the dependency the engine enforces).
//   - Therefore the bridge between the two belongs at the wiring point.
//
// The adapter also generates the workflow_id (a fresh UUID per create) and
// marshals the workflows.PrepareInput the engine stores on the instance.
// ---------------------------------------------------------------------------

// Compile-time assertion that *paymentWorkflowAPI satisfies api.WorkflowAPI.
var _ api.WorkflowAPI = (*paymentWorkflowAPI)(nil)

type paymentWorkflowAPI struct {
	starter *payment.WorkflowStarter
	intents *payment.PaymentIntentRepo
	newUUID func() string
}

// Start delegates to WorkflowStarter.StartPayment with a freshly-generated
// workflow id and the marshalled PrepareInput.
func (a *paymentWorkflowAPI) Start(ctx context.Context, req api.StartWorkflowRequest) (api.StartWorkflowResponse, error) {
	workflowID := "wf-" + a.newUUID()

	// Build the engine's PrepareInput from the REST request. PaymentID mirrors
	// the workflow id so downstream services can correlate the payment with the
	// saga instance via the same identifier.
	input := workflows.PrepareInput{
		PaymentID:       workflowID,
		PayerCustomerID: req.PayerCustomerID,
		PayerAccountNo:  req.PayerAccountNo,
		PayeeAccountNo:  req.PayeeAccountNo,
		Currency:        req.Currency,
		AmountMinor:     req.AmountMinor,
	}
	inputBytes, err := json.Marshal(input)
	if err != nil {
		return api.StartWorkflowResponse{}, fmt.Errorf("marshal PrepareInput: %w", err)
	}

	result, err := a.starter.StartPayment(ctx, payment.StartPaymentRequest{
		IdempotencyKey:  req.IdempotencyKey,
		RequestHash:     req.RequestHash,
		PayerCustomerID: req.PayerCustomerID,
		PayerAccountNo:  req.PayerAccountNo,
		PayeeAccountNo:  req.PayeeAccountNo,
		Currency:        req.Currency,
		AmountMinor:     req.AmountMinor,
		Input:           inputBytes,
	}, workflowID)
	if err != nil {
		return api.StartWorkflowResponse{}, err
	}
	return api.StartWorkflowResponse{
		WorkflowID: result.Intent.WorkflowID,
		Status:     string(result.Intent.Status),
		Replayed:   !result.Inserted,
	}, nil
}

// Status loads the payment intent by workflow id and projects it onto the API
// DTO. Returns payment.ErrWorkflowNotFound (mapped to 404 by the handler) when
// the workflow id does not exist.
func (a *paymentWorkflowAPI) Status(ctx context.Context, workflowID string) (api.WorkflowStatusResponse, error) {
	intent, err := a.intents.GetByWorkflowID(ctx, workflowID)
	if err != nil {
		return api.WorkflowStatusResponse{}, err
	}
	return api.WorkflowStatusResponse{
		WorkflowID:      intent.WorkflowID,
		Status:          string(intent.Status),
		IdempotencyKey:  intent.IdempotencyKey,
		PayerCustomerID: intent.PayerCustomerID,
		PayerAccountNo:  intent.PayerAccountNo,
		PayeeAccountNo:  intent.PayeeAccountNo,
		Currency:        intent.Currency,
		AmountMinor:     intent.AmountMinor,
		Reversed:        intent.Reversed,
	}, nil
}

// Reverse marks the payment intent reversed. Triggering actual saga
// compensation (emitting reverse commands to ledger/core) requires engine
// support for externally-initiated compensation of a succeeded instance; that
// is a follow-up task. For now the endpoint records the operator-initiated
// reversal so GET reports reversed=true and status=reversed.
func (a *paymentWorkflowAPI) Reverse(ctx context.Context, workflowID string) (api.ReverseWorkflowResponse, error) {
	if err := a.intents.MarkReversedByWorkflowID(ctx, workflowID); err != nil {
		return api.ReverseWorkflowResponse{}, err
	}
	return api.ReverseWorkflowResponse{
		WorkflowID: workflowID,
		Status:     string(payment.IntentReversed),
	}, nil
}

// newWorkflowUUID generates a v4 UUID hex string (no dashes) suitable as a
// workflow id suffix. It mirrors the unexported workflow.newUUID and
// messaging.newMessageID; a later refactor may expose a shared UUID helper.
func newWorkflowUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Errorf("generate workflow UUID: %w", err))
	}
	raw[6] = raw[6]&0x0f | 0x40 // version 4
	raw[8] = raw[8]&0x3f | 0x80 // variant 10
	return hex.EncodeToString(raw[:])
}

type pinger interface{ Ping() error }

func waitForDB(p pinger, retries int, wait time.Duration) error {
	var err error
	for i := 0; i < retries; i++ {
		if err = p.Ping(); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
