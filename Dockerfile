# syntax=docker/dockerfile:1.7

# Raven's factory.
#
# The build stage runs on the builder's own platform (BUILDPLATFORM) and
# cross-compiles for each target (TARGETOS, TARGETARCH): CI builds amd64 and
# arm64 on an amd64 runner, where compiling under QEMU took about two
# minutes for arm64 against twenty seconds natively. The final stage runs no
# command, so arm64 needs no emulation at all. Docker's classic builder
# (compose without the buildx plugin) sets none of the three: BUILDPLATFORM
# defaults to amd64 below, and an empty GOOS and GOARCH build for the host.
ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is reported to core at registration, and goes into the scripts' hello.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=${VERSION}" -o /out/raven-factory ./cmd/raven-factory

# The script is a template built into the factory: no toolchain at run time.
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/raven-factory /app/raven-factory
# Configured entirely from the environment (cmd/raven-factory/main.go). No
# port: it only dials out (to core's RabbitMQ, and core for its factory link).

ENTRYPOINT ["/app/raven-factory"]
