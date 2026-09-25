#!/usr/bin/env bash
# Build a self-contained, single-file Pinard distribution for Linux/glibc x64.
#
# Produces dist/pinard-linux-x64.run — a makeself self-extracting archive that
# bundles: the static `aoc` Go binary, the bash launcher, the Pi extensions
# (with node_modules), AND a vendored Node + Pi runtime. The target machine needs
# only the thin host CLIs (tmux, git, glab, fzf) — no node/nvm/npm.
set -euo pipefail

REPO="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/.." && pwd)"
STAGE="$REPO/dist/stage"
OUT="$REPO/dist/pinard-linux-x64.run"

# Node + Pi sources (the currently-resolved runtime). Override NODE_BIN / PI_PKG
# to pin a specific build; defaults follow the active `node`/global pi install.
#
# Auto-select the node pinard pins in .nvmrc (single source) via nvm, so `make dist`
# bakes the declared version regardless of the shell's ambient/default nvm node.
# Skipped when NODE_BIN is set explicitly or nvm/.nvmrc are unavailable (then the
# ambient node is used and the post-bootstrap check below warns on a mismatch).
if [[ -z "${NODE_BIN:-}" && -s "$HOME/.nvm/nvm.sh" && -s "$REPO/.nvmrc" ]]; then
  export NVM_DIR="$HOME/.nvm"
  set +eu
  # shellcheck disable=SC1091
  . "$NVM_DIR/nvm.sh"
  nvm use "$(tr -dc '0-9.' < "$REPO/.nvmrc")" >/dev/null 2>&1 || true
  set -eu
fi
NODE_BIN="${NODE_BIN:-$(readlink -f "$(command -v node)")}"
NODE_PREFIX="$(dirname "$(dirname "$NODE_BIN")")"            # …/vN.N.N
# PI_PKG (the pi runtime to vendor) is resolved AFTER dependency bootstrap so it
# follows the version pinard's extensions are built against
# (pi-extension/package.json — the single source of truth) rather than a divergent
# ambient global. An explicit PI_PKG override still wins.

echo "==> repo:   $REPO"
echo "==> node:   $NODE_BIN"

[[ -x "$NODE_BIN" ]] || { echo "node binary not found ($NODE_BIN)"; exit 1; }

# Confirm glibc target (the bundle is glibc-only by design).
if ! file "$NODE_BIN" | grep -q "GNU/Linux"; then
  echo "warning: node does not look like a glibc Linux build" >&2
fi

rm -rf "$STAGE"
mkdir -p "$STAGE/bin" "$STAGE/runtime/bin" "$STAGE/runtime/lib/node_modules"

# 0. Bootstrap dependencies so a clean checkout can build (node_modules are
# gitignored; babysitter is a submodule). Shared with ./install via
# scripts/bootstrap-deps.sh (single source of truth) so the two never drift:
# babysitter submodule + SDK build + @a5c-ai/babysitter-sdk resolver symlink +
# pi-extension deps. Idempotent.
echo "==> bootstrapping dependencies"
bash "$REPO/scripts/bootstrap-deps.sh" "$REPO"

# Vendor the pi runtime pinard's extensions resolve (single source of truth:
# pi-extension/package.json), so the baked runtime never diverges from the API the
# extensions were compiled against. Fall back to the ambient global install.
PI_PKG="${PI_PKG:-$REPO/pi-extension/node_modules/@earendil-works/pi-coding-agent}"
[[ -d "$PI_PKG" ]] || PI_PKG="$NODE_PREFIX/lib/node_modules/@earendil-works/pi-coding-agent"
echo "==> pi:     $PI_PKG"
[[ -d "$PI_PKG" ]] || { echo "pi package not found ($PI_PKG)"; exit 1; }

# Keep the baked node in sync with pinard's declarations (no separate version pin):
#   - .nvmrc          = the node major pinard develops/builds on (advisory)
#   - pi engines.node = the hard floor the vendored pi requires (fail-fast)
NODE_VER="$("$NODE_BIN" -p 'process.versions.node')"; NODE_MAJOR="${NODE_VER%%.*}"
NVMRC="$(tr -dc '0-9.' < "$REPO/.nvmrc" 2>/dev/null || true)"
if [[ -n "$NVMRC" && "$NODE_MAJOR" != "${NVMRC%%.*}" ]]; then
  echo "warning: baked node $NODE_VER (major $NODE_MAJOR) != .nvmrc ($NVMRC) — run 'nvm use' before building" >&2
fi
PI_NODE_REQ="$("$NODE_BIN" -p "(require('$PI_PKG/package.json').engines||{}).node||''" 2>/dev/null || true)"
REQ_MAJOR="$(printf '%s' "$PI_NODE_REQ" | grep -oE '[0-9]+' | head -1)"
if [[ -n "$REQ_MAJOR" && "$NODE_MAJOR" -lt "$REQ_MAJOR" ]]; then
  echo "error: baked node $NODE_VER does not satisfy pi engines.node '$PI_NODE_REQ'" >&2; exit 1
fi

# 1. Build provenance (commit/tag/dirty/built_at) — single source of truth
# shared with cmd/aoc/Makefile and ./install (scripts/build-info.sh). Written
# into the staged tree as $STAGE/BUILD_INFO (lands at $PINARD_HOME/BUILD_INFO)
# and baked into the aoc binary via ldflags.
echo "==> build info"
BUILD_INFO_REPO="$REPO" "$REPO/scripts/build-info.sh" "$STAGE/BUILD_INFO"
sed 's/^/    /' "$STAGE/BUILD_INFO"
# shellcheck disable=SC1091
source "$STAGE/BUILD_INFO"

