# Build context is the repository root (see compose.yaml): the module
# resolves the shared lib through relative replace directives in go.mod,
# so the context must include both the service dir and lib/.
FROM golang:1.22-alpine AS builder
ENV GOWORK=off
WORKDIR /src/batch-scheduler
COPY lib/ /src/lib/
COPY batch-scheduler/ /src/batch-scheduler/
RUN CGO_ENABLED=0 go build -o /out/app ./cmd/batch-scheduler

FROM alpine:3.20
COPY --from=builder /out/app /usr/local/bin/app
ENTRYPOINT ["/usr/local/bin/app"]
