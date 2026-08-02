#!/usr/bin/env bash
# DCN 架构仿真 · 端到端验收
# gate 1  DCN 内本地转账
# gate 2  跨 DCN 转账（RMB 总事务）
# gate 3  爆炸半径（dcn02-db 宕机 + 迟到回执再补偿）
# gate 4  协调者崩溃恢复（事务续跑）
# gate 5  子事务幂等（重复投递）
# gate 6  在线扩容（dcn04）
# gate 7  ADM 全局汇总与核对
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

# 失败中途退出时尽量恢复环境
trap 'docker start dcn02-db >/dev/null 2>&1; docker start dcn02-app >/dev/null 2>&1; true' EXIT

balance() { curl -sf "$1/accounts/$2/balance" | jq -r '.balance'; }

# assert_delta <before> <after> <delta> <desc>：before - after == delta（数值比较）
assert_delta() {
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" --arg d "$3" \
    '((($a|tonumber) - ($b|tonumber) - ($d|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$4" || fail "$4 (before=$1 after=$2 want-delta=$3)"
}

assert_equal() { # <a> <b> <desc>：a == b（数值比较）
  local r
  r=$(jq -n --arg a "$1" --arg b "$2" \
    '((($a|tonumber) - ($b|tonumber)) | . * .) < 0.000001')
  [ "$r" = "true" ] && pass "$3" || fail "$3 ($1 != $2)"
}

wait_tx() { # <txId> <limit_sec> → 打印最终状态
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

echo "== Gate 1: DCN 内转账（本地事务）=="
b1=$(balance $DCN01 1001); b2=$(balance $DCN01 1002)
curl -sf -X POST "$GATEWAY/dcn/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"100.00"}' >/dev/null \
  && pass "本地转账请求成功（经网关）" || fail "本地转账请求失败（经网关）"
assert_delta "$b1" "$(balance $DCN01 1001)" 100 "1001 扣款 100"
assert_delta "$b2" "$(balance $DCN01 1002)" -100 "1002 入账 100"

echo "== Gate 2: 跨 DCN 转账（RMB 总事务）=="
b1=$(balance $DCN01 1001); b2=$(balance $DCN02 2001)
curl -sf -X POST "$GATEWAY/dcn/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":2001,"amount":"50.00"}' >/dev/null \
  && pass "跨 DCN 转账请求成功（经网关）" || fail "跨 DCN 转账请求失败（经网关）"
assert_delta "$b1" "$(balance $DCN01 1001)" 50 "1001 扣款 50"
assert_delta "$b2" "$(balance $DCN02 2001)" -50 "2001 入账 50"

echo "== Gate 3: 爆炸半径（docker stop dcn02-db）=="
pre1001=$(balance $DCN01 1001); pre2001=$(balance $DCN02 2001); pre3001=$(balance $DCN03 3001)
docker stop dcn02-db >/dev/null
# 3a. dcn01 本地交易不受影响
curl -sf -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"10.00"}' >/dev/null \
  && pass "dcn02 宕机时 dcn01 本地转账成功" || fail "dcn02 宕机影响 dcn01 本地转账"
# 3b. 涉及 dcn02 的跨单元交易：明确报错且总事务 COMPENSATED
G3TX="verify-g3-$$"
g3code=$(curl -s -o /tmp/dcn-g3-resp.json -w '%{http_code}' -X POST "$DCN01/transfer" \
  -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G3TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"20.00\"}")
[ "$g3code" = "502" ] && pass "故障交易明确报错 (HTTP 502)" || fail "故障交易 HTTP 状态=$g3code，期望 502"
st=$(wait_tx "$G3TX" 30)
[ "$st" = "COMPENSATED" ] && pass "故障交易被逆序补偿 (COMPENSATED)" || fail "故障交易状态=$st，期望 COMPENSATED"
want1001=$(jq -nr --arg a "$pre1001" '(($a|tonumber) - 10) | tostring')
assert_equal "$want1001" "$(balance $DCN01 1001)" "1001 仅承担 3a 的本地转账，故障交易扣款已冲正"
# 3c. dcn03 不受影响
assert_equal "$pre3001" "$(balance $DCN03 3001)" "dcn03 余额不受影响"
# 3d. 恢复后：迟到的 CREDIT 回执触发再补偿，2001 回到 gate 前余额
docker start dcn02-db >/dev/null
wait_url "$DCN02/healthz" 90 && pass "dcn02 恢复" || fail "dcn02 未恢复"
ok=false
for _ in $(seq 60); do
  cur=$(balance $DCN02 2001 2>/dev/null || echo "")
  r=$(jq -n --arg a "$cur" --arg b "$pre2001" \
    '((($a|tonumber?) - ($b|tonumber)) | . * .) < 0.000001' 2>/dev/null || echo false)
  [ "$r" = "true" ] && { ok=true; break; }
  sleep 1
