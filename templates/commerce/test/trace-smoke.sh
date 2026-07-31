#!/usr/bin/env bash
set -euo pipefail

gateway=${GATEWAY:-http://localhost:18100}
jaeger_network=${JAEGER_NETWORK:-commerce-obs}
request_id="trace-smoke-$(date +%s)"

curl -fsS -H "X-Request-ID: ${request_id}" "${gateway}/api/v1/products?limit=1" >/dev/null
for _ in $(seq 1 30); do
  if docker run --rm --network "${jaeger_network}" curlimages/curl:8.10.1 \
    -fsS "http://jaeger:16686/api/traces?service=catalog&limit=20" |
    jq -e --arg request_id "${request_id}" '.. | strings | select(. == $request_id)' >/dev/null
  then
    exit 0
  fi
  sleep 1
done
echo "catalog trace for ${request_id} did not reach Jaeger" >&2
exit 1
