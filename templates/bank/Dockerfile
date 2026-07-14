FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /core-banking ./cmd/core-banking

FROM alpine:3.19
COPY --from=build /core-banking /core-banking
EXPOSE 8080
ENTRYPOINT ["/core-banking"]
