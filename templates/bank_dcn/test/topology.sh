#!/usr/bin/env bash
# Static topology contract: WeBank-style security zones (dmz/sf/core/mgmt)
# x IDC fault domains (idc1/idc2 app tier, idc1-db/idc2-db DB tier),
# cross-deployed primary databases, ALL app DB traffic via db-proxy,
# dcn04 expansion profile
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

check "eight networks dmz/sf/core/mgmt/idc1/idc2/idc1-db/idc2-db exist" \
  '.networks | keys | contains(["dmz", "sf", "core", "mgmt", "idc1", "idc2", "idc1-db", "idc2-db"])'
check "dcn01-db only in idc1-db" \
  '.services["dcn01-db"].networks | keys == ["idc1-db"]'
check "dcn03-db only in idc1-db" \
  '.services["dcn03-db"].networks | keys == ["idc1-db"]'
check "dcn02-db only in idc2-db (cross-deployed primary)" \
  '.services["dcn02-db"].networks | keys == ["idc2-db"]'
check "dcn04-db only in idc2-db" \
  '.services["dcn04-db"].networks | keys == ["idc2-db"]'
check "dcn01-db-replica only in idc1-db" \
  '.services["dcn01-db-replica"].networks | keys == ["idc1-db"]'
check "global-zone databases only on core" \
  '[.services["gns-db"], .services["rmb-db"], .services["adm-db"], .services["batch-db"]] | all(.networks | keys == ["core"])'
check "NO application joins any DB network" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"], .services["dcn01-app"], .services["dcn02-app"], .services["dcn03-app"], .services["dcn04-app"]] | all(.networks | has("core") or has("idc1-db") or has("idc2-db") | not)'
check "dcn01-app dual-homed idc1+sf" \
  '.services["dcn01-app"].networks | keys | sort == ["idc1", "sf"]'
check "dcn02-app dual-homed idc2+sf" \
  '.services["dcn02-app"].networks | keys | sort == ["idc2", "sf"]'
check "dcn03-app dual-homed idc1+sf" \
  '.services["dcn03-app"].networks | keys | sort == ["idc1", "sf"]'
check "dcn04-app dual-homed idc2+sf" \
  '.services["dcn04-app"].networks | keys | sort == ["idc2", "sf"]'
check "all eight app DSNs go through db-proxy port-per-set" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"], .services["dcn01-app"], .services["dcn02-app"], .services["dcn03-app"], .services["dcn04-app"]] | all(.environment.DB_DSN | contains("tcp(db-proxy:"))'
check "traefik only on dmz+sf (DMZ proxy)" \
  '.services["traefik"].networks | keys | sort == ["dmz", "sf"]'
check "shared services gns/rmb/adm/batch only on sf" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"]] | all(.networks | keys == ["sf"])'
check "middleware redis/rabbitmq only on sf" \
  '[.services["gns-redis"], .services["dcn-rabbitmq"]] | all(.networks | keys == ["sf"])'
check "db keepers on mgmt + all DB networks (3-node raft quorum)" \
  '[.services["db-keeper-1"], .services["db-keeper-2"], .services["db-keeper-3"]] | all(.networks | keys | sort == ["core", "idc1-db", "idc2-db", "mgmt"])'
check "db proxies bridge sf and every DB network" \
  '[.services["db-proxy"], .services["db-proxy-2"]] | all(.networks | keys | sort == ["core", "idc1-db", "idc2-db", "mgmt", "sf"])'
check "db-admin only on mgmt" \
  '.services["db-admin"].networks | keys == ["mgmt"]'
check "prometheus on mgmt+sf (cross-zone scraping)" \
  '.services["prometheus"].networks | keys | sort == ["mgmt", "sf"]'
check "mysqld-exporter reaches idc1-db+idc2-db+core from mgmt" \
  '.services["mysqld-exporter"].networks | keys | sort == ["core", "idc1-db", "idc2-db", "mgmt"]'
check "redis-exporter on mgmt+sf" \
  '.services["redis-exporter"].networks | keys | sort == ["mgmt", "sf"]'
check "grafana and console only on mgmt" \
  '[.services["grafana"], .services["console"]] | all(.networks | keys == ["mgmt"])'
check "dcn04 is in expansion profile" \
  '.services["dcn04-app"].profiles == ["expansion"] and .services["dcn04-db"].profiles == ["expansion"]'
check "dcn01-db has binlog/GTID flags for self-managed replication" \
  '.services["dcn01-db"].command | join(" ") | contains("gtid-mode=ON")'

echo "topology OK"
