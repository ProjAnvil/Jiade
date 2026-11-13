# syntax=docker/dockerfile:1.7
# Commerce seed CLI image.
#
# One-shot job (no HTTP listener, no healthcheck): it generates and verifies
# dev/demo/load scale data against the running topology.
#
# The build context is the workspace root (see compose.yaml): the module
# resolves commerce/lib and commerce/order through relative replace
# directives in go.mod, and order itself replaces commerce/inventory, so the
# context must include lib/, inventory/, order/, and the seed directory.
#
# Pinned builder: golang:1.22 (matches seed/go.mod `go 1.22`).
# Runtime: gcr.io/distroless/static-debian12:nonroot (UID 65532).

ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot

FROM ${GO_IMAGE} AS builder
ENV GOWORK=off
WORKDIR /src/seed

# Cache module downloads independently of source changes. The relative
# replace targets (../lib, ../inventory, ../order) must exist for
# `go mod download`, so their go.mod files are copied first.
COPY lib/go.mod lib/go.sum /src/lib/
COPY inventory/go.mod inventory/go.sum /src/inventory/
COPY order/go.mod order/go.sum /src/order/
COPY seed/go.mod seed/go.sum /src/seed/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the dependency modules and the seed module.
COPY lib/ /src/lib/
COPY inventory/ /src/inventory/
COPY order/ /src/order/
COPY seed/ /src/seed/

# Reproducible build flags: strip the path, disable CGO, enable VCS stamping
# only when .git is present (it is not in the build context).
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -ldflags="-s -w" -o /out/seed ./cmd/seed

FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.title="commerce-seed" \
      org.opencontainers.image.source="templates/commerce" \
      org.opencontainers.image.description="Commerce seed CLI"

# Copy the seed binary. No wget layer: the CLI has no HTTP health endpoints.
COPY --from=builder /out/seed /usr/local/bin/service

# Distroless static image runs as UID 65532 by default; make it explicit.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
