# syntax=docker/dockerfile:1
#
# Build context is the repository root (see ../docker-compose.yml).
# Configuration comes from the environment (see ../.env.example).

###############################################################################
# Build stage
###############################################################################
FROM golang:1.24-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /build/agent-go

COPY agent-go/go.mod agent-go/go.sum ./
RUN go mod download

COPY agent-go ./
RUN go build -trimpath -ldflags="-s -w" -o /out/nexus-miner-agent ./cmd/agent

###############################################################################
# Runtime stage
###############################################################################
FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 nexus \
    && useradd --system --uid 10001 --gid nexus --home-dir /app --shell /usr/sbin/nologin nexus \
    && mkdir -p /app \
    && chown -R nexus:nexus /app

WORKDIR /app

COPY --from=builder /out/nexus-miner-agent /usr/local/bin/nexus-miner-agent

USER nexus

STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/nexus-miner-agent"]
