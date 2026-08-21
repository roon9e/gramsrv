# Multi-stage Dockerfile for all telesrv microservices
# Stage 1: Build all binaries using Go 1.25 on Alpine
FROM golang:1.25-alpine AS builder

WORKDIR /src
RUN apk add --no-cache git

# Download Go dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy full source tree
COPY . .

# Build all microservices and utility binaries with optimizations
ENV CGO_ENABLED=0
RUN mkdir -p /out && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-edge ./cmd/telesrv-edge && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-core ./cmd/telesrv-core && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-egress ./cmd/telesrv-egress && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-file ./cmd/telesrv-file && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-sfu ./cmd/telesrv-sfu && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-admin ./cmd/telesrv-admin && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-update ./cmd/telesrv-update && \
    go build -trimpath -ldflags="-s -w" -o /out/telesrv-ton ./cmd/telesrv-ton && \
    go build -trimpath -ldflags="-s -w" -o /out/telegramloginkeygen ./cmd/telegramloginkeygen

# Stage 2: Minimal runtime environment with ffmpeg, openssl, and certificates
FROM alpine:3.21

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    curl \
    openssl \
    ffmpeg

WORKDIR /app

# Copy binaries into system PATH
COPY --from=builder /out/* /usr/local/bin/

# Copy entrypoint script
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Default working directories for persistent state and configs
RUN mkdir -p /app/data /app/configs

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["telesrv-edge"]
