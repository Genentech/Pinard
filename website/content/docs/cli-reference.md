---
title: CLI Reference
weight: 60
group: Reference
---

Two entry points: **`aoc`** (the Go binary — scaffolding, daemon, spawning, everything
mechanical) and **`pinard`** (the launcher shell script — starts the conductor tiers and
workers).

<figure class="doc-figure">
  <div class="doc-figure-visual">
    <img src="/images/docs/cli-command-map-v2.jpg" alt="A cellar tool board with six independent command-family stations: launcher, estate setup, runtime, observation, remote and funding, and administration. The stations are categories, not sequential steps.">
    <span class="doc-figure-label doc-figure-label--desktop charcoal" style="--x: 18%; --y: 43%;"><strong>L · Launcher</strong><br><code>pinard</code> · <code>--maitre</code> · <code>--worker</code></span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 50%; --y: 43%;"><strong>E · Estate setup</strong><br><code>init</code> · <code>add vigne</code> · <code>config</code></span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 83.33%; --y: 43%;"><strong>R · Runtime</strong><br><code>daemon</code> · <code>spawn</code> · <code>maitre</code></span>
    <span class="doc-figure-label doc-figure-label--desktop charcoal" style="--x: 16.67%; --y: 91%;"><strong>O · Observe</strong><br><code>status</code> · <code>dashboard</code> · <code>track-mr</code></span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 50%; --y: 91%;"><strong>X · Remote & funding</strong><br><code>uncork</code> · <code>webterm</code> · <code>capsule-*</code></span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 83.33%; --y: 91%;"><strong>A · Administration</strong><br><code>create-user</code> · <code>cleanup</code> · internals</span>
  </div>
  <figcaption><strong>Choose a family, then scan its reference section.</strong> These are independent tool groups, not a command sequence; the detailed syntax below remains authoritative.</figcaption>
  <ul class="doc-figure-legend" aria-label="CLI command families">
    <li><span class="doc-figure-key charcoal">L</span><span><strong>Launcher</strong> — enter a control tier, attach to a session, or start a worker with <code>pinard</code>.</span></li>
    <li><span class="doc-figure-key">E</span><span><strong>Estate setup</strong> — create the vignoble and register repositories, schedules, and configuration.</span></li>
    <li><span class="doc-figure-key terracotta">R</span><span><strong>Runtime</strong> — operate the daemon, agents, maîtres, and notifications.</span></li>
    <li><span class="doc-figure-key charcoal">O</span><span><strong>Observe</strong> — inspect status, dashboards, merge requests, schedules, and browser terminals.</span></li>
    <li><span class="doc-figure-key">X</span><span><strong>Remote & funding</strong> — bootstrap isolated hosts and manage optional Capsule Protocol commands.</span></li>
    <li><span class="doc-figure-key terracotta">A</span><span><strong>Administration</strong> — provision accounts, archive completed work, and support launcher internals.</span></li>
  </ul>
</figure>

## `pinard` (launcher)

```bash
pinard                              # in a vignoble dir: start the régisseur
pinard                              # anywhere else: fzf-pick a running session and attach
pinard --maitre <parcelle>         # start/attach a parcelle maître
pinard --worker …                  # run as a worker (see Remote Workers)
pinard --restart                   # kill and restart pinard tmux sessions
```

The launcher resolves its runtime as **bundled > nvm > PATH**, so a release bundle needs
no system Node.

## `aoc` — setup

### `aoc init [name]`

Scaffold a vignoble and start its daemon.

```bash
aoc init myproject --gitlab-host gitlab.com --gitlab-group mygroup [--path ~/vignoble-myproject]
aoc init myproject --local        # solo mode: localhost endpoints, no --gitlab-host required
```

| Flag | Purpose |
|------|---------|
| `--gitlab-host` | GitLab hostname (required for normal mode; optional with `--local`) |
| `--gitlab-group` | GitLab group path |
| `--path` | Target directory (defaults to `~/vignoble-<name>`) |
| `--local` | Solo mode: write `~/.config/pinard/credentials.yaml` with localhost endpoints; does not start the daemon automatically (run `aoc daemon start` after services are up) |

### `aoc add vigne <name>`

Register a repository in the current vignoble's `vignes.yaml`.

```bash
aoc add vigne my-api --path ~/my-api --repo mygroup/my-api [--auto-merge]
```

