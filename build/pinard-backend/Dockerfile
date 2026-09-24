# pinard-backend — one image backing the website, the webterm-gateway, AND the
# memory services, selected by `command`:
#   webterm-gateway serve-site --dir /srv/site --addr :80   → static docs site (port 80)
#   webterm-gateway                                          → k8s web-terminal gateway (port 8080)
#   memory-ingester                                          → memory ingester
#   memory-recall                                            → memory recall service
#   memory-rollup                                            → scope roll-up engine
#   memory-curator                                           → per-vigne wiki curator
#
# Moved from the repo root: the root `Dockerfile` now builds the agent/worker
# image (see /Dockerfile) — GHCR's bare `ghcr.io/genentech/pinard` name was
# reassigned to the worker; this backend bundle publishes as
# `ghcr.io/genentech/pinard-backend`. Internally this is still what the
# gpapy-asg-ci `.build-image`/ECR pipeline expects to find at the repo-root
# `Dockerfile` path — see the note in .gitlab-ci.yml above the `build` job.
#
# Build (from repo root): docker build -f build/pinard-backend/Dockerfile .
# Multi-stage: render the Hugo docs, build all Go binaries, assemble a minimal
# runtime. No Python runtime required — all memory services are native Go binaries.

ARG DEBIAN=debian:bookworm-slim
ARG GOLANG=golang:1.24-bookworm
ARG TARGETARCH

# ── Stage 1: render the Hugo site ────────────────
FROM --platform=$BUILDPLATFORM ${DEBIAN} AS site
ARG HUGO_VERSION=0.148.1
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates wget git gcc g++ libc-dev && \
    wget -q https://go.dev/dl/go1.24.7.linux-amd64.tar.gz && \
        tar xzf go1.24.7.linux-amd64.tar.gz -C /usr/local && rm go1.24.7.linux-amd64.tar.gz && \
    wget -q https://github.com/gohugoio/hugo/releases/download/v${HUGO_VERSION}/hugo_extended_${HUGO_VERSION}_linux-amd64.tar.gz && \
        tar xzf hugo_extended_${HUGO_VERSION}_linux-amd64.tar.gz -C /usr/local/bin && \
        rm hugo_extended_${HUGO_VERSION}_linux-amd64.tar.gz && \
    rm -rf /var/lib/apt/lists/*
ENV PATH="/usr/local/go/bin:${PATH}"
WORKDIR /src
COPY website/ ./website/
RUN cd website && hugo --minify --destination /out/site

# ── Stage 2: build all Go binaries ───────────────
FROM --platform=$BUILDPLATFORM ${GOLANG} AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/webterm-gateway ./cmd/webterm-gateway && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/memory-ingester ./cmd/memory-ingester && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/memory-recall    ./cmd/memory-recall && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/memory-rollup    ./cmd/memory-rollup && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
        go build -trimpath -ldflags="-s -w" -o /out/memory-curator   ./cmd/memory-curator

# ── Stage 3: minimal runtime ──────────────────────
FROM ${DEBIAN}
# EXTRA_CA_DEB_URL: when set, download and install an extra corporate/internal CA
# .deb so the pod can reach TLS endpoints behind a private CA. Leave empty for
# public builds and rely on the Helm-mounted CA bundle (memory.caBundle) instead.
# No private URL appears in this file — it is passed as a build arg by internal CI.
ARG EXTRA_CA_DEB_URL=""
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git openssh-client wget \
    && if [ -n "$EXTRA_CA_DEB_URL" ]; then \
         wget -q "$EXTRA_CA_DEB_URL" -O extra-ca-certificates.deb \
         && dpkg -i extra-ca-certificates.deb \
         && rm -f extra-ca-certificates.deb \
         && update-ca-certificates; \
    fi \
    && apt-get purge -y wget && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/webterm-gateway   /usr/local/bin/webterm-gateway
COPY --from=build /out/memory-ingester   /usr/local/bin/memory-ingester
COPY --from=build /out/memory-recall     /usr/local/bin/memory-recall
COPY --from=build /out/memory-rollup     /usr/local/bin/memory-rollup
COPY --from=build /out/memory-curator    /usr/local/bin/memory-curator
COPY --from=site  /out/site              /srv/site
# Entrypoint script: if EXTRA_CA_CERTS points to a mounted CA bundle, append it
# to the system trust store before starting the main process.
RUN printf '#!/bin/sh\nset -e\nif [ -n "$EXTRA_CA_CERTS" ] && [ -f "$EXTRA_CA_CERTS" ]; then\n    cat "$EXTRA_CA_CERTS" >> /etc/ssl/certs/ca-certificates.crt\nfi\nexec "$@"\n' > /usr/local/bin/docker-entrypoint.sh \
    && chmod +x /usr/local/bin/docker-entrypoint.sh
WORKDIR /app
EXPOSE 80 8080
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh", "webterm-gateway"]
CMD []
