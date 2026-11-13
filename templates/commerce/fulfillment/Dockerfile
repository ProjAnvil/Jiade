# syntax=docker/dockerfile:1.7
# Commerce fulfillment service image.
#
# The build context is the workspace root (see compose.yaml): the module
# resolves the shared lib through a relative replace directive in go.mod, so
# the context must include both lib/ and the service directory.
#
# Pinned builder: golang:1.22 (matches fulfillment/go.mod `go 1.22`).
# Runtime: gcr.io/distroless/static-debian12:nonroot (UID 65532).
#
# A static busybox wget is layered in from `busybox:1.36-musl` so the compose
# healthcheck can probe /livez without shipping a full shell.

ARG GO_IMAGE=golang:1.22-bookworm
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot
ARG BUSYBOX_IMAGE=busybox:1.36-musl

FROM ${GO_IMAGE} AS builder
ENV GOWORK=off
WORKDIR /src/fulfillment

# Cache module downloads independently of source changes. The relative
# replace target (../lib) must exist for `go mod download`, so its go.mod
# is copied first.
COPY lib/go.mod lib/go.sum /src/lib/
COPY fulfillment/go.mod fulfillment/go.sum /src/fulfillment/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the shared lib and the service module.
COPY lib/ /src/lib/
COPY fulfillment/ /src/fulfillment/

# Reproducible build flags: strip the path, disable CGO, enable VCS stamping
# only when .git is present (it is not in the build context).
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go build -ldflags="-s -w" -o /out/fulfillment ./cmd/fulfillment

# Static wget for the healthcheck. busybox:1.36-musl wget is a fully static
# binary that runs in a distroless image.
FROM ${BUSYBOX_IMAGE} AS wget-source
# no-op stage; we only need /bin/wget from this image.

FROM ${RUNTIME_IMAGE}
LABEL org.opencontainers.image.title="commerce-fulfillment" \
      org.opencontainers.image.source="templates/commerce" \
      org.opencontainers.image.description="Commerce fulfillment service"

# Copy the service binary and a static wget for the healthcheck.
COPY --from=builder /out/fulfillment /usr/local/bin/service
COPY --from=wget-source /bin/wget /usr/local/bin/wget

# The health, /readyz, and /livez endpoints live on HTTP_ADDR. Default :8080
# matches the service-side default and the compose-exposed Traefik rule.
EXPOSE 8080

# Distroless static image runs as UID 65532 by default; make it explicit.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