### `aoc add schedule <name>` / `aoc schedule`

Add a cron-scheduled spawn (see [Scheduling](/docs/scheduling/)).

```bash
aoc add schedule nightly --project my-api --cron "0 2 * * *" --prompt "…"
```

### `aoc config get|set <path> [value]`

Read or write `vignes.yaml` with dot-path notation.

```bash
aoc config set vignes.my-api.auto_merge true
aoc config get models.worker.id
```

## `aoc` — daemon

```bash
aoc daemon start        # start detached (self-daemonizing, PID in .state/daemon.pid)
aoc daemon status       # liveness
aoc daemon stop
aoc daemon restart
```

`aoc daemon` (no subcommand) runs the watchers in the foreground. The one-shot compat
commands `aoc watch-mrs`, `aoc watch-issues`, and `aoc run-schedules` run a single cycle —
prefer the daemon for continuous operation.

## `aoc` — agents

### `aoc spawn`

Launch a worker in its own git worktree + tmux session.

```bash
aoc spawn --project my-api --prompt "Fix the auth bug in login.go"
aoc spawn --project my-api --prompt "Task" --target-branch cuvee/batch-1
aoc spawn --project my-api --issue 42 --parcelle semantic-search
```

| Flag | Purpose |
|------|---------|
| `--project` | Vigne/project name |
| `--prompt` | Task prompt |
| `--issue` | GitLab issue IID driving the work |
| `--parcelle` | Workstream name (defaults to the project) |
| `--target-branch` | MR target branch (auto-detected from repo default branch if omitted; `cuvee/<name>` branches are auto-created on origin if missing) |
| `--name` | Session name (auto-generated if omitted) |
| `--process` | Babysitter process definition |
| `--run-id` | Resume an existing babysitter run |
| `--runtime` | `local` (default) or `singularity` |
| `--sif` | Singularity image path (with `--runtime singularity`) |
| `--no-worktree` | Run in the project path without a worktree (data/orchestration jobs) |
| `--force` | Spawn even if a live worker already exists for this run ID |
| `--contract-id` | Mnemosyne contract ID — injects `PINARD_CAPSULE_CONTRACT` into the worker env; auto-detected from the issue when `--issue` is given |
| `--no-capsule` | Skip capsule auto-detection; spawn on operator token even if the issue has a funded contract |

### `aoc attach <session>`

Stream a vendangeur's terminal output over NATS to your **local terminal**. Resolves
the session from the `pinard-agents` KV by name, agentId, or runId. Uses the same
grant-gated responder protocol as the web gateway — requires `webterm.grant_secret`
in `credentials.yaml`.

```bash
aoc attach my-session             # read-only view
aoc attach abc123def              # resolve by agentId or runId
aoc attach my-session --timeout 5m  # detach after 5 min idle
aoc attach my-session --steer     # writable steer mode (operator only)
```

| Flag | Purpose |
|------|--------|
| `--vignoble-name` | Vignoble NATS namespace; defaults to `NATS_VIGNOBLE` |
| `--timeout` | Detach after this much idle time (0 = no timeout, the default) |
| `--steer` | Open in read-write mode — forwards local keystrokes to the agent's PTY |

Press `Ctrl+C` to detach (sends a close signal so the responder tears down immediately).
For a browser-based view, see `aoc webterm-link` and [Web Terminal](/docs/web-terminal/).

### `aoc maitre spawn|attach|list`

Manage per-parcelle maître windows.

```bash
aoc maitre attach --parcelle semantic-search   # spawn-if-missing, then switch to its window
aoc maitre list                                 # windows in the conductor session
```

### `aoc notify <message>`

Publish a notification to the conductor over NATS.

```bash
aoc notify "Task complete, opened MR !42"
```

## `aoc` — pressoir (provider-neutral git host)

The **pressoir** commands are a thin, provider-neutral shell over your git host
(GitLab or GitHub). The extension calls them internally instead of shelling out to
`glab`; you can also call them from scripts or the conductor without knowing which
provider is in use.