done
$ok && pass "迟到回执再补偿：2001 余额最终恢复" || fail "2001 余额未恢复 (=$cur, want $pre2001)"

echo "== Gate 4: 协调者崩溃恢复（事务续跑）=="
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
wait_url "$DCN02/healthz" 90 || fail "dcn02-app 未恢复"
st=$(wait_tx "$G4TX" 60)
[ "$st" = "COMMITTED" ] && pass "协调者重启后事务续跑成功 (COMMITTED)" || fail "事务状态=$st，期望 COMMITTED"
r=$(jq -n --arg a "$s1" --arg b "$s2" --arg c "$(balance $DCN01 1001)" --arg d "$(balance $DCN02 2001)" \
  '(((($a|tonumber) + ($b|tonumber)) - (($c|tonumber) + ($d|tonumber))) | . * .) < 0.000001')
[ "$r" = "true" ] && pass "两库余额合计不变" || fail "两库余额合计发生变化"

echo "== Gate 5: 子事务幂等（重复投递）=="
G5TX="verify-g5-$$"
curl -s -X POST "$DCN01/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G5TX\",\"fromId\":1001,\"toId\":2001,\"amount\":\"50.00\"}" >/dev/null
st=$(wait_tx "$G5TX" 30)
[ "$st" = "COMMITTED" ] || fail "gate5 前置转账未提交 (=$st)"
pre=$(balance $DCN01 1001)
docker exec dcn-rabbitmq rabbitmqadmin -u dcn -p dcn123 publish \
  exchange=rmb.steps routing_key=step.dcn01 payload_encoding=string \
  payload="{\"txId\":\"$G5TX\",\"stepNo\":1,\"action\":\"DEBIT\",\"accountId\":1001,\"amount\":\"50.00\"}" \
  >/dev/null 2>&1 && pass "重复子事务已投递" || fail "rabbitmqadmin 投递失败"
sleep 3
assert_equal "$pre" "$(balance $DCN01 1001)" "重复投递不产生重复扣款"

echo "== Gate 6: 在线扩容（dcn04）=="
docker compose --profile expansion up -d --build dcn04-db dcn04-app >/dev/null
wait_url "$DCN04/healthz" 120 && pass "dcn04 单元就绪" || fail "dcn04 未就绪"
curl -sf -X POST "$GNS/routes" -H 'Content-Type: application/json' \
  -d '{"dcn":"dcn04","segStart":4000,"segEnd":4999,"endpoint":"http://dcn04-app:8080"}' >/dev/null \
  && pass "dcn04 号段注册成功" || fail "dcn04 号段注册失败"
acct=$(curl -sf -X POST "$GNS/accounts" -H 'Content-Type: application/json' \
  -d "{\"name\":\"Expand-1\",\"initBalance\":\"500.00\",\"requestId\":\"verify-g6\"}")
newid=$(echo "$acct" | jq -r '.accountId')
[ "$newid" -ge 4000 ] 2>/dev/null && [ "$newid" -le 4999 ] 2>/dev/null \
  && pass "新开户落入 4xxx ($newid)" || fail "新开户未落入 4xxx ($acct)"
G6TX="verify-g6-$$"
curl -s -X POST "$DCN04/transfer" -H 'Content-Type: application/json' \
  -d "{\"txId\":\"$G6TX\",\"fromId\":$newid,\"toId\":1001,\"amount\":\"30.00\"}" >/dev/null
st=$(wait_tx "$G6TX" 30)
[ "$st" = "COMMITTED" ] && pass "跨新旧单元转账成功 (COMMITTED)" || fail "跨新旧单元转账状态=$st"

echo "== Gate 7: ADM 全局汇总与核对 =="
sleep 3 # 容忍汇总链路秒级延迟（仿真 T+x）
sum=$(curl -sf "$ADM/report/summary")
accs=$(echo "$sum" | jq -r '.accounts')
[ "$accs" -ge 7 ] 2>/dev/null && pass "全局账户数 $accs >= 7" || fail "全局账户数异常: $sum"
rec=$(curl -sf "$ADM/reconcile")
[ "$(echo "$rec" | jq -r '.consistent')" = "true" ] \
  && pass "ADM 汇总与各 DCN 实时余额核对一致" || fail "核对不一致: $rec"

echo
if [ "$FAILED" -ne 0 ]; then
  echo "VERIFY FAILED"
  exit 1
fi
echo "VERIFY OK"
