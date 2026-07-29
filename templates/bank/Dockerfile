# syntax=docker/dockerfile:1.7
FROM golang:1.22-alpine AS build
ARG SERVICE=core-banking
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

FROM alpine:3.20
COPY --from=build /out/service /usr/local/bin/service
# BusyBox wget is retained solely for the in-container Compose healthcheck.
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
