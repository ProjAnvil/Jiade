#!/usr/bin/env bash
# Verify a payment-service request trace reaches Jaeger.
#
# Sends a GET through the gateway with an X-Request-ID, then polls Jaeger's
# API for a trace from the payment service containing that request id.
# Requires the observability overlay to be up:
#   make observability
#   make trace-smoke
set -euo pipefail

gateway=${GATEWAY:-http://localhost:18000}
jaeger_network=${JAEGER_NETWORK:-bank-obs}
request_id="trace-smoke-$(date +%s)"

# Send a read-only request that flows through the HTTP middleware (otelhttp)
# so the otel collector receives the span and exports it to Jaeger. A GET to
# the payments list is safe, idempotent, and always reachable via the gateway.
curl -fsS -H "X-Request-ID: ${request_id}" "${gateway}/api/v1/payments" >/dev/null

# Poll Jaeger for up to 30 s for a trace that carries the request id. The
# payment service records it as an attribute on the root HTTP span.
for _ in $(seq 1 30); do
  if docker run --rm --network "${jaeger_network}" curlimages/curl:8.10.1 \
    -fsS "http://jaeger:16686/api/traces?service=payment&limit=20" |
    jq -e --arg request_id "${request_id}" '.. | strings | select(. == $request_id)' >/dev/null
  then
    echo "trace for ${request_id} found in Jaeger"
    exit 0
  fi
  sleep 1
done

echo "payment trace for ${request_id} did not reach Jaeger" >&2
echo "(ensure the services export OTLP to otel-collector:4317 —" >&2
echo " the observability stack is up but the application trace exporter" >&2
echo " must be wired for traces to flow)" >&2
exit 1