```bash
aoc pressoir get-issue --repo <owner/repo> --number <n>
aoc pressoir get-pr --repo <owner/repo> --number <n>
aoc pressoir open-pr --repo <owner/repo> --src <branch> --dst <branch> --title <t> [--body <b>] [--draft]
aoc pressoir comment-pr --repo <owner/repo> --number <n> --body <b>
aoc pressoir comment-issue --repo <owner/repo> --number <n> --body <b>
aoc pressoir list-pr-notes --repo <owner/repo> --number <n>
aoc pressoir list-issue-notes --repo <owner/repo> --number <n>
aoc pressoir update-issue --repo <owner/repo> --number <n> [--labels l] [--add-labels l] [--remove-labels l] [--state-event open|close] [--assignee u]
aoc pressoir resolve-user --username <u> [--repo <owner/repo>]
aoc pressoir track-pr --repo <owner/repo> --number <n> --session <s>
aoc pressoir get-repo --repo <owner/repo>
aoc pressoir link-issues --repo <owner/repo> --number <n> --target-repo <r2> --target-number <n2> [--link-type blocks]
aoc pressoir approve-pr --repo <owner/repo> --number <n>             # approve using PINARD_OWNER_GITLAB_TOKEN (manual use only)
aoc pressoir get-pr-changes --repo <owner/repo> --number <n>          # list changed files as a newline-separated list
```

All subcommands output JSON on stdout. The provider (`gitlab` or `github`) is resolved
from `vignes.yaml` for the given `--repo`; no flags needed when the repo is registered.

