FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=core-banking
RUN CGO_ENABLED=0 go build -o /app ./cmd/${CMD}

FROM alpine:latest
COPY --from=build /app /app
ENTRYPOINT ["/app"]
