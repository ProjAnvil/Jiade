#!/usr/bin/env bash
set -euo pipefail

docker compose -f compose.yaml config --quiet
test "$(docker compose -f compose.yaml config --services | grep -c -- '-db$')" -eq 7
test "$(docker compose -f compose.yaml config --format json |
  jq '[.services | to_entries[] | select(.value.ports != null) | .key] == ["traefik"]')" = "true"
rg -n 'replicas: 2' compose.yaml
