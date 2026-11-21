#!/usr/bin/env bash
# Cloud smoke check for the deployed bank_dcn stack.
#
# Run from inside the VPC (e.g. on the SF node over SSM) after the
# deployment stage:
#   GATEWAY=http://internal-<alb-dns> PROM_URL=http://<mgmt-ip>:9090 ./verify-aws.sh
#
# Checks (mirrors the compose verify gates 1/2/7):
#   1. GNS route table: EXPECT_DCNS units, all ACTIVE
#   2. healthz for gns / rmb / adm / batch through the gateway (strip-prefix path)
#   3. open fixed-idempotent accounts, then one intra-DCN and one cross-DCN
#      transfer (skipped automatically on rerun once accounts exist)
#   4. ADM reconciliation is consistent
#   5. Prometheus is healthy and all EXPECT_TARGETS scrape targets are up
set -euo pipefail

GATEWAY="${GATEWAY:?set GATEWAY to the internal ALB base URL}"
PROM_URL="${PROM_URL:-}"
EXPECT_DCNS="${EXPECT_DCNS:-3}"
EXPECT_TARGETS="${EXPECT_TARGETS:-8}"

PASS=0
FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

jqget() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)"; }

# 1. GNS route table
routes=$(curl -sf --max-time 10 "$GATEWAY/gns/routes") || { fail "GET /gns/routes"; exit 1; }
ndcn=$(echo "$routes" | jqget 'len([r for r in d if r["status"]=="ACTIVE"])')
inactive=$(echo "$routes" | jqget 'len([r for r in d if r["status"]!="ACTIVE"])')
[ "$ndcn" = "$EXPECT_DCNS" ] && [ "$inactive" = "0" ] \
  && pass "GNS routes: $ndcn ACTIVE units" || fail "GNS routes: got $ndcn ACTIVE ($inactive inactive), expected $EXPECT_DCNS"

# 2. healthz through the gateway (strip-prefix routers)
for s in gns rmb adm batch; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$GATEWAY/$s/healthz")
  [ "$code" = "200" ] && pass "$s healthz 200" || fail "$s healthz $code"
done

# 3. accounts + transfers (idempotent: fixed requestIds, rerun-safe)
rec0=$(curl -sf --max-time 10 "$GATEWAY/adm/reconcile") || rec0='{}'
existing=$(echo "$rec0" | jqget 'sum(p.get("accounts",0) for p in d.get("perDcn",[]))')
if [ "$existing" -lt 3 ] 2>/dev/null; then
  for pair in "1001:a" "1002:b" "2001:c"; do
    id="${pair%%:*}"; tag="${pair##*:}"
    out=$(curl -sf --max-time 15 -X POST "$GATEWAY/gns/accounts" \
      -H 'Content-Type: application/json' \
      -d "{\"name\":\"verify-$tag\",\"initBalance\":\"1000.00\",\"requestId\":\"verify-$tag\"}") \
      && echo "opened: $out" || fail "open account $id"
  done
  intra=$(curl -sf --max-time 15 -X POST "$GATEWAY/dcn/transfer" \
    -H 'Content-Type: application/json' \
    -d '{"fromId":1001,"toId":1002,"amount":"100.00"}') \
    && pass "intra-DCN transfer: $intra" || fail "intra-DCN transfer 1001->1002"
  cross=$(curl -sf --max-time 20 -X POST "$GATEWAY/dcn/transfer" \
    -H 'Content-Type: application/json' \
    -d '{"fromId":1001,"toId":2001,"amount":"50.00"}') \
    && pass "cross-DCN transfer (RMB): $cross" || fail "cross-DCN transfer 1001->2001"
  sleep 4
else
  pass "accounts already seeded (totalAccounts=$existing), transfers skipped"
fi

# 4. ADM reconciliation
rec=$(curl -sf --max-time 10 "$GATEWAY/adm/reconcile") || rec=''
consistent=$(echo "${rec:-{\}}" | jqget 'd.get("consistent",False)')
[ "$consistent" = "True" ] \
  && pass "ADM reconcile consistent" || fail "ADM reconcile: $rec"

# 5. Prometheus
if [ -n "$PROM_URL" ]; then
  ph=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$PROM_URL/-/healthy")
  [ "$ph" = "200" ] && pass "prometheus /-/healthy" || fail "prometheus /-/healthy $ph"
  counts=$(curl -sf --retry 2 --max-time 15 "$PROM_URL/api/v1/query?query=up" \
    | jqget '(len([r for r in d["data"]["result"] if r["value"][1] == "1"]), len(d["data"]["result"]))' \
    || echo '(0, 0)')
  nup=$(echo "$counts" | tr -d '() ' | cut -d, -f1)
  nall=$(echo "$counts" | tr -d '() ' | cut -d, -f2)
  [ "$nup" = "$EXPECT_TARGETS" ] && [ "$nall" = "$EXPECT_TARGETS" ] \
    && pass "prometheus targets $nup/$EXPECT_TARGETS up" \
    || fail "prometheus targets: $nup up of $nall (expected $EXPECT_TARGETS)"
else
  echo "SKIP: prometheus (PROM_URL not set)"
fi

echo "----------------------------------------"
echo "verify-aws: $PASS passed, $FAIL failed"
[ "$FAIL" = "0" ]
