# pinard-solo — single image bundling NATS, engram, SurrealDB, memory services,
# and the webterm-gateway for local / solo-mode operation (no k8s required).
#
# Supervisor: s6-overlay v3. Startup order: nats → engram + surrealdb → memory-ingester → webterm-gateway.
# All state lives under /data (mount a volume for persistence).
#
# Build (from build/pinard-solo/):
#   make fetch-binaries   # download nats-server, surreal, engram onto the host
#   make image            # docker build
#
# Run:
#   docker run -v $(pwd)/data:/data \
#     -e SURREAL_PASS=secret \
#     -e NATS_VIGNOBLE=myvig \
#     -p 4222:4222 -p 7437:7437 -p 8000:8000 -p 8080:8080 \
#     pinard-solo

ARG GOLANG=golang:1.24-bookworm
ARG S6_VERSION=3.1.6.2

# ── Stage 1: build Go binaries ───────────────────
FROM ${GOLANG} AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN mkdir -p /out && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/webterm-gateway  ./cmd/webterm-gateway && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/memory-ingester  ./cmd/memory-ingester && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/memory-recall    ./cmd/memory-recall  && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o /out/memory-rollup    ./cmd/memory-rollup

# ── Stage 2: runtime ─────────────────────────────
FROM debian:bookworm-slim

ARG S6_VERSION
ARG TARGETARCH=amd64

# System deps: ca-certificates, wget, netcat (health checks), curl (HEALTHCHECK).
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates wget netcat-openbsd curl xz-utils \
    && rm -rf /var/lib/apt/lists/*

# ── s6-overlay ───────────────────────────────────
RUN wget -q -O /tmp/s6-overlay.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_VERSION}/s6-overlay-noarch.tar.xz" \
    && wget -q -O /tmp/s6-overlay-arch.tar.xz \
        "https://github.com/just-containers/s6-overlay/releases/download/v${S6_VERSION}/s6-overlay-x86_64.tar.xz" \
    && tar -C / -Jxpf /tmp/s6-overlay.tar.xz \
    && tar -C / -Jxpf /tmp/s6-overlay-arch.tar.xz \
    && rm /tmp/s6-overlay.tar.xz /tmp/s6-overlay-arch.tar.xz

# ── Third-party binaries (pre-fetched by `make fetch-binaries`) ──────
# Binaries are fetched by the Makefile into build/pinard-solo/build/
# and referenced here relative to the repo-root build context.
COPY build/pinard-solo/build/nats-server /usr/local/bin/nats-server
RUN chmod +x /usr/local/bin/nats-server

COPY build/pinard-solo/build/engram /usr/local/bin/engram
RUN chmod +x /usr/local/bin/engram

COPY build/pinard-solo/build/surreal /usr/local/bin/surreal
RUN chmod +x /usr/local/bin/surreal

# ── Go binaries (built above) ────────────────────
COPY --from=builder /out/webterm-gateway  /usr/local/bin/webterm-gateway
COPY --from=builder /out/memory-ingester  /usr/local/bin/memory-ingester
COPY --from=builder /out/memory-recall    /usr/local/bin/memory-recall
COPY --from=builder /out/memory-rollup    /usr/local/bin/memory-rollup

# ── s6-overlay service definitions ───────────────
COPY build/pinard-solo/s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN find /etc/s6-overlay/s6-rc.d -name "run" -o -name "check" | xargs chmod +x

# Persist JetStream store, engram DB, and SurrealDB data across restarts.
VOLUME ["/data"]

# NATS 4222 | engram 7437 | SurrealDB 8000 | webterm-gateway 8080
EXPOSE 4222 7437 8000 8080

# All services healthy when NATS + engram + SurrealDB are reachable.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD nc -z 127.0.0.1 4222 \
     && wget -q --spider http://127.0.0.1:7437/health \
     && wget -q --spider http://127.0.0.1:8000/health

WORKDIR /
ENTRYPOINT ["/init"]
