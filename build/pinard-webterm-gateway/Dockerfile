# pinard-webterm-gateway — standalone image for the k8s web-terminal gateway.
#
# Multi-stage build: compiles the Go binary from source, assembles a minimal
# runtime layer. Uses only public base images — no internal registry references.
#
# Build (from repo root):
#   docker build -f build/pinard-webterm-gateway/Dockerfile -t pinard-webterm-gateway .
#
# Moved from cmd/webterm-gateway/Dockerfile. The pinard-backend image
# (build/pinard-backend/Dockerfile, formerly the repo-root Dockerfile) also
# includes this binary alongside the memory services and static site. Use this
# Dockerfile when you need the gateway standalone (e.g. a dedicated gateway
# deployment). Unrelated to the root Dockerfile, which now builds the
# agent/worker image.

ARG GOLANG=golang:1.24-bookworm
ARG DEBIAN=debian:bookworm-slim

# ── Stage 1: build ────────────────────────────────
FROM --platform=$BUILDPLATFORM ${GOLANG} AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/webterm-gateway ./cmd/webterm-gateway

# ── Stage 2: minimal runtime ──────────────────────
FROM ${DEBIAN}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/webterm-gateway /usr/local/bin/webterm-gateway

# PINARD_CREDENTIALS / NATS_VIGNOBLE / WEBTERM_ADDR / PINARD_WEBTERM_* /
# PINARD_NATS_PASSWORD are supplied at runtime (env vars or mounted secret).
EXPOSE 8080
ENTRYPOINT ["webterm-gateway"]
