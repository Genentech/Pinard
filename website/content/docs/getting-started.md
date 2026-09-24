---
title: Getting Started
weight: 2
group: Introduction
---

This guide takes you from nothing to a running vignoble with a conductor.

> **Two deployment paths.** The enterprise path (shared NATS cluster, cloud services) is described in full below. For a **solo laptop install** with no shared infrastructure, jump to [Solo mode (laptop)](#solo-mode-laptop).

<figure class="doc-figure doc-figure--wide">
  <div class="doc-figure-visual">
    <img src="/images/docs/getting-started-journey-v2.jpg" alt="A six-stop sketched journey from a Pinard installation crate through credentials, estate creation, repository registration, daemon startup, and an open conductor control room.">
    <span class="doc-figure-label charcoal" style="--x: 8.3%; --y: 17.8%;">1</span>
    <span class="doc-figure-label charcoal" style="--x: 24.3%; --y: 17.8%;">2</span>
    <span class="doc-figure-label charcoal" style="--x: 41.5%; --y: 17.8%;">3</span>
    <span class="doc-figure-label charcoal" style="--x: 57.2%; --y: 17.8%;">4</span>
    <span class="doc-figure-label charcoal" style="--x: 73%; --y: 17.8%;">5</span>
    <span class="doc-figure-label charcoal" style="--x: 89.1%; --y: 17.8%;">6</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 8%; --y: 76%;">Install</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 24%; --y: 76%;">Credentials</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 41.5%; --y: 76%;">Create vignoble</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 57%; --y: 76%;">Register vigne</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 73%; --y: 76%;">Start daemon</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 90%; --y: 76%;">Launch conductor</span>
  </div>
  <figcaption><strong>Your first vignoble, in six stops.</strong> The illustration is a progress map; the commands below remain the source of truth.</figcaption>
  <ol class="doc-figure-legend doc-figure-legend--column-major" aria-label="Getting started sequence">
    <li><span class="doc-figure-key">1</span><span>Install Pinard from a release bundle or source.</span></li>
    <li><span class="doc-figure-key">2</span><span>Configure GitLab and NATS credentials.</span></li>
    <li><span class="doc-figure-key">3</span><span>Scaffold the vignoble with <code>aoc init</code>.</span></li>
    <li><span class="doc-figure-key">4</span><span>Register each repository as a vigne.</span></li>
    <li><span class="doc-figure-key">5</span><span>Start the always-on daemon.</span></li>
    <li><span class="doc-figure-key">6</span><span>Optionally enter the conductor control room.</span></li>
  </ol>
</figure>

## Install

Pinard ships two ways.

### Container images

Pre-built images are published to **GitHub Container Registry** on every release —
no credentials needed to pull:

| Image | Contents |
|-------|----------|
| `ghcr.io/genentech/pinard` | the agent/worker image — vendored Node/Pi runtime, `aoc`, and `engram`. Runs as régisseur, maître, or vendangeur (worker) depending on the args passed to `pinard`. |
| `ghcr.io/genentech/pinard-backend` | k8s-hosted backend: webterm gateway + memory services + static docs site |
| `ghcr.io/genentech/pinard-webterm-gateway` | standalone webterm gateway only |

```bash
docker pull ghcr.io/genentech/pinard:latest
docker pull ghcr.io/genentech/pinard-backend:latest
docker pull ghcr.io/genentech/pinard-webterm-gateway:latest
```

The `pinard-backend` and `pinard-webterm-gateway` images are for **k8s-hosted backend
services**; the `pinard` (agent/worker) image is for running a worker (or a
régisseur/maître) without a host install — e.g. on k8s or an HPC node. It bootstraps
credentials from `PINARD_UNCORK_URL`/`PINARD_POUR_URL` the same way the Singularity
(`pinard-base.sif`) build does. For a host install, use a release bundle or source
(below) instead.

### From a release bundle (recommended)

A release is a single self-extracting `.run` archive (Linux/glibc x64). It bundles the
`aoc` binary, the launcher, the Pi extensions, and a vendored Node + Pi runtime — so the
host needs only the thin CLIs `tmux git glab fzf`, no Node/npm.

```bash
./pinard-linux-x64.run          # installs to ~/.pinard, symlinks aoc/pinard into ~/.local/bin
```

### From source

```bash
cd pinard/cmd/aoc && make install   # builds + installs aoc to ~/.local/bin
./install                            # sets up config templates, permissions, pre-commit hook
```

**Runtime requirements:** `./install` enforces **Node ≥ 22.19.0** and **Pi ≥ 0.80.6**.
If `nvm` is available it activates the pinned Node (22 LTS) from `.nvmrc` automatically;
otherwise install Node 22 manually before running `./install`. Node 22 LTS ships
native prebuilts for `better-sqlite3`, so no C++ compiler is needed on the host.

Set `PINARD_NODE=/path/to/node` to override which node binary is used when the nvm
default is too old; the launcher and daemon both respect this variable.

`aoc` is a static Go binary with zero runtime dependencies.

The **engram CLI** is installed (or upgraded/downgraded) to the **same version the
cluster runs** — pinned in `.engram-version` at the repo root. This keeps the local and
cloud engram in lockstep; a version drift can cause cloud sync to fail due to
mutation/chunk format mismatches. Re-running `./install` after a cluster upgrade will
update your local engram CLI automatically.

## Create a vignoble (interactive wizard)

The fastest path on a fresh machine is the interactive wizard. Run `aoc init` (or
`pinard init`) from any directory with no arguments and answer a few prompts:

```
$ aoc init

Welcome to Pinard! Let's create your first vignoble.

Vignoble name [myproject]:
Location
  * 1) ~/vignoble-myproject
    2) Use the current directory (/home/me/code)
Choice [1]:
Backend
  * 1) Connect to a Git host (GitHub / GitLab)
    2) Solo / local (no Git host — laptop-only)
Choice [1]:
Git provider
  * 1) GitHub (github.com or GHES)
    2) GitLab (self-hosted or gitlab.com)
Choice [1]:
Git host [github.com]:
GitHub org (optional):
Add a first repository now? [y/N]: y
  Repository short name (e.g. my-app): my-api
  Repository path (e.g. owner/my-app): myorg/my-api
  Enable auto-merge? [y/N]:
```

The wizard:
- Scaffolds `~/vignoble-<name>` (or the current directory) with all the required files.
- Generates a `~/.config/pinard/credentials.yaml` **template** if one does not exist —
  fill in the `CHANGE_ME` fields before starting the daemon.
- Starts the daemon automatically.

Just press **Enter** to accept every default and you get a GitHub-backed vignoble at
`~/vignoble-<name>`. Run `pinard` from the vignoble directory to open the conductor.

> **Already have credentials?** If `~/.config/pinard/credentials.yaml` already exists
> the wizard leaves it untouched.

### Non-interactive (CI / scripts)

Passing all required flags bypasses the wizard entirely — safe for automation:

```bash
# GitLab
aoc init myproject --gitlab-host gitlab.example.com --gitlab-group mygroup

# GitHub (via pressoir block in vignes.yaml)
aoc init myproject --gitlab-host "" --path ~/vignoble-myproject

# Solo / local
aoc init myproject --local
```

## Credentials

The wizard writes a template to `~/.config/pinard/credentials.yaml` when none exists.
Fill in the `CHANGE_ME` fields and export the referenced env vars:

```bash
# GitHub-backed (template filled in by the wizard)
export PINARD_GITHUB_TOKEN="ghp_xxxxx"
export PINARD_NATS_PASSWORD="xxxxx"
echo 'export PINARD_GITHUB_TOKEN=...' >> ~/.config/pinard/env
echo 'export PINARD_NATS_PASSWORD=...' >> ~/.config/pinard/env
```

```bash
# GitLab-backed
export PINARD_GITLAB_TOKEN="glpat-xxxxx"
export PINARD_NATS_PASSWORD="xxxxx"
echo 'export PINARD_GITLAB_TOKEN=...' >> ~/.config/pinard/env
```

Secrets placed in `~/.config/pinard/env` are sourced automatically by the launcher and
daemon. See [Configuration](/docs/configuration/) for the full schema.

## Register a vigne

The wizard can add a first repo during creation. To add more later:

```bash
aoc add vigne my-api --path ~/my-api --repo myorg/my-api
```

This appends an entry to `vignes.yaml`. Repeat for every repo. (Add `--auto-merge` only if
you want that vigne's MRs merged automatically — it's off by default; see
[Configuration](/docs/configuration/))

## Run the daemon

The daemon is the always-on engine (MR/issue/schedule watchers, auto-spawn, dispatch):

```bash
aoc daemon start      # self-daemonizes, logs to logs/aoc-daemon.log, PID in .state/daemon.pid
aoc daemon status     # check it's alive
```

`aoc daemon start/stop/restart/status` manage the background process. It hot-reloads
itself when the `aoc` binary, `vignes.yaml`, or `schedules.yaml` change.

## Launch the conductor

The conductor (régisseur) is an interactive Pi session that receives events, spawns
agents, and lets you steer work conversationally:

```bash
cd ~/vignoble-myproject
pinard
```

Run `pinard` from anywhere (with no vignoble) to pick and attach to a running session via
`fzf`. The conductor is optional — the daemon does the mechanical work on its own — but it
gives you the LLM-powered control room.

## Solo mode (laptop)

For a single-user laptop install with **no shared NATS cluster or cloud services**,
use `aoc init --local`. It writes a `~/.config/pinard/credentials.yaml` pre-configured
for localhost endpoints (NATS on `4222`, engram on `7437`, SurrealDB on `8000`,
webterm on `8080`) and skips the `--gitlab-host` requirement.

### 1. Start the local service stack

Spin up the `pinard-solo` Docker image, which bundles NATS, engram, SurrealDB,
the memory services, and the webterm gateway under one supervisor:

```bash
docker run -d \
  -v $(pwd)/pinard-data:/data \
  -e SURREAL_PASS=changeme \
  -e NATS_VIGNOBLE=myproject \
  -p 4222:4222 -p 7437:7437 -p 8000:8000 -p 8080:8080 \
  pinard-solo:latest
```

All persistent state (NATS JetStream store, engram database, SurrealDB) lives under
the mounted `/data` volume so restarts resume from the same state.

### 2. Scaffold the vignoble

```bash
aoc init myproject --local                          # no --gitlab-host required
# Optional: add GitLab details if you want issue/MR tracking
aoc init myproject --local --gitlab-host gitlab.com --gitlab-group mygroup
cd ~/vignoble-myproject
```

`--local` writes `~/.config/pinard/credentials.yaml` with localhost endpoints. It does
**not** start the daemon — the daemon is started separately after services are up.

### 3. Set your LLM key and start the daemon

```bash
echo 'export ANTHROPIC_API_KEY=sk-ant-...' >> ~/.config/pinard/env
aoc daemon start
```

BYO LLM keys (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`) in `~/.config/pinard/env` are
passed through to agents automatically — no proxy configuration needed.

To use OpenAI or DeepSeek natively (no Anthropic proxy), also set `models.provider`
in `vignes.yaml`. See [Custom LLM provider](/docs/configuration/#custom-llm-provider-openai--deepseek) for details.

### 4. Launch the conductor

```bash
cd ~/vignoble-myproject
pinard
```

> **macOS port overrides.** If a native macOS Pinard services app is present, it writes
> `~/Library/Application Support/Pinard/config.json` with the ports it chose. `aoc init
> --local` reads this file automatically so the generated `credentials.yaml` uses the
> same ports. You rarely need to set these manually.

## Next steps

- [Orchestration & Parcelles](/docs/orchestration/) — how the régisseur, maîtres, and
  vendangeurs divide work.
- [The SWE Process](/docs/swe-process/) — drive work from GitLab issues.
- [CLI Reference](/docs/cli-reference/) — the full `aoc` and `pinard` surface.
