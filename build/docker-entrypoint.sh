#!/bin/sh
# Entrypoint for the pinard agent/worker image (root Dockerfile). Mirrors the
# %runscript bootstrap in dist/singularity/pinard-base.def, MINUS the SLURM
# preflight (HPC-specific — must not fail a k8s/OSS container).
#
# Materialize shared creds + seed the proxy provider from PINARD_POUR_URL when
# PINARD_UNCORK_URL is set. Skipped otherwise (e.g. creds already mounted in).
set -e
if [ -n "${PINARD_UNCORK_URL:-}" ]; then
    aoc uncork --url "$PINARD_UNCORK_URL"
    aoc ensure-proxy-provider || true
fi
exec /opt/pinard-home/bin/pinard "$@"
