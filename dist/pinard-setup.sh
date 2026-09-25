#!/usr/bin/env bash
# Post-extract setup, run by the makeself .run after unpacking the bundle into a
# temp dir. Installs Pinard to $PINARD_HOME (default ~/.pinard), symlinks the
# binaries into ~/.local/bin, and scaffolds user config. Self-contained: the
# bundle ships its own Node + Pi under runtime/, so no node/nvm/npm is needed.
set -euo pipefail

BUNDLE_DIR="$(cd "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")" && pwd)"
PINARD_HOME="${PINARD_HOME:-$HOME/.pinard}"
BIN_DIR="$HOME/.local/bin"

echo "Installing Pinard → $PINARD_HOME"

# 1. Preflight: thin host CLIs we do NOT bundle (not language runtimes).
missing=()
for cmd in tmux git glab fzf; do
  command -v "$cmd" &>/dev/null || missing+=("$cmd")
done
if (( ${#missing[@]} )); then
  echo "  ⚠ missing host tools: ${missing[*]}"
  echo "    install them, e.g.:  sudo apt install -y ${missing[*]}"
fi

# 2. Copy the bundle into place (replace any prior install, keep user config elsewhere).
rm -rf "$PINARD_HOME"
mkdir -p "$PINARD_HOME"
cp -a "$BUNDLE_DIR/." "$PINARD_HOME/"
rm -f "$PINARD_HOME/pinard-setup.sh"  # don't ship the installer inside the install
echo "  ✓ bundle copied"

# 3. Symlink binaries onto PATH.
mkdir -p "$BIN_DIR"
for b in aoc pinard pinard-picker; do
  ln -sf "$PINARD_HOME/bin/$b" "$BIN_DIR/$b"
done
echo "  ✓ ~/.local/bin/{aoc,pinard,pinard-picker}"

# 4. Pi permission policies (global allow; trusted worker-policy enforces deny).
PERM="$HOME/.pi/agent/pi-permissions.jsonc"
if [[ ! -f "$PERM" ]]; then
  mkdir -p "$(dirname "$PERM")"
  cat > "$PERM" << 'JSON'
{
  "defaultPolicy": { "tools": "allow", "bash": "allow", "mcp": "deny", "skills": "allow", "special": "allow" },
  "special": { "external_directory": "allow" }
}
JSON
  echo "  ✓ pi-permissions.jsonc"
fi
WP="$HOME/.pi/agent/worker-policy"
mkdir -p "$WP"
cat > "$WP/pi-permissions.jsonc" << 'JSON'
{
  "defaultPolicy": { "tools": "allow", "bash": "allow", "mcp": "deny", "skills": "allow", "special": "allow" },
  "special": { "external_directory": "deny" }
}
JSON
echo "  ✓ worker-policy (external_directory: deny)"

# 5. Credentials template.
CREDS="$HOME/.config/pinard/credentials.yaml"
if [[ ! -f "$CREDS" ]]; then
  mkdir -p "$(dirname "$CREDS")"
  cat > "$CREDS" << 'YAML'
gitlab:
  host: gitlab.example.com
  user: CHANGE_ME
  token_env: PINARD_GITLAB_TOKEN
  # owner_token_env: PINARD_OWNER_GITLAB_TOKEN  # optional: human operator's personal token;
  #   when set, spawn_agent assigns issues under the owner's identity so the
  #   owner-gate approves automatically (no manual approval comment needed).
  ssh_key: ~/.ssh/pinard_id_ed25519
  git_name: Pinard
  git_email: CHANGE_ME@example.com

nats:
  url: wss://nats.example.com
  user: CHANGE_ME
  password_env: PINARD_NATS_PASSWORD

# engram cloud replication (optional — omit to keep memory local-only)
# cloud_token_env names an env var holding the bearer token; or use cloud_token
# for a literal value. ENGRAM_CLOUD_TOKEN is set in ~/.config/pinard/env.
# cloud.json in each vignoble's .engram/ intentionally has an empty token field —
# engram cloud config only sets the server URL; the token flows via env exclusively.
#engram:
#  server: https://engram.example.com
#  cloud_token_env: ENGRAM_CLOUD_TOKEN
YAML
  echo "  ✓ credentials.yaml (TEMPLATE — edit before use)"
else
  echo "  · credentials.yaml (exists, kept)"
fi
# credentials.yaml holds secrets (tokens, engram cloud_token) — lock it down
# regardless of whether we just created it or it already existed. Idempotent.
chmod 600 "$CREDS"
chmod 700 "$(dirname "$CREDS")"

# 6. Corporate CA certs (filesystem fallback for NATS WS TLS).
# Place any required corporate root/issuing CA .crt files in ~/.config/pinard/certs/
# before connecting to a TLS-terminated NATS server.
CERTS="$HOME/.config/pinard/certs"
mkdir -p "$CERTS"

# 7. Remove stale global extension symlink (would conflict with workers).
[[ -L "$HOME/.pi/agent/extensions/pinard.ts" ]] && rm -f "$HOME/.pi/agent/extensions/pinard.ts"

cat << EOF

Done. Pinard installed to $PINARD_HOME (bundled Node + Pi).

Next steps:
  1. Edit ~/.config/pinard/credentials.yaml (replace CHANGE_ME)
  2. Export secrets: PINARD_GITLAB_TOKEN, PINARD_NATS_PASSWORD
  3. ssh-keygen -t ed25519 -C pinard -f ~/.ssh/pinard_id_ed25519 -N ""
  4. aoc init myproject --gitlab-host gitlab.example.com
  5. cd ~/vignoble-myproject && pinard

Ensure ~/.local/bin is on PATH.
EOF
