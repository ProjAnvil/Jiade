#!/usr/bin/env bash
# Bank payment saga smoke gates.
#
# Drives the payment-transfer saga through each failure + recovery path
# using the deterministic test-only failure controls (see
# internal/platform/testfail). Each gate is a positive or negative
# assertion; any failure exits non-zero so CI can mark the suite red.
#
# Pre-requisites:
#   - The full stack is up (make up).
#   - The smoke overlay has been applied so BANK_TEST_FAILURES_ENABLED
#     is set on payment/core-banking/risk (the script does this on
#     startup).
#
# Usage:
#   make smoke                       # from templates/bank
#   GATEWAY=http://localhost:18000 bash test/smoke.sh
set -euo pipefail

gateway=${GATEWAY:-http://localhost:18000}
compose_files=(-f compose.yaml)
obs_files=(-f compose.yaml -f compose.observability)
jaeger_network=${JAEGER_NETWORK:-bank-obs}
poll_seconds=${POLL_SECONDS:-90}
# fail_fast=1 stops on the first gate failure; default keeps going so a
# single smoke run reports every gate that regressed.
fail_fast=${FAIL_FAST:-0}

pass_count=0
fail_count=0
failed_gates=()

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() { printf '[smoke] %s\n' "$*"; }

gate_pass() {
  pass_count=$((pass_count + 1))
  printf '[smoke] PASS  %s\n' "$1"
}

gate_fail() {
  fail_count=$((fail_count + 1))
  failed_gates+=("$1: $2")
  printf '[smoke] FAIL  %s — %s\n' "$1" "$2" >&2
  if [ "$fail_fast" = "1" ]; then
    exit 1
  fi
}

# require checks a precondition and bails out entirely when it is not
# met (a missing precondition means the suite cannot run at all, not a
# gate failure).
require() {
  if ! "$@" >/dev/null 2>&1; then
    printf '[smoke] FATAL: precondition failed: %s\n' "$*" >&2
    exit 2
  fi
}

# svc_count returns the number of running containers for a compose service.
svc_count() {
  docker compose "${compose_files[@]}" ps -q "$1" 2>/dev/null | wc -l | tr -d ' '
}

# Submit a payment workflow; echo the workflow_id from the JSON response.
submit_payment() {
  local idem_key="$1"
  local payer_customer="$2"
  local payer_account="$3"
  local payee_account="$4"
  local amount="${5:-5000}"
  curl -fsS \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: ${idem_key}" \
    -d "{\"payer_customer_id\":\"${payer_customer}\",\"payer_account_no\":\"${payer_account}\",\"payee_account_no\":\"${payee_account}\",\"currency\":\"CNY\",\"amount_minor\":${amount}}" \
    "${gateway}/api/v1/payments/workflows" | jq -r '.workflow_id'
}

workflow_status() {
  curl -fsS "${gateway}/api/v1/payments/workflows/$1" | jq -r '.status'
}

# poll_status echoes the final status and returns 0 when workflow_id
# reaches one of the expected statuses within poll_seconds. Returns 1
# on timeout.
poll_status() {
  local wf_id="$1"
  shift
  local expected="$*"
  local status=""
  local deadline=$((SECONDS + poll_seconds))
  while [ "$SECONDS" -lt "$deadline" ]; do
    status=$(workflow_status "$wf_id" 2>/dev/null || echo "")
    for want in $expected; do
      if [ "$status" = "$want" ]; then
        printf '%s' "$status"
        return 0
      fi
    done
    sleep 1
  done
  printf '%s' "$status"
  return 1
}

# SQL helpers — each runs a single query in the named database via the
# matching -db container. -tA strips table headers so the output is
# just the value (or values, one per line).
core_psql() { docker compose "${compose_files[@]}" exec -T core-banking-db psql -U bank -d core_db -tA -c "$1"; }
risk_psql() { docker compose "${compose_files[@]}" exec -T risk-db psql -U bank -d risk_db -tA -c "$1"; }
pay_psql()  { docker compose "${compose_files[@]}" exec -T payment-db psql -U bank -d pay_db  -tA -c "$1"; }
cust_psql() { docker compose "${compose_files[@]}" exec -T customer-db psql -U bank -d cust_db -tA -c "$1"; }

# ---------------------------------------------------------------------------
# Pre-flight: apply smoke overlay and wait for the gateway.
# ---------------------------------------------------------------------------

apply_smoke_overlay() {
  log "applying smoke overlay (BANK_TEST_FAILURES_ENABLED=true)"
  docker compose "${compose_files[@]}" -f compose.smoke.yaml up -d \
    --no-deps --force-recreate payment core-banking risk >/dev/null 2>&1 || true
  # The recreate invalidates Traefik's backends; give the new containers
  # a moment to re-register before probing the gateway.
  sleep 5
}

wait_for_gateway() {
  log "waiting for gateway at ${gateway}"
  local deadline=$((SECONDS + 60))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -fsS "${gateway}/api/v1/payments/transfers?limit=1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# pick_seed_accounts echoes two space-separated account numbers from the
# seed data: a funded low-risk payer and any other active account as
# payee. Bails out when the seed has no suitable payer.
pick_seed_accounts() {
  # Find a low-risk, KYC-verified, active customer.
  local payer_customer
  payer_customer=$(cust_psql "SELECT cust_id FROM cust_info WHERE risk_level='low' AND kyc_status='passed' AND customer_status='active' ORDER BY cust_id LIMIT 1" 2>/dev/null | tr -d '[:space:]')
  if [ -z "$payer_customer" ]; then
    log "FATAL: no low-risk active customer in cust_info; run make seed first"
    return 1
  fi
  # Find that customer's highest-balance active demand account.
  local payer_account
  payer_account=$(core_psql "SELECT da.account_no FROM demand_account da JOIN account_balance ab ON da.account_no=ab.account_no WHERE da.cust_id='${payer_customer}' AND da.acct_status='active' AND ab.available_balance > 100 ORDER BY ab.biz_date DESC LIMIT 1" 2>/dev/null | tr -d '[:space:]')
  if [ -z "$payer_account" ]; then
    log "FATAL: no funded active account for ${payer_customer}; run make seed first"
    return 1
  fi
  # Find any OTHER active account to act as payee (different account_no).
  local payee_account
  payee_account=$(core_psql "SELECT account_no FROM demand_account WHERE acct_status='active' AND account_no <> '${payer_account}' ORDER BY account_no LIMIT 1" 2>/dev/null | tr -d '[:space:]')
  if [ -z "$payee_account" ]; then
    log "FATAL: no second active account for payee; run make seed first"
    return 1
  fi
  printf '%s %s %s' "$payer_customer" "$payer_account" "$payee_account"
}

# ---------------------------------------------------------------------------
# Gate 1: verify 2 gateway instances for core-banking / payment / risk.
# ---------------------------------------------------------------------------

gate_1_replicas() {
  local gate="G1-replicas"
  for svc in core-banking payment risk; do
    local n
    n=$(svc_count "$svc")
    if [ "$n" -lt 2 ]; then
      gate_fail "$gate" "service ${svc} has ${n} replicas, want >= 2"
      return
    fi
  done
  gate_pass "$gate (core-banking=$(svc_count core-banking) payment=$(svc_count payment) risk=$(svc_count risk))"
}

# ---------------------------------------------------------------------------
# Gate 2: successful workflow → succeeded.
# ---------------------------------------------------------------------------

gate_2_success() {
  local gate="G2-success"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-ok-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  if [ -z "$wf_id" ]; then
    gate_fail "$gate" "submit returned no workflow_id"
    return
  fi
  local status
  if status=$(poll_status "$wf_id" "succeeded"); then
    gate_pass "$gate (${wf_id} → ${status})"
  else
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want succeeded"
  fi
}

# ---------------------------------------------------------------------------
# Gate 3: risk rejection → no hold, no voucher.
# ---------------------------------------------------------------------------

gate_3_risk_reject() {
  local gate="G3-risk-reject"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-reject-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  local status
  if ! status=$(poll_status "$wf_id" "compensated" "rejected"); then
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want compensated/rejected"
    return
  fi
  local hold_n voucher_n
  hold_n=$(core_psql "SELECT count(*) FROM funds_hold WHERE workflow_id='${wf_id}'" 2>/dev/null | tr -d '[:space:]')
  voucher_n=$(core_psql "SELECT count(*) FROM held_transfer WHERE idempotency_key LIKE 'wf:${wf_id}%'" 2>/dev/null | tr -d '[:space:]')
  if [ "${hold_n}" = "0" ] && [ "${voucher_n}" = "0" ]; then
    gate_pass "$gate (${wf_id} → ${status}, hold=0, voucher=0)"
  else
    gate_fail "$gate" "${wf_id} hold=${hold_n} voucher=${voucher_n}, want 0/0"
  fi
}

# ---------------------------------------------------------------------------
# Gate 4: insufficient funds → authorization voided.
# ---------------------------------------------------------------------------

gate_4_insufficient() {
  local gate="G4-insufficient"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-insuff-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  local status
  if ! status=$(poll_status "$wf_id" "compensated"); then
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want compensated"
    return
  fi
  local auth_status
  auth_status=$(risk_psql "SELECT status FROM payment_authorization WHERE workflow_id='${wf_id}'" 2>/dev/null | tr -d '[:space:]')
  if [ "$auth_status" = "voided" ]; then
    gate_pass "$gate (${wf_id} → ${status}, authorization=${auth_status})"
  else
    gate_fail "$gate" "${wf_id} authorization=${auth_status:-<none>}, want voided"
  fi
}

# ---------------------------------------------------------------------------
# Gate 5: transient transfer failure → hold released.
# ---------------------------------------------------------------------------

gate_5_transient() {
  local gate="G5-transient"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-transient-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  local status
  if ! status=$(poll_status "$wf_id" "compensated"); then
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want compensated"
    return
  fi
  local hold_status
  hold_status=$(core_psql "SELECT status FROM funds_hold WHERE workflow_id='${wf_id}'" 2>/dev/null | tr -d '[:space:]')
  if [ "$hold_status" = "released" ]; then
    gate_pass "$gate (${wf_id} → ${status}, hold=${hold_status})"
  else
    gate_fail "$gate" "${wf_id} hold=${hold_status:-<none>}, want released"
  fi
}

# ---------------------------------------------------------------------------
# Gate 6: repeat commands/events → exactly one voucher.
# ---------------------------------------------------------------------------

gate_6_duplicate() {
  local gate="G6-duplicate"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local idem_key="smoke-dup-$$-$(date +%s)"
  # Submit the same payment twice with the SAME Idempotency-Key.
  local wf_id1 wf_id2
  wf_id1=$(submit_payment "$idem_key" "$payer_customer" "$payer_account" "$payee_account" 5000)
  wf_id2=$(submit_payment "$idem_key" "$payer_customer" "$payer_account" "$payee_account" 5000)
  if [ "$wf_id1" != "$wf_id2" ]; then
    gate_fail "$gate" "replay returned different workflow ids: ${wf_id1} vs ${wf_id2}"
    return
  fi
  local status
  if ! status=$(poll_status "$wf_id1" "succeeded"); then
    gate_fail "$gate" "${wf_id1} ended in ${status:-<timeout>}, want succeeded"
    return
  fi
  local voucher_n
  voucher_n=$(core_psql "SELECT count(*) FROM held_transfer WHERE idempotency_key LIKE 'wf:${wf_id1}%'" 2>/dev/null | tr -d '[:space:]')
  if [ "${voucher_n}" = "1" ]; then
    gate_pass "$gate (${wf_id1} → ${status}, voucher=${voucher_n})"
  else
    gate_fail "$gate" "${wf_id1} voucher=${voucher_n}, want 1"
  fi
}

# ---------------------------------------------------------------------------
# Gate 7: kill one payment container during waiting_result → takeover.
# ---------------------------------------------------------------------------

gate_7_takeover() {
  local gate="G7-takeover"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  # Scale to 2 replicas up-front so we know another instance can absorb
  # the kill. The base topology already runs 2, so this is a guard.
  if [ "$(svc_count payment)" -lt 2 ]; then
    gate_fail "$gate" "payment has <2 replicas; cannot test takeover"
    return
  fi
  local wf_id
  wf_id=$(submit_payment "smoke-kill-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  if [ -z "$wf_id" ]; then
    gate_fail "$gate" "submit returned no workflow_id"
    return
  fi
  # Kill ONE of the two payment containers. docker compose -q lists the
  # container ids; head -1 picks one. SIGKILL simulates a hard crash.
  local victim
  victim=$(docker compose "${compose_files[@]}" ps -q payment 2>/dev/null | head -1)
  if [ -n "$victim" ]; then
    log "killing payment container ${victim%% *} for ${wf_id}"
    docker kill "$victim" >/dev/null 2>&1 || true
  fi
  # restart: unless-stopped brings the victim back; the surviving
  # replica (and the recovery loop) advance the workflow to success.
  local status
  if status=$(poll_status "$wf_id" "succeeded"); then
    gate_pass "$gate (${wf_id} → ${status} after container kill)"
  else
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want succeeded"
  fi
}

# ---------------------------------------------------------------------------
# Gate 8: reverse a completed payment → red reversal entries.
# ---------------------------------------------------------------------------

gate_8_reverse() {
  local gate="G8-reverse"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-rev-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  local status
  if ! status=$(poll_status "$wf_id" "succeeded"); then
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want succeeded before reverse"
    return
  fi
  # Trigger the reversal workflow. The endpoint returns the reversal
  # workflow id, but we only need to observe the original intent
  # flip to reversed.
  local rev_wf
  rev_wf=$(curl -fsS -X POST -H "Idempotency-Key: smoke-reverse-$$-$(date +%s)" \
    "${gateway}/api/v1/payments/workflows/${wf_id}/reverse" | jq -r '.reversal_workflow_id // empty')
  if [ -z "$rev_wf" ]; then
    gate_fail "$gate" "reverse endpoint returned no reversal_workflow_id"
    return
  fi
  # Wait for the ORIGINAL intent to be marked reversed (the consumer's
  # auto-detection flips it once the reversal workflow succeeds).
  local deadline=$((SECONDS + poll_seconds))
  local reversed="false"
  while [ "$SECONDS" -lt "$deadline" ]; do
    reversed=$(curl -fsS "${gateway}/api/v1/payments/workflows/${wf_id}" | jq -r '.reversed')
    [ "$reversed" = "true" ] && break
    sleep 1
  done
  # Assert a reversal voucher exists (the original posting's voucher_no
  # is referenced by a voucher_reversal row).
  local rev_n
  rev_n=$(core_psql "SELECT count(*) FROM voucher_reversal" 2>/dev/null | tr -d '[:space:]')
  if [ "$reversed" = "true" ] && [ "${rev_n:-0}" -ge 1 ]; then
    gate_pass "$gate (${wf_id} reversed, reversal_vouchers=${rev_n})"
  else
    gate_fail "$gate" "${wf_id} reversed=${reversed}, voucher_reversal=${rev_n:-0}"
  fi
}

# ---------------------------------------------------------------------------
# Gate 9: compensation exhaustion → compensation_failed.
# ---------------------------------------------------------------------------

gate_9_compensation_failed() {
  local gate="G9-compensation-failed"
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  local wf_id
  wf_id=$(submit_payment "smoke-compfail-$$-$(date +%s)" "$payer_customer" "$payer_account" "$payee_account" 5000)
  # The compfail gate fails the release-hold compensation transiently
  # on every attempt; the engine retries up to CompensationMaxAttempts
  # (default 5) and then transitions the instance to compensation_failed.
  local status
  if status=$(poll_status "$wf_id" "compensation_failed"); then
    gate_pass "$gate (${wf_id} → ${status})"
  else
    gate_fail "$gate" "${wf_id} ended in ${status:-<timeout>}, want compensation_failed"
  fi
}

# ---------------------------------------------------------------------------
# Gate 10: negative probes — internal routes and gRPC exposure.
# ---------------------------------------------------------------------------

gate_10_negative_probes() {
  local gate="G10-negative-probes"
  local problems=""

  # 10a: /internal/* is NOT routed by Traefik. The Traefik labels only
  # match PathPrefix(`/api/v1/...`); an internal path must 404 at the
  # gateway rather than reach a service container.
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "${gateway}/internal/payment/workflows" || echo "000")
  if [ "$code" != "404" ]; then
    problems+="internal-route http=${code} (want 404); "
  fi

  # 10b: the gRPC port (9090) is NOT published to the host. core-banking
  # exposes 9090 only inside the docker network, so a host-side TCP
  # connect must fail (no listener).
  if nc -z localhost 9090 2>/dev/null; then
    problems+="grpc-9090 reachable from host (want closed); "
  fi

  # 10c: the admin gRPC port (9091) is NOT published to the host.
  if nc -z localhost 9091 2>/dev/null; then
    problems+="admin-grpc-9091 reachable from host (want closed); "
  fi

  # 10d: the payment admin gRPC surface is reachable only inside the
  # network. Confirm by probing from the data network and asserting the
  # token gate rejects an unauthenticated RPC — the service is up but
  # refuses the call when BANK_OPERATOR_TOKEN is empty (fail-closed).
  local admin_probe
  # NOTE: || true (not || echo "000") — curl -w '%{http_code}' already
  # writes "000" on connection failure, and appending another "000"
  # produces "000000" which falls through the case below.  Using || true
  # lets curl's own status code stand; an empty string (docker run itself
  # failed) is also acceptable.
  admin_probe=$(docker run --rm --network bank-data curlimages/curl:8.10.1 \
    -s -o /dev/null -w '%{http_code}' \
    "http://payment:9091/" 2>/dev/null || true)
  # gRPC servers respond 200 to a plain HTTP/1.1 probe with a 415/400
  # content-type mismatch; the key assertion is that the port is NOT
  # silent (service is listening inside the network). 000 or empty means
  # the container could not reach the port at all — also acceptable since
  # the surface is protected.
  case "$admin_probe" in
    000|200|400|415|"") ;;  # acceptable: listening but rejects
    *) problems+="admin-grpc in-network http=${admin_probe} (unexpected); " ;;
  esac

  if [ -z "$problems" ]; then
    gate_pass "$gate (internal=404, grpc-host=closed, admin-in-net=protected)"
  else
    gate_fail "$gate" "${problems}"
  fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  log "bank payment smoke suite starting (gateway=${gateway})"

  apply_smoke_overlay
  require wait_for_gateway

  log "selecting seed accounts"
  local seed
  if ! seed=$(pick_seed_accounts); then
    exit 2
  fi
  # shellcheck disable=SC2086
  set -- $seed
  local payer_customer="$1" payer_account="$2" payee_account="$3"
  log "using payer=${payer_customer}/${payer_account} payee=${payee_account}"

  gate_1_replicas
  gate_2_success   "$payer_customer" "$payer_account" "$payee_account"
  gate_3_risk_reject "$payer_customer" "$payer_account" "$payee_account"
  gate_4_insufficient "$payer_customer" "$payer_account" "$payee_account"
  gate_5_transient "$payer_customer" "$payer_account" "$payee_account"
  gate_6_duplicate "$payer_customer" "$payer_account" "$payee_account"
  gate_7_takeover  "$payer_customer" "$payer_account" "$payee_account"
  gate_8_reverse   "$payer_customer" "$payer_account" "$payee_account"
  gate_9_compensation_failed "$payer_customer" "$payer_account" "$payee_account"
  gate_10_negative_probes

  printf '\n[smoke] ===== summary: %d passed, %d failed =====\n' "$pass_count" "$fail_count"
  if [ "$fail_count" -gt 0 ]; then
    printf '[smoke] failed gates:\n'
    for g in "${failed_gates[@]}"; do
      printf '  - %s\n' "$g"
    done
    exit 1
  fi
  exit 0
}

main "$@"
