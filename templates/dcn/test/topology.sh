#!/usr/bin/env bash
# 静态拓扑契约：三网络、主库交叉部署、DCN 应用双网卡、DCN 库不进全局区、dcn04 扩容 profile
set -euo pipefail
cd "$(dirname "$0")/.."

cfg=$(docker compose --profile expansion config --format json)

check() { # <描述> <jq 断言>
  if echo "$cfg" | jq -e "$2" >/dev/null; then
    echo "PASS: $1"
  else
    echo "FAIL: $1"
    exit 1
  fi
}

check "三网络 idc1/idc2/global-net 存在" \
  '.networks | keys | contains(["global-net", "idc1", "idc2"])'
check "dcn01-db 仅在 idc1" \
  '.services["dcn01-db"].networks | keys == ["idc1"]'
check "dcn03-db 仅在 idc1" \
  '.services["dcn03-db"].networks | keys == ["idc1"]'
check "dcn02-db 仅在 idc2（主库交叉部署）" \
  '.services["dcn02-db"].networks | keys == ["idc2"]'
check "dcn01-app 双网卡 idc1+global-net" \
  '.services["dcn01-app"].networks | keys | sort == ["global-net", "idc1"]'
check "dcn02-app 双网卡 idc2+global-net" \
  '.services["dcn02-app"].networks | keys | sort == ["global-net", "idc2"]'
check "dcn03-app 双网卡 idc1+global-net" \
  '.services["dcn03-app"].networks | keys | sort == ["global-net", "idc1"]'
check "dcn04-app 双网卡 idc2+global-net" \
  '.services["dcn04-app"].networks | keys | sort == ["global-net", "idc2"]'
check "dcn04-db 仅在 idc2" \
  '.services["dcn04-db"].networks | keys == ["idc2"]'
check "DCN 数据库均不接入 global-net" \
  '[.services["dcn01-db"], .services["dcn02-db"], .services["dcn03-db"], .services["dcn04-db"]] | all(.networks | has("global-net") | not)'
check "全局区服务不接入任何 IDC 网络" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"], .services["batch-db"]] | all(.networks | keys == ["global-net"])'
check "dcn04 在 expansion profile" \
  '.services["dcn04-app"].profiles == ["expansion"] and .services["dcn04-db"].profiles == ["expansion"]'

echo "topology OK"
