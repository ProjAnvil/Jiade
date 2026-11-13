# syntax=docker/dockerfile:1.7
# Build context is the workspace root (compose.yaml passes
# dockerfile: payment/Dockerfile): the module resolves the generated contracts
# (module bank) and the shared lib through relative replace directives in
# go.mod, so the context must include contracts/, lib/ and this directory.
FROM golang:1.22-alpine AS build
ENV GOWORK=off
WORKDIR /src/payment
COPY contracts/ /src/contracts/
COPY lib/ /src/lib/
COPY payment/ /src/payment/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/service ./cmd/payment

FROM alpine:3.20
COPY --from=build /out/service /usr/local/bin/service
# BusyBox wget is retained solely for the in-container Compose healthcheck.
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
