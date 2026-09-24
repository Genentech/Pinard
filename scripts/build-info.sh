#!/usr/bin/env bash
# Shared git build-provenance helper — the SINGLE source of truth for
# commit/tag/dirty/built_at so dist/build.sh, cmd/aoc/Makefile, and ./install
# can never drift (see #302).
#
# Usage:
#   scripts/build-info.sh              # print KEY=value lines to stdout
#   scripts/build-info.sh <dest-file>  # write KEY=value lines to <dest-file>
#
# Emits: COMMIT, COMMIT_SHORT, TAG, DIRTY, BUILT_AT.
# Degrades to "unknown" outside a git checkout — never fails the caller.
set -euo pipefail

REPO_DIR="${BUILD_INFO_REPO:-$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)}"

if git -C "$REPO_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  COMMIT="$(git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
  COMMIT_SHORT="$(git -C "$REPO_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)"
  TAG="$(git -C "$REPO_DIR" describe --tags --always --dirty 2>/dev/null || echo unknown)"
  if git -C "$REPO_DIR" diff-index --quiet HEAD -- 2>/dev/null; then
    DIRTY=false
  else
    DIRTY=true
  fi
else
  COMMIT=unknown
  COMMIT_SHORT=unknown
  TAG=unknown
  DIRTY=false
fi
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

OUT="COMMIT=$COMMIT
COMMIT_SHORT=$COMMIT_SHORT
TAG=$TAG
DIRTY=$DIRTY
BUILT_AT=$BUILT_AT"

if [[ $# -ge 1 ]]; then
  printf '%s\n' "$OUT" > "$1"
else
  printf '%s\n' "$OUT"
fi