echo "==> building aoc (static)"
# Build tags are opt-in via PINARD_BUILD_TAGS, not hardcoded: the OSS export
# ships only the //go:build !capsule stubs (capsule_stub.go, cmd_capsule*.go
# are dropped by scripts/export-oss.sh), so a public build must be tag-less.
# Internal builds (the top-level Makefile's `dist` target, which produces the
# internal .run / SIF images) set PINARD_BUILD_TAGS=capsule to bake in the
# real Genentech-only implementation.
BUILD_TAGS_ARGS=()
[[ -n "${PINARD_BUILD_TAGS:-}" ]] && BUILD_TAGS_ARGS=(-tags "$PINARD_BUILD_TAGS")
( cd "$REPO/cmd/aoc" && CGO_ENABLED=0 go build "${BUILD_TAGS_ARGS[@]}" \
    -ldflags "-X main.version=$TAG -X main.commit=$COMMIT_SHORT -X main.buildDate=$BUILT_AT" \
    -o "$STAGE/bin/aoc" . )

# 2. Launcher + picker.
cp "$REPO/bin/pinard" "$REPO/bin/pinard-picker" "$STAGE/bin/"
chmod +x "$STAGE/bin/"*

# 3. Extension tree + supporting assets.
echo "==> staging extensions + assets"
cp -a "$REPO/pi-extension" "$STAGE/pi-extension"
cp -a "$REPO/themes"       "$STAGE/themes"
cp -a "$REPO/processes"    "$STAGE/processes" 2>/dev/null || true
mkdir -p "$STAGE/deps"
cp -a "$REPO/deps/babysitter" "$STAGE/deps/babysitter"

# 4. Vendor Node + Pi.
echo "==> vendoring node + pi"
cp "$NODE_BIN" "$STAGE/runtime/bin/node"
cp -a "$PI_PKG" "$STAGE/runtime/lib/node_modules/pi-coding-agent"
# `pi` launcher shim → node dist/cli.js (relative, so it's relocatable).
cat > "$STAGE/runtime/bin/pi" << 'SH'
#!/usr/bin/env bash
DIR="$(cd "$(dirname "$(readlink -f "$0")")" && pwd)"
exec "$DIR/node" "$DIR/../lib/node_modules/pi-coding-agent/dist/cli.js" "$@"
SH
chmod +x "$STAGE/runtime/bin/pi" "$STAGE/runtime/bin/node"

# 5. Prune cruft not needed at runtime (Linux-only bundle).
echo "==> pruning darwin/win32 native + non-runtime files"
find "$STAGE" -type d \( -path '*/prebuilds/darwin*' -o -path '*/prebuilds/win32*' \) -prune -exec rm -rf {} + 2>/dev/null || true
find "$STAGE" -type f -name '*.node' \( -iname '*darwin*' -o -iname '*win32*' \) -delete 2>/dev/null || true
# Dev-only type-check tooling (the gate runs from the source repo, not the bundle).
rm -rf "$STAGE/pi-extension/node_modules/typescript" \
       "$STAGE/pi-extension/node_modules/@types" \
       "$STAGE/pi-extension/tsconfig.json" \
       "$STAGE/pi-extension/package-lock.json" 2>/dev/null || true
# Babysitter ships a large methodology/doc library + e2e artifacts not used at
# runtime (the worker only needs the SDK under packages/). Drop the bulk.
rm -rf "$STAGE/deps/babysitter/library" \
       "$STAGE/deps/babysitter/e2e-artifacts" \
       "$STAGE/deps/babysitter/docs" \
       "$STAGE/deps/babysitter/.git" 2>/dev/null || true
# Strip sourcemaps + markdown docs from the vendored Pi runtime (size only).
find "$STAGE/runtime" -type f \( -name '*.map' -o -name '*.md' \) -delete 2>/dev/null || true

# 6. Setup hook (runs post-extract).
cp "$REPO/dist/pinard-setup.sh" "$STAGE/pinard-setup.sh"
chmod +x "$STAGE/pinard-setup.sh"

echo "==> staged size: $(du -sh "$STAGE" | cut -f1)"

# 7. Package with makeself. We vendor a patched copy under dist/.makeself
# (committed): the stock makeself pipes the file list through `xargs tar -cf`,
# which with a large tree (~100k files) splits into multiple xargs batches that
# each truncate the archive. The vendored copy feeds paths to a single tar via
# `--null -T -` instead. See dist/.makeself/README for the one-line diff.
MAKESELF="$REPO/dist/.makeself/makeself.sh"
[[ -x "$MAKESELF" ]] || { echo "vendored makeself missing: $MAKESELF" >&2; exit 1; }

echo "==> packaging → $OUT"
# makeself stages a temp tarball in $TMPDIR; the bundle is ~1GB so point it at a
# roomy filesystem instead of a small /tmp. Override with PINARD_DIST_TMPDIR.
export TMPDIR="${PINARD_DIST_TMPDIR:-/data/storage/tmp/pinard-dist}"
mkdir -p "$TMPDIR"
# Default (temp) extraction: the setup hook copies the bundle to PINARD_HOME,
# then makeself cleans up the temp dir automatically.
# --tar-format gnu: deep node_modules paths exceed the default ustar 100/255-char
# limit; GNU format has no such limit.
"$MAKESELF" --gzip --tar-format gnu \
  "$STAGE" "$OUT" \
  "Pinard multi-agent orchestrator (Linux x64)" \
  ./pinard-setup.sh
rm -rf "$TMPDIR"

echo "==> done: $OUT ($(du -h "$OUT" | cut -f1))"
