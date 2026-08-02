#!/usr/bin/env bash
# Static topology contract: three networks, cross-deployed primary databases, dual-homed DCN apps,
# DCN databases kept off the global zone, dcn04 expansion profile
set -euo pipefail
cd "$(dirname "$0")/.."

cfg=$(docker compose --profile expansion config --format json)

check() { # <description> <jq assertion>
  if echo "$cfg" | jq -e "$2" >/dev/null; then
    echo "PASS: $1"
  else
    echo "FAIL: $1"
    exit 1
  fi
}

check "three networks idc1/idc2/global-net exist" \
  '.networks | keys | contains(["global-net", "idc1", "idc2"])'
check "dcn01-db only in idc1" \
  '.services["dcn01-db"].networks | keys == ["idc1"]'
check "dcn03-db only in idc1" \
  '.services["dcn03-db"].networks | keys == ["idc1"]'
check "dcn02-db only in idc2 (cross-deployed primary)" \
  '.services["dcn02-db"].networks | keys == ["idc2"]'
check "dcn01-app dual-homed idc1+global-net" \
  '.services["dcn01-app"].networks | keys | sort == ["global-net", "idc1"]'
check "dcn02-app dual-homed idc2+global-net" \
  '.services["dcn02-app"].networks | keys | sort == ["global-net", "idc2"]'
check "dcn03-app dual-homed idc1+global-net" \
  '.services["dcn03-app"].networks | keys | sort == ["global-net", "idc1"]'
check "dcn04-app dual-homed idc2+global-net" \
  '.services["dcn04-app"].networks | keys | sort == ["global-net", "idc2"]'
check "dcn04-db only in idc2" \
  '.services["dcn04-db"].networks | keys == ["idc2"]'
check "DCN databases are not on global-net" \
  '[.services["dcn01-db"], .services["dcn02-db"], .services["dcn03-db"], .services["dcn04-db"]] | all(.networks | has("global-net") | not)'
check "global-zone services are not on any IDC network" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"], .services["batch-db"], .services["traefik"], .services["console"]] | all(.networks | keys == ["global-net"])'
check "traefik only on global-net" \
  '.services["traefik"].networks | keys == ["global-net"]'
check "dcn04 is in expansion profile" \
  '.services["dcn04-app"].profiles == ["expansion"] and .services["dcn04-db"].profiles == ["expansion"]'

echo "topology OK"
