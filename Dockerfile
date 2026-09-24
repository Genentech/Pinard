# pinard — the agent/worker image (vendangeur 🧺, maître, and régisseur all run
# this same binary; the role is selected by the args passed to `pinard`, e.g.
# `--worker --vignoble-name <v>` for a standalone/HPC-style worker).
#
# Publishes as the bare `ghcr.io/genentech/pinard` name. Equivalent to
# `dist/singularity/pinard-base.def` + `dist/singularity/pinard-os.def`
# combined into a single Docker multi-stage build — same base OS
# (`docker://rockylinux:9`, which the SIF chain already bootstraps from), same
# `make dist` bundle, same engram version, same env, same uncork bootstrap.
# Keep the two in lockstep: engram version (below, must match
# `.engram-version`), PINARD_HOME/PATH, and the uncork bootstrap logic. Does
# NOT replicate the SIF's SLURM/`sbatch` preflight — that's HPC-specific and
# must not fail a k8s/OSS container.
#
# No secrets, no internal-only CA, and no internal-only defaults (e.g. a
# corporate LLM proxy URL) are baked in — this image is published publicly.
# An internal CA can be layered in at build time via EXTRA_CA_DEB_URL, same
# opt-in pattern as build/pinard-backend/Dockerfile.
#
# Build (from repo root): docker build -t pinard .
# Run:    docker run --rm -e PINARD_UNCORK_URL=... -e PINARD_POUR_URL=... \
#           pinard --worker --vignoble-name <v> --model <id> ...

ARG DEBIAN=debian:bookworm-slim
ARG GOLANG=golang:1.24-bookworm
ARG TARGETARCH

# ── Stage 1: build the `make dist` bundle (aoc + launcher + extensions + a
# vendored Node/Pi runtime), reproducibly and version-pinned — NOT from
# whatever Node/npm happen to be ambient on the builder, which is `dist/build.sh`'s
# documented main risk when run outside CI. No --platform=$BUILDPLATFORM here:
# this stage npm-installs native addons (e.g. better-sqlite3) and vendors the
# node binary itself, both of which must match the TARGET architecture, so it
# runs emulated (via buildx/QEMU) for non-native platforms rather than
# cross-compiling.
FROM node:22-bookworm AS dist-builder
# Go toolchain, copied from the pinned golang image rather than apt (bookworm's
# packaged compiler doesn't satisfy `go 1.24.7` in go.mod). Resolves to the
# same target architecture as this stage under a multi-platform buildx build.
COPY --from=golang:1.24-bookworm /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates python3 make g++ \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY . .
ENV NODE_BIN=/usr/local/bin/node
RUN bash dist/build.sh

# ── Stage 2: runtime (Rocky Linux 9, matching the SIF chain) ─────────────────
FROM rockylinux:9 AS runtime
# Re-declare: ARGs declared before the first FROM are global-scope only and do
# NOT automatically propagate into a build stage — each stage that needs
# TARGETARCH (a buildx-populated automatic ARG) must redeclare it.
ARG TARGETARCH

# Optional corporate CA .deb (unpacked with `ar`/`tar`, same format as the
# SIF's internal CA and build/pinard-backend/Dockerfile's EXTRA_CA_DEB_URL).
# Empty by default — the published OSS image trusts only the public CA set.
ARG EXTRA_CA_DEB_URL=""
RUN set -eux; \
    dnf -y install epel-release; \
    dnf -y install git wget tar gzip which procps-ng glibc ca-certificates; \
    if [ -n "$EXTRA_CA_DEB_URL" ]; then \
        dnf -y install binutils xz; \
        wget -q -O /tmp/extra-ca.deb "$EXTRA_CA_DEB_URL"; \
        ( cd /tmp && ar p extra-ca.deb data.tar.xz | tar xJ ./usr/share/ca-certificates ); \
        find /tmp/usr/share/ca-certificates -name '*.crt' -exec cp {} /etc/pki/ca-trust/source/anchors/ \; ; \
        update-ca-trust; \
        rm -rf /tmp/extra-ca.deb /tmp/usr; \
    fi; \
    dnf clean all; \
    rm -rf /var/cache/dnf

# engram — statically linked binary required for mem_* tools (mem_save,
# mem_search, …). A sandboxed/standalone worker has no daemon to serve it, so
# it self-serves. ENGRAM_VERSION must stay in lockstep with .engram-version
# at the repo root and dist/singularity/pinard-base.def (both 1.16.1 today).
ARG ENGRAM_VERSION=1.16.1
RUN curl -fsSL \
      "https://github.com/Gentleman-Programming/engram/releases/download/v${ENGRAM_VERSION}/engram_${ENGRAM_VERSION}_linux_${TARGETARCH}.tar.gz" \
      | tar -xz -C /usr/local/bin engram \
    && chmod +x /usr/local/bin/engram \
    && /usr/local/bin/engram --version

# Extract the makeself bundle. PINARD_HOME/HOME/TMPDIR are redirected the same
# way as the SIF %post (the ~800M payload doesn't fit in a small /tmp).
COPY --from=dist-builder /src/dist/pinard-linux-x64.run /opt/pinard-linux-x64.run
RUN set -eux; \
    mkdir -p /opt/pinard-home /opt/build-home /opt/extract-tmp; \
    chmod +x /opt/pinard-linux-x64.run; \
    PINARD_HOME=/opt/pinard-home HOME=/opt/build-home TMPDIR=/opt/extract-tmp \
        /opt/pinard-linux-x64.run; \
    rm -rf /opt/extract-tmp /opt/build-home /opt/pinard-linux-x64.run; \
    chmod -R a+rX /opt/pinard-home; \
    ln -sf /opt/pinard-home/bin/pinard /usr/local/bin/pinard; \
    ln -sf /opt/pinard-home/bin/aoc /usr/local/bin/aoc; \
    ln -sf /opt/pinard-home/bin/pinard-picker /usr/local/bin/pinard-picker

ENV PINARD_HOME=/opt/pinard-home
ENV PATH=/opt/pinard-home/runtime/bin:/usr/local/bin:$PATH

# Provenance: recorded as OCI labels and in $PINARD_HOME/BUILD_INFO, so a
# booted worker can report its build (surfaced in the pinard-agents KV record
# like the SIF). See #302 for the equivalent pattern on other images.
ARG GIT_REVISION=unknown
ARG VERSION=dev
ARG BUILT_AT=unknown
LABEL org.opencontainers.image.revision="${GIT_REVISION}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILT_AT}" \
      org.opencontainers.image.source="https://github.com/genentech/pinard"
RUN printf 'revision=%s\nversion=%s\nbuilt_at=%s\n' "$GIT_REVISION" "$VERSION" "$BUILT_AT" \
      > "$PINARD_HOME/BUILD_INFO"

COPY build/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

WORKDIR /opt/pinard-home
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD []
