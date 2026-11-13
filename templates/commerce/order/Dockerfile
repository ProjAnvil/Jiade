# syntax=docker/dockerfile:1.7
# Commerce order service image.
#
# The build context is the workspace root (see compose.yaml): the module
# resolves the shared lib and the inventory module (a legacy direct
# dependency used by the order package's decode-contract tests and declared
# in order/go.mod) through relative replace directives, so the context must
# include lib/, inventory/, and the order directory.
#
# Pinned builder: golang:1.22 (matches order/go.mod `go 1.22`).
# Runtime: gcr.io/distroless/static-debian12:nonroot (UID 65532).
#
# A static busybox wget is layered in from `busybox:1.36-musl` so the compose
# healthcheck can probe /livez without shipping a full shell.

ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG BUSYBOX_IMAGE=busybox:1.36-musl

FROM ${GO_IMAGE} AS builder
ENV GOWORK=off
WORKDIR /src/order

# Cache module downloads independently of source changes. The relative
# replace targets (../lib, ../inventory) must exist for `go mod download`,
# so their go.mod files are copied first.
COPY lib/go.mod lib/go.sum /src/lib/
COPY inventory/go.mod inventory/go.sum /src/inventory/
COPY order/go.mod order/go.sum /src/order/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the shared lib, the inventory module, and the service module.
COPY lib/ /src/lib/
COPY inventory/ /src/inventory/
COPY order/ /src/order/

# Reproducible build flags: strip the path, disable CGO, enable VCS stamping
# only when .git is present (it is not in the build context).
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -ldflags="-s -w" -o /out/order ./cmd/order

# Static wget for the healthcheck. busybox:1.36-musl wget is a fully static
# binary that runs in a distroless image.
FROM ${BUSYBOX_IMAGE} AS wget-source
# no-op stage; we only need /bin/wget from this image.

FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.title="commerce-order" \
      org.opencontainers.image.source="templates/commerce" \
      org.opencontainers.image.description="Commerce order service"

# Copy the service binary and a static wget for the healthcheck.
COPY --from=builder /out/order /usr/local/bin/service
COPY --from=wget-source /bin/wget /usr/local/bin/wget

# The health, /readyz, and /livez endpoints live on HTTP_ADDR. Default :8080
# matches the service-side default and the compose-exposed Traefik rule.
EXPOSE 8080

# Distroless static image runs as UID 65532 by default; make it explicit.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
