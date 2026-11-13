# syntax=docker/dockerfile:1.7
# Build context is the workspace root (compose.yaml passes
# dockerfile: seed/Dockerfile): the seed module resolves every sibling module
# (contracts, lib, all seven services) through relative replace directives in
# go.mod, so the context must include all of them. db/migrations is copied
# before seed/ so the rarely changing SQL sits on an earlier layer.
FROM golang:1.22-alpine AS build
ENV GOWORK=off
WORKDIR /src/seed
COPY contracts/ /src/contracts/
COPY lib/ /src/lib/
COPY core-banking/ /src/core-banking/
COPY customer/ /src/customer/
COPY payment/ /src/payment/
COPY reward/ /src/reward/
COPY risk/ /src/risk/
COPY loan/ /src/loan/
COPY wealth/ /src/wealth/
COPY db/migrations/ /src/db/migrations/
COPY seed/ /src/seed/
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/service ./cmd/seed

FROM alpine:3.20
COPY --from=build /out/service /usr/local/bin/service
# cmd/seed reads db/migrations/*.sql at runtime. Keep migrations in the
# immutable image and use /app as the deterministic relative-path root.
COPY db/migrations /app/db/migrations
WORKDIR /app
EXPOSE 8080 9090
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/service"]