`approve-pr` uses `PINARD_OWNER_GITLAB_TOKEN` (the operator’s own PAT, not the bot
token) so GitLab’s self-approval restriction does not apply. It is a **manual operator
tool** — Pinard’s automated review path (see [MR Workflow](/docs/mr-workflow/#automated-review-auto_review))
never calls it automatically.

`get-pr-changes` outputs one file path per line; useful for scripts and for the
automated review prompt.

### `aoc epic` {#aoc-epic}

Provider-neutral epic operations. On GitLab this maps to a GitLab issue hierarchy;
on GitHub it creates a parent issue with native sub-issues (task-list fallback when
sub-issues are unavailable).

```bash
aoc epic create --repo <owner/repo> --title <t> [--body <b>]   # create a parent issue (epic)
aoc epic add-child --repo <owner/repo> --parent <N> --child <M>  # attach a child issue
```

## `aoc` — merge requests

```bash
aoc track-mr --session <s> --mr <n> --project <p>   # register an MR with the watcher
aoc untrack-mr --session <s>                          # stop watching
```

### `aoc mr-memory` {#aoc-mr-memory}

Fetch a merged MR from GitLab and publish a memory event — identical to what the
live mr-watcher emits on merge. Useful for backfilling knowledge from MRs that
merged before memory ingestion was configured, or for replaying an MR that was
skipped by the noise filter.

```bash
aoc mr-memory --repo group/project --mr <iid>
aoc mr-memory --repo group/project --mr <iid> --dry-run    # inspect payload, no publish
aoc mr-memory --repo group/project --mr <iid> --force      # bypass noise filter
aoc mr-memory --repo group/project --mr <iid> --project <name>  # explicit project name
```

The command runs both the description+issues pass (Pass 1) and the review delta pass
(Pass 2), and also extracts any `@memory:` markers from the review notes. By default
the project name is resolved from `vignes.yaml`; if no match is found it falls back
to the repository basename.

## `aoc` — status & schedules

```bash
aoc status              # tracked MRs, issues, workers, schedules
aoc dashboard           # live TUI: workers, MRs, schedules, notifications
aoc list-schedules      # schedules and their last run times
aoc unschedule --name <name>
```

## `aoc gc` — agent garbage collector {#aoc-gc}

Reap workers that are done and sweep orphaned tmux socket files. Safe by default:
never touches conductor/maître/régisseur, open-MR workers, active-turn workers, or
fresh remote/standalone workers.

```bash
aoc gc                          # reap finished workers + sweep dead sockets
aoc gc --dry-run                # preview what would be collected; no changes
aoc gc --vignoble myproject     # scope to a specific vignoble
aoc gc --all                    # scope to all vignobles in the KV bucket
aoc gc --older-than 30m         # idle grace period before a finished worker is reaped (default 10m)
aoc gc --no-workers             # skip worker reaping (sockets only)
aoc gc --no-sockets             # skip tmux socket sweep (workers only)
```

**Reap criteria** (a worker is reaped when it meets any of these):

| Criterion | Condition |
|-----------|----------|
| Ephemeral / scheduled | Idle longer than `--older-than`, not actively turning |
| Abandoned | No local tmux session, heartbeat stale > 4 h |
| Stopped / done | `state=stopped` or `state=done` and idle > `--older-than` |

**Always preserved:** the `conductor` session and reserved windows, workers with an
open non-merged MR, workers actively running a turn (`tempo=active`), and remote/
standalone workers with a recent heartbeat (within 4 hours).

Reaping kills the tmux session, removes the KV entry, and removes the worktree.
Socket sweep removes dead `pinard-*` sockets and leftover webterm-test sockets in
`/tmp/tmux-<uid>/`.

## `aoc` — web terminal

```bash
aoc webterm-link --target <session>            # print a read-only browser link
aoc webterm-link --target <session> --auto     # same, but print nothing (exit 0) when webterm/post_links is off
aoc webterm-responder                          # run the tmux-backed host responder
aoc webterm-worker-responder                   # daemon-less PTY responder (HPC / no-tmux path)
aoc webterm-doctor [vignoble]                  # diagnose /sessions visibility for all agents
```

The link is **unsigned** when Cognito SSO is enabled (gateway grants only SSO'd operators)
or **signed + expiring** otherwise. `--auto` is intended for automated callers that want
to append a link only when one exists.

`aoc webterm-worker-responder` bridges the caller's own PTY (passed via `--pty-fd`)
directly over NATS using the same grant-gated protocol — no tmux required. It is
launched automatically by `bin/pinard --worker` on daemon-less or Singularity hosts
where tmux is unavailable.

`aoc webterm-doctor` connects to NATS, reads all records from the `pinard-agents` KV
bucket, and prints per-agent include/exclude reasoning matching the logic the gateway's
`/sessions` index uses. Use it to diagnose why a worker does not appear in the
control-room index:

```bash
aoc webterm-doctor                           # use resolved vignoble
aoc webterm-doctor myproject                 # pass vignoble as positional argument
aoc webterm-doctor --vignoble-name myproject # or as a flag
```

Output columns: `VERDICT` (INCLUDE / EXCLUDE / LOCAL / ERROR), `KEY`, and `REASON`
(e.g. vignoble mismatch, stale lastSeen, missing name, or the role + state for
included agents). "LOCAL" means the agent appears via tmux listing, not the KV scan.
Deleted/tombstoned KV keys (from normal worker teardown) are skipped silently rather
than printed as `ERROR` rows; a trailing `(skipped N deleted/tombstoned keys)` line
reports how many were filtered out.

| Flag (`webterm-worker-responder`) | Purpose |
|-----------------------------------|---------|
| `--session-name` | Session name this responder answers for |
| `--pty-fd` | PTY master file descriptor (opened by the caller) |
| `--vignoble-name` | Vignoble NATS namespace |

See [Web Terminal](/docs/web-terminal/).

## `aoc uncork`

Materialize a credential/config bundle for a sandboxed or HPC worker. Reads a JSON
manifest from a URL or stdin and writes each listed file under `$HOME`.

```bash
aoc uncork                          # read manifest from stdin
aoc uncork --url <endpoint>         # fetch manifest from URL (default: $PINARD_UNCORK_URL)
aoc uncork --url <endpoint> --home /custom/home  # write files under a different base dir
```

The manifest is a JSON object:

```json
{
  "files": [
    { "path": ".config/pinard/credentials.yaml", "content": "…", "mode": "0600" },
    { "path": "encoded.bin", "content": "<base64>", "encoding": "base64", "checksum": "sha256:<hex>" }
  ]
}
```

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `path` | ✓ | — | Relative to `$HOME`; absolute paths and `..` traversal are rejected |
| `content` | ✓ | — | File body (plain string or base64-encoded) |
| `encoding` | — | `plain` | `base64` decodes the content field |
| `mode` | — | `0600` | Octal file permission string |
| `checksum` | — | none | Optional `sha256:<hex>` for integrity verification |

The command fails fast on any non-2xx response, a `410 Gone` (revoked bundle), or malformed JSON.
See [Remote Workers — Sandboxed bootstrap](/docs/remote-workers/#sandboxed-bootstrap) for the full workflow.

## `aoc` — capsules

Buddy Capsules fund a vendangeur's LLM quota through Mnemosyne.
See [Buddy Capsules](/docs/capsules/) for the full workflow.

### `aoc capsule-keygen`

Generate the ed25519 identity keypair for this Pinard host (one-time setup).

```bash
aoc capsule-keygen            # writes ~/.config/pinard/capsule_key.pem
aoc capsule-keygen --force    # rotate: overwrite existing keypair
```

### `aoc capsule-pubkey`

Print the base64-encoded raw ed25519 public key to share with funders.

```bash
aoc capsule-pubkey
```

### `aoc capsule-contract`

Create a Mnemosyne ContractAction and post the `contract_id` as a comment on a GitLab issue.

```bash
aoc capsule-contract \
  --title "Short label (≤60 chars)" \
  --description "What work is requested" \
  --repo mygroup/myproject \
  --issue 42
```

Authentication via device-auth flow (once); tokens cached at
`~/.config/pinard/mnemosyne-tokens.json`.

### `aoc capsule-redeem`

Redeem a funded Mnemosyne contract and print a bare Claude API token to stdout.
Called automatically by the worker startup path when `PINARD_CAPSULE_CONTRACT` is set;
you normally don't call it directly.

```bash
aoc capsule-redeem <contract_id>
```

### `aoc capsule-post-result`

Render `<rundir>/capsule-report.md` to HTML and PATCH the contract's result URL.
Called by the babysitter at the end of a funded run.

## `aoc` — memory & ontology

### `aoc memory-status`

Show unified memory health: Engram replication, SurrealDB ingestion, and wiki curation
in three tabbed sections, followed by a one-line verdict.

```bash
aoc memory-status                            # auto-detect vignoble from cwd or NATS_VIGNOBLE
aoc memory-status --vignoble myproject       # explicit vignoble
aoc memory-status --json                     # raw JSON output
aoc memory-status --timeout 5000            # request timeout in milliseconds (default 10000)
```

| Section | What it reports |
|---------|----------------|
| **Engram** | Reachable (true/false) + pending cloud-sync count |
| **SurrealDB** | Per-group ingest lag, failed-write count, last ingest time |
| **Wiki** | Per-group doc count, auto-serve count, git-publish status, last wiki commit |

The command exits non-zero when any group has lag > 0, failed writes > 0, wiki docs
exist but have not been git-committed/pushed (`git_publish_ok=false`), or the ingester
is unreachable. Suitable as a health check in scripts and CI.

### `aoc ontology validate`

Validate a domain ontology YAML file against the Pinard meta-schema. Exits 0 on
success, non-zero on failure — suitable as a CI gate in domain repos.

```bash
aoc ontology validate path/to/my-pipeline.yaml
# ✓ my-pipeline.yaml is valid
```

### `aoc ontology inspect`

Print the composed ontology for a given `group_id` — shows entity roles, edge types,
and the core/domain version stamp.

```bash
aoc ontology inspect --group-id my-pipeline-build
# Composed ontology for group_id="my-pipeline-build" (core 1.0.0 + domain my-pipeline@1.0.0):
# Entity roles:  task  step  verdict  decision  gate  …  pipeline_job  …
# Edge types:    DependsOn (3 pairs)  …
```

See [The Ontology & Domain Extension](/docs/memory-ontology/) for how to write a
domain file and configure the domain loader (`PINARD_ONTOLOGY_DIRS`).

## `aoc` — admin & internals

```bash
aoc create-user --name alice --vignoble myproject     # NATS account/user (requires nsc)
aoc cleanup archive --project <p> --change <name>     # archive a completed openspec change
```

`aoc create-user` only applies to self-hosted **nsc/JWT** NATS deployments. Config-account
deployments (Helm/Vault-managed, e.g. production `pinard-nats`) use a single
pre-configured account with unrestricted permissions, so this command is a no-op there —
running it without `nsc` installed now prints an explanation instead of a bare "not
found" error.

Additional internal subcommands (`resolve-model`, `vigne-args`, `env-exports`,
`ensure-proxy-provider`, `governance-prompt`, `nats-publish`) exist for the launcher and
daemon; you won't normally call them directly.

`aoc governance-prompt --process <name> [--host <gitlab-host>]` prints the no-op
bootstrap prompt used for process workers. The launcher calls it automatically when
no explicit prompt is given; expose it here so custom launch scripts can stay in sync
without hard-coding the text.
