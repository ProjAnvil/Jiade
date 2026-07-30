#!/usr/bin/env bash
set -euo pipefail

# The seed binary reads db/migrations/*.sql relative to its working directory.
# This static image contract keeps that runtime dependency explicit without
# needing to create Compose volumes for a Docker smoke test.
rg -F 'COPY db/migrations /app/db/migrations' Dockerfile
rg -F 'WORKDIR /app' Dockerfile
rg -F 'ENTRYPOINT ["/usr/local/bin/service"]' Dockerfile
