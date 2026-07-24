# syntax=docker/dockerfile:1.7
# Commerce service image.
#
# One Dockerfile for all six Go services (catalog, customer, inventory, order,
# payment, fulfillment) plus the seed CLI. The SERVICE build arg selects which
# `cmd/<svc>` binary is produced. The result is a non-root distroless image
# with a read-only root filesystem (read-only is enforced at compose time).
#
# Pinned builder: golang:1.22 (matches templates/commerce/go.mod `go 1.22`).
# Runtime: gcr.io/distroless/static-debian12:nonroot (UID 65532).
#
# A static busybox wget is layered in from `busybox:1.36-musl` so the compose
# healthcheck can probe /livez without shipping a full shell.

ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG BUSYBOX_IMAGE=busybox:1.36-musl

FROM ${GO_IMAGE} AS builder
ARG SERVICE=order
WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the module. The compose build context is templates/commerce.
COPY . .

# Reproducible build flags: strip the path, disable CGO, enable VCS stamping
# only when .git is present (it is not in the build context).
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    test -n "${SERVICE}" && \
    go build -ldflags="-s -w" -o "/out/${SERVICE}" "./cmd/${SERVICE}"

# Static wget for the healthcheck. busybox:1.36-musl wget is a fully static
# binary that runs in a distroless image.
FROM ${BUSYBOX_IMAGE} AS wget-source
# no-op stage; we only need /bin/wget from this image.

FROM ${RUNTIME_IMAGE}
ARG SERVICE=order
ENV SERVICE=${SERVICE}
LABEL org.opencontainers.image.title="commerce-${SERVICE}" \
      org.opencontainers.image.source="templates/commerce" \
      org.opencontainers.image.description="Commerce ${SERVICE} service"

# Copy the service binary and a static wget for the healthcheck.
COPY --from=builder /out/${SERVICE} /usr/local/bin/service
COPY --from=wget-source /bin/wget /usr/local/bin/wget

# The health, /readyz, and /livez endpoints live on HTTP_ADDR. Default :8080
# matches the service-side default and the compose-exposed Traefik rule.
EXPOSE 8080

# Distroless static image runs as UID 65532 by default; make it explicit.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
