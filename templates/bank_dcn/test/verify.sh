#!/usr/bin/env bash
# DCN architecture simulation - end-to-end acceptance
# gate 1  intra-DCN local transfer
# gate 2  cross-DCN transfer (RMB global transaction)
# gate 3  blast radius (dcn02-db outage + late-receipt re-compensation)
# gate 4  coordinator crash recovery (transaction resume)
# gate 5  sub-transaction idempotency (duplicate delivery)
# gate 6  online expansion (dcn04)
# gate 7  ADM global aggregation and reconciliation
# gate 8  end-of-day batch (interest accrual + idempotent rerun + ADM reconciliation)
set -u
cd "$(dirname "$0")/.."

GNS=${GNS:-http://localhost:18080}
GATEWAY=${GATEWAY:-http://localhost:18070}
DCN01=${DCN01:-http://localhost:18081}
DCN02=${DCN02:-http://localhost:18082}
DCN03=${DCN03:-http://localhost:18083}
DCN04=${DCN04:-http://localhost:18084}
RMB=${RMB:-http://localhost:18090}
ADM=${ADM:-http://localhost:18091}

FAILED=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILED=1; }

# Best-effort environment recovery when exiting mid-run on failure
trap 'docker start dcn02-db >/dev/null 2>&1; docker start dcn02-app >/dev/null 2>&1; true' EXIT

balance() { curl -sf "$1/accounts/$2/balance" | jq -r '.balance'; }

# assert_delta <before> <after> <delta> <desc>: before - after == delta (numeric comparison)
assert_delta() {
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" --arg d "$3" \
    '((($a|tonumber) - ($b|tonumber) - ($d|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$4" || fail "$4 (before=$1 after=$2 want-delta=$3)"
}

assert_equal() { # <a> <b> <desc>: a == b (numeric comparison)
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" \
    '((($a|tonumber) - ($b|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$3" || fail "$3 ($1 != $2)"
}

wait_tx() { # <txId> <limit_sec> -> prints the final status
  local txid=$1 s=""
  for _ in $(seq "$2"); do
    s=$(curl -sf "$RMB/transactions/$txid" | jq -r '.status' 2>/dev/null || true)
    [ -n "$s" ] && [ "$s" != "PROCESSING" ] && [ "$s" != "null" ] && { echo "$s"; return; }
    sleep 1
  done
  echo "$s"
}

wait_url() { # <url> <limit_sec>
  for _ in $(seq "$2"); do
    curl -sf "$1" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

echo "== Gate 1: intra-DCN transfer (local transaction) =="
b1=$(balance $DCN01 1001); b2=$(balance $DCN01 1002)
curl -sf -X POST "$GATEWAY/dcn/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"100.00"}' >/dev/null \
  && pass "local transfer succeeded (via gateway)" || fail "local transfer failed (via gateway)"
assert_delta "$b1" "$(balance $DCN01 1001)" 100 "1001 debited 100"
assert_delta "$b2" "$(balance $DCN01 1002)" -100 "1002 credited 100"

echo "== Gate 2: cross-DCN transfer (RMB global transaction) =="
b1=$(balance $DCN01 1001); b2=$(balance $DCN02 2001)
curl -sf -X POST "$GATEWAY/dcn/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":2001,"amount":"50.00"}' >/dev/null \
  && pass "cross-DCN transfer succeeded (via gateway)" || fail "cross-DCN transfer failed (via gateway)"
assert_delta "$b1" "$(balance $DCN01 1001)" 50 "1001 debited 50"
assert_delta "$b2" "$(balance $DCN02 2001)" -50 "2001 credited 50"

echo "== Gate 3: blast radius (docker stop dcn02-db) =="
pre1001=$(balance $DCN01 1001); pre2001=$(balance $DCN02 2001); pre3001=$(balance $DCN03 3001)
docker stop dcn02-db >/dev/null
# 3a. dcn01 local transactions are unaffected
curl -sf -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"10.00"}' >/dev/null \
  && pass "dcn01 local transfer succeeds while dcn02 is down" || fail "dcn02 outage affects dcn01 local transfer"
# 3b. cross-unit transaction involving dcn02: explicit error and global transaction COMPENSATED
G3TX="verify-g3-$$"
g3code=$(curl -s -o /tmp/dcn-g3-resp.json -w '%{http_code}' -X POST "$DCN01/transfer" \
  -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G3TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"20.00\"}")
[ "$g3code" = "502" ] && pass "failed transaction returns explicit error (HTTP 502)" || fail "failed transaction HTTP status=$g3code, expected 502"
st=$(wait_tx "$G3TX" 30)
[ "$st" = "COMPENSATED" ] && pass "failed transaction compensated in reverse order (COMPENSATED)" || fail "failed transaction status=$st, expected COMPENSATED"
want1001=$(jq -nr --arg a "$pre1001" '(($a|tonumber) - 10) | tostring')
assert_equal "$want1001" "$(balance $DCN01 1001)" "1001 only bears gate 3a local transfer; failed transaction debit reversed"
# 3c. dcn03 is unaffected
assert_equal "$pre3001" "$(balance $DCN03 3001)" "dcn03 balance unaffected"
# 3d. after recovery: the late CREDIT receipt triggers re-compensation, returning 2001 to its pre-gate balance
docker start dcn02-db >/dev/null
wait_url "$DCN02/healthz" 90 && pass "dcn02 recovered" || fail "dcn02 not recovered"
ok=false
for _ in $(seq 60); do
  cur=$(balance $DCN02 2001 2>/dev/null || echo "")
  r=$(jq -n --arg a "$cur" --arg b "$pre2001" \
    '((($a|tonumber?) - ($b|tonumber)) | . * .) < 0.000001' 2>/dev/null || echo false)
  [ "$r" = "true" ] && { ok=true; break; }
  sleep 1
done
$ok && pass "late receipt re-compensation: 2001 balance restored" || fail "2001 balance not restored (=$cur, want $pre2001)"

echo "== Gate 4: coordinator crash recovery (transaction resume) =="
s1=$(balance $DCN01 1001); s2=$(balance $DCN02 2001)
G4TX="verify-g4-$$"
docker stop dcn02-app >/dev/null
( curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G4TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"25.00\"}" \
  >/tmp/dcn-g4-resp.json 2>&1 ) &
sleep 1
docker restart -t 1 rmb-coordinator >/dev/null
sleep 2
docker start dcn02-app >/dev/null
wait_url "$DCN02/healthz" 90 || fail "dcn02-app not recovered"
st=$(wait_tx "$G4TX" 60)
[ "$st" = "COMMITTED" ] && pass "transaction resumed after coordinator restart (COMMITTED)" || fail "transaction status=$st, expected COMMITTED"
r=$(jq -n --arg a "$s1" --arg b "$s2" --arg c "$(balance $DCN01 1001)" --arg d "$(balance $DCN02 2001)" \
  '(((($a|tonumber) + ($b|tonumber)) - (($c|tonumber) + ($d|tonumber))) | . * .) < 0.000001')
[ "$r" = "true" ] && pass "combined balance across both units unchanged" || fail "combined balance across both units changed"

echo "== Gate 5: sub-transaction idempotency (duplicate delivery) =="
G5TX="verify-g5-$$"
curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G5TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"50.00\"}" >/dev/null
st=$(wait_tx "$G5TX" 30)
[ "$st" = "COMMITTED" ] || fail "gate5 prerequisite transfer not committed (=$st)"
pre=$(balance $DCN01 1001)
docker exec dcn-rabbitmq rabbitmqadmin -u dcn -p dcn123 publish \
  exchange=rmb.steps routing_key=step.dcn01 payload_encoding=string \
  payload="{\"txId\":\"$G5TX\",\"stepNo\":1,\"action\":\"DEBIT\",\"accountId\":1001,\"amount\":\"50.00\"}" \
  >/dev/null 2>&1 && pass "duplicate sub-transaction delivered" || fail "rabbitmqadmin publish failed"
sleep 3
assert_equal "$pre" "$(balance $DCN01 1001)" "duplicate delivery does not cause double debit"

echo "== Gate 6: online expansion (dcn04) =="
docker compose --profile expansion up -d --build dcn04-db dcn04-app >/dev/null
wait_url "$DCN04/healthz" 120 && pass "dcn04 unit ready" || fail "dcn04 not ready"
curl -sf -X POST "$GNS/routes" -H 'Content-Type: application/json' \
  -d '{"dcn":"dcn04","segStart":4000,"segEnd":4999,"endpoint":"http://dcn04-app:8080"}' >/dev/null \
  && pass "dcn04 segment registered" || fail "dcn04 segment registration failed"
acct=$(curl -sf -X POST "$GNS/accounts" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Expand-1\",\"initBalance\":\"500.00\",\"requestId\":\"verify-g6\"}")
newid=$(echo "$acct" | jq -r '.accountId')
[ "$newid" -ge 4000 ] 2>/dev/null && [ "$newid" -le 4999 ] 2>/dev/null \
  && pass "new account falls in 4xxx range ($newid)" || fail "new account not in 4xxx range ($acct)"
G6TX="verify-g6-$$"
curl -s -X POST "$DCN04/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G6TX\",\"fromId\":$newid,\"toId\":1001,\"amount\":\"30.00\"}" >/dev/null
st=$(wait_tx "$G6TX" 30)
[ "$st" = "COMMITTED" ] && pass "cross new/old unit transfer succeeded (COMMITTED)" || fail "cross new/old unit transfer status=$st"

echo "== Gate 7: ADM global aggregation and reconciliation =="
sleep 3 # tolerate second-level latency in the aggregation pipeline (simulated T+x)
sum=$(curl -sf "$ADM/report/summary")
accs=$(echo "$sum" | jq -r '.accounts')
[ "$accs" -ge 7 ] 2>/dev/null && pass "global account count $accs >= 7" || fail "global account count abnormal: $sum"
rec=$(curl -sf "$ADM/reconcile")
[ "$(echo "$rec" | jq -r '.consistent')" = "true" ] \
  && pass "ADM aggregation reconciles with each DCN's real-time balance" || fail "reconciliation inconsistent: $rec"

echo "== Gate 8: end-of-day batch (interest + idempotency + reconciliation) =="
# Note: gate 6 has expanded dcn04 into an ACTIVE unit, so the interest batch is also
# scheduled to dcn04; this gate therefore sums pre/post balances across all four units (dcn01-04).
BIZDATE=$(date +%F)
pre1=$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')
pre2=$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')
pre3=$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')
pre4=$(curl -sf "$DCN04/internal/balance-sum" | jq -r '.balanceSum')
job=$(curl -sf -X POST "$GATEWAY/batch/jobs/interest" -H 'Content-Type: application/json' \
  -d "{\"bizDate\":\"$BIZDATE\"}")
[ "$(echo "$job" | jq -r '.status')" = "SUCCEEDED" ] \
  && pass "interest batch completed (SUCCEEDED)" || fail "interest batch failed: $job"
interest=$(echo "$job" | jq -r '.totalInterest')
unitsum=$(echo "$job" | jq -r '[.units[].interest | tonumber] | add | . * 100 | round / 100 | tostring')
assert_equal "$unitsum" "$interest" "per-unit interest sum = scheduler aggregated total"
# total balance increase across the four units = total interest
post1=$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')
post2=$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')
post3=$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')
post4=$(curl -sf "$DCN04/internal/balance-sum" | jq -r '.balanceSum')
pre_total=$(jq -nr --arg a "$pre1" --arg b "$pre2" --arg c "$pre3" --arg d "$pre4" \
  '($a|tonumber)+($b|tonumber)+($c|tonumber)+($d|tonumber) | tostring')
post_total=$(jq -nr --arg a "$post1" --arg b "$post2" --arg c "$post3" --arg d "$post4" \
  '($a|tonumber)+($b|tonumber)+($c|tonumber)+($d|tonumber) | tostring')
assert_delta "$post_total" "$pre_total" "$interest" "four-unit balance sum increased = total interest"
# idempotency: re-triggering with the same bizDate does not rerun the job, and balances stay unchanged
job2=$(curl -sf -X POST "$GATEWAY/batch/jobs/interest" -H 'Content-Type: application/json' \
  -d "{\"bizDate\":\"$BIZDATE\"}")
[ "$(echo "$job2" | jq -r '.totalInterest')" = "$interest" ] \
  && pass "re-trigger is idempotent (total unchanged)" || fail "re-trigger result drifted: $job2"
assert_equal "$post_total" "$(jq -nr --arg a "$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')" --arg b "$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')" --arg c "$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')" --arg d "$(curl -sf "$DCN04/internal/balance-sum" | jq -r '.balanceSum')" '($a|tonumber)+($b|tonumber)+($c|tonumber)+($d|tonumber) | tostring')" "no double crediting after re-trigger"
sleep 3
rec=$(curl -sf "$ADM/reconcile")
[ "$(echo "$rec" | jq -r '.consistent')" = "true" ] \
  && pass "ADM reconciliation consistent after batch" || fail "reconciliation inconsistent after batch: $rec"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "VERIFY FAILED"
  exit 1
fi
echo "VERIFY OK"
