# syntax=docker/dockerfile:1.7

# Raven's factory. No GOARCH: the build stage runs as the target platform
# (QEMU in CI), so the binary always matches the image's architecture.
FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is reported to core at registration, and goes into the scripts' hello.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/raven-factory ./cmd/raven-factory

# The script is a template built into the factory: no toolchain at run time.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/raven-factory /app/raven-factory
# Configured entirely from the environment (cmd/raven-factory/main.go). No
# port: it only dials out (to core's RabbitMQ, and core for its factory link).

ENTRYPOINT ["/app/raven-factory"]
