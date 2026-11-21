#!/usr/bin/env bash
# Static topology contract: WeBank-style security zones (dmz/sf/core/mgmt)
# x IDC fault domains (idc1/idc2), cross-deployed primary databases,
# dual-homed DCN apps, dcn04 expansion profile
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

check "six networks dmz/sf/core/mgmt/idc1/idc2 exist" \
  '.networks | keys | contains(["dmz", "sf", "core", "mgmt", "idc1", "idc2"])'
check "dcn01-db only in idc1" \
  '.services["dcn01-db"].networks | keys == ["idc1"]'
check "dcn03-db only in idc1" \
  '.services["dcn03-db"].networks | keys == ["idc1"]'
check "dcn02-db only in idc2 (cross-deployed primary)" \
  '.services["dcn02-db"].networks | keys == ["idc2"]'
check "dcn04-db only in idc2" \
  '.services["dcn04-db"].networks | keys == ["idc2"]'
check "DCN databases are not on any business zone" \
  '[.services["dcn01-db"], .services["dcn02-db"], .services["dcn03-db"], .services["dcn04-db"]] | all(.networks | has("sf") or has("core") or has("mgmt") | not)'
check "dcn01-app dual-homed idc1+sf" \
  '.services["dcn01-app"].networks | keys | sort == ["idc1", "sf"]'
check "dcn02-app dual-homed idc2+sf" \
  '.services["dcn02-app"].networks | keys | sort == ["idc2", "sf"]'
check "dcn03-app dual-homed idc1+sf" \
  '.services["dcn03-app"].networks | keys | sort == ["idc1", "sf"]'
check "dcn04-app dual-homed idc2+sf" \
  '.services["dcn04-app"].networks | keys | sort == ["idc2", "sf"]'
check "traefik only on dmz+sf (DMZ proxy)" \
  '.services["traefik"].networks | keys | sort == ["dmz", "sf"]'
check "shared services gns/rmb/adm/batch on sf+core" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"]] | all(.networks | keys | sort == ["core", "sf"])'
check "global-zone databases only on core" \
  '[.services["gns-db"], .services["rmb-db"], .services["adm-db"], .services["batch-db"]] | all(.networks | keys == ["core"])'
check "middleware redis/rabbitmq only on sf" \
  '[.services["gns-redis"], .services["dcn-rabbitmq"]] | all(.networks | keys == ["sf"])'
check "prometheus on mgmt+sf (cross-zone scraping)" \
  '.services["prometheus"].networks | keys | sort == ["mgmt", "sf"]'
check "mysqld-exporter reaches idc1+idc2+core from mgmt" \
  '.services["mysqld-exporter"].networks | keys | sort == ["core", "idc1", "idc2", "mgmt"]'
check "redis-exporter on mgmt+sf" \
  '.services["redis-exporter"].networks | keys | sort == ["mgmt", "sf"]'
check "grafana and console only on mgmt" \
  '[.services["grafana"], .services["console"]] | all(.networks | keys == ["mgmt"])'
check "dcn04 is in expansion profile" \
  '.services["dcn04-app"].profiles == ["expansion"] and .services["dcn04-db"].profiles == ["expansion"]'

echo "topology OK"
