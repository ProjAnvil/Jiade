#!/usr/bin/env bash
# Verify a payment-service request trace reaches Jaeger with a rich set
# of spans covering each layer of the saga.
#
# Submits a payment workflow with a known request id, then polls
# Jaeger's API (reached via the bank-obs docker network — the host port
# is intentionally NOT published) for a trace that:
#   - carries the request id (REST entry span), AND
#   - contains at least one span from each of:
#       * customer or core gRPC (Preparation reads)
#       * messaging publish / consume (saga command dispatch)
#       * workflow Action (engine AuthorizeRisk / PlaceFundsHold /
#         PostLedgerTransfer)
#
# Requires the observability overlay AND the smoke overlay:
#   make observability
#   make smoke        # applies BANK_TEST_FAILURES_ENABLED
#   make trace-smoke
#
# The smoke overlay is required so the payment can be created with a
# deterministic idempotency key; the trace smoke does NOT depend on a
# failure outcome (it uses the plain success path), but the overlay
# keeps trace-smoke runnable as part of the smoke suite.
set -euo pipefail

gateway=${GATEWAY:-http://localhost:18000}
jaeger_network=${JAEGER_NETWORK:-bank-obs}
request_id="trace-smoke-$(date +%s)"
idem_key="trace-smoke-${request_id}"

# Submit a payment workflow carrying the request id. The workflow runs
# the happy path (risk approve → hold → transfer → succeeded); the
# trace therefore fans out across REST, customer/core gRPC, messaging,
# and workflow Action spans. We do NOT need to wait for the workflow to
# complete — Jaeger receives spans as they are emitted, so polling for
# the trace is sufficient.
body=$(printf '{"payer_customer_id":"%s","payer_account_no":"%s","payee_account_no":"%s","currency":"CNY","amount_minor":5000}' \
  "${PAYER_CUSTOMER_ID:-C0000001}" \
  "${PAYER_ACCOUNT_NO:-A000000001}" \
  "${PAYEE_ACCOUNT_NO:-A000000002}")
if ! curl -fsS --retry 15 --retry-delay 1 --retry-connrefused \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: ${idem_key}" \
  -H "X-Request-ID: ${request_id}" \
  -d "$body" \
  "${gateway}/api/v1/payments/workflows" >/dev/null; then
  echo "trace-smoke: failed to submit payment workflow" >&2
  exit 1
fi

# Poll Jaeger for up to 60 s for a trace carrying the request id. Once
# found, assert the trace contains at least one span from each layer by
# inspecting the operation names and service names.
for _ in $(seq 1 60); do
  trace_json=$(docker run --rm --network "${jaeger_network}" curlimages/curl:8.10.1 \
    -fsS "http://jaeger:16686/api/traces?service=payment&limit=20" 2>/dev/null || echo "")
  if [ -n "$trace_json" ] && echo "$trace_json" | \
      jq -e --arg rid "$request_id" \
        '.. | objects | select(.tags? != null) | .tags[] | select(.key=="request.id" or .key=="x-request-id") | select(.value==$rid)' \
      >/dev/null 2>&1; then
    # Found the trace; now verify span coverage. A rich saga trace has:
    #   - an HTTP span (already implied by the request id match)
    #   - a gRPC span to customer or core-banking
    #   - a messaging span (publish or consume)
    #   - a workflow Action span
    missing=""
    # gRPC: any span whose operation name contains "grpc" or whose
    # tags include rpc.system=grpc. The otelgrpc instrumentation uses
    # operation names like "*.Server" or "*.Client" and tags
    # rpc.system=grpc.
    if ! echo "$trace_json" | jq -e \
        '.. | objects | (.operationName? // "") | test("grpc|\\.Server|\\.Client")' \
        >/dev/null 2>&1 && \
       ! echo "$trace_json" | jq -e \
        '.. | objects | select(.key?=="rpc.system") | select(.value?=="grpc")' \
        >/dev/null 2>&1; then
      missing+="grpc "
    fi
    # messaging: any span tagged messaging.system or with an operation
    # name containing "publish"/"consume"/"send"/"process".
    if ! echo "$trace_json" | jq -e \
        '.. | objects | select(.key?=="messaging.system")' \
        >/dev/null 2>&1 && \
       ! echo "$trace_json" | jq -e \
        '.. | objects | (.operationName? // "") | test("publish|consume|send|process|\\.amqp")' \
        >/dev/null 2>&1; then
      missing+="messaging "
    fi
    # workflow Action: any span whose operation name contains
    # "workflow.action" (the engine's Action span) or "action".
    if ! echo "$trace_json" | jq -e \
        '.. | objects | (.operationName? // "") | test("workflow.action|action\\.|saga")' \
        >/dev/null 2>&1; then
      missing+="workflow-action "
    fi
    if [ -z "$missing" ]; then
      echo "trace for ${request_id} found in Jaeger with REST+gRPC+messaging+workflow spans"
      exit 0
    fi
    echo "trace-smoke: trace for ${request_id} found but missing spans:${missing}" >&2
    # Re-poll — Jaeger may still be receiving spans from in-flight
    # actions. The polling budget gives late spans time to arrive.
  fi
  sleep 1
done

echo "trace-smoke: trace for ${request_id} did not satisfy span coverage within 60s" >&2
echo "(ensure the services export OTLP to otel-collector:4317 —" >&2
echo " the observability stack is up but the application trace exporter" >&2
echo " must be wired for traces to flow)" >&2
exit 1
