# Pinard — Development Rules

## Architecture

Pinard is a multi-agent orchestration system. Components communicate exclusively via NATS JetStream.

| Component | Code | Runs as |
|-----------|------|---------|
| Daemon | `cmd/aoc/daemon.go` | self-supervising background process (`aoc daemon start`, PID file in `.state/daemon.pid`) |
| Régisseur | `pi-extension/pinard/` (no `PINARD_PARCELLE`) | Pi extension (Opus), `conductor` tmux session, window 0 `[régisseur]` — vignoble general lane + overview |
| Maître | `pi-extension/pinard/` (`PINARD_PARCELLE=<name>`) | Pi extension (Opus), one tmux window per parcelle in the `conductor` session |
| Vendangeur 🧺 (worker) | `pi-extension/worker/` | Pi extension (Sonnet), spawned tmux sessions |
| CLI | `cmd/aoc/` | Go binary (`~/.local/bin/aoc`) |
| Launcher | `bin/pinard` | Shell script (starts the régisseur, `--maitre <parcelle>`, or `--worker`) |

> **Terminology — vendangeur vs worker.** The harvester role is shown as **vendangeur 🧺** in all user-facing surfaces (régisseur dashboard, status/session lines, `aoc dashboard`, `aoc status`, tool labels, slash-command help). The **code keeps the name `worker`** — identifiers, the `pi-extension/worker/` extension dir, `WORKER_*` env vars, the `models.worker` config key, the `--worker` launcher flag, `worker-policy`, tool names (`list_workers`/`kill_worker`/`interrupt_worker`), and NATS durable names are all unchanged. When this doc says "worker" it means the code-level concept; the UI says "vendangeur".

The daemon is always running (started via `aoc daemon start`, no systemd dependency). It handles: MR watching, issue watching, schedule evaluation, auto-spawn, direct dispatch to worker inboxes, and maître liveness. The régisseur/maîtres are optional — they provide LLM-powered orchestration and user interaction but the system works without them.

### Three-tier orchestration (parcelle maîtres)

One vignoble = one **régisseur** + N per-parcelle **maîtres** + M **workers**:

- **Régisseur** (`conductor` session, window `[régisseur]`): the general lane — vignoble overview + unparceled / untriaged / vignoble-level events. Persistent session at `.state/regisseur-session.jsonl` (not under `parcelles/` — it is not a workstream). Does **not** ingest the per-parcelle agent-events firehose (`IS_MAITRE` gates that consumer) — it uses the KV overview (`list_parcelles`) plus the notifications/issues/schedules consumers.
- **Maître** (per parcelle): a parcelle-scoped conductor. Consumes only `pinard.<v>.parcelles.<parcelle>.agents.*.events.>`. Conductor-grade model. Persistent, resumable session at `parcelles/<parcelle>/session.jsonl` (`pi --session`, resume-if-exists). Always runs autonomously on its parcelle's events (its durable JetStream consumer ingests and steers the session regardless of tmux attach state); attaching to its window lets you watch and steer it interactively. Runs as a tmux window; **not** a separate tmux server or a subagent-package child.
- **Attach** = native tmux window switch (`aoc maitre attach --parcelle <name>` → spawns-if-missing then `select-window`; also the `/parcelle <name>` command and `attach_parcelle` tool). "Steer" a maître = type in its window.
- **Liveness**: the daemon owns it — `recoverMaitres` (in orphan recovery) ensures a window exists for every parcelle with live work, when the `conductor` session is running. Idle-exit is deferred (active maîtres stay warm).

## Critical Invariants

### Event flow

- Daemon publishes events to JetStream streams (guaranteed delivery)
- Daemon dispatches actionable events (`pipeline_failed`, `review_comment`, `main_pipeline_failed`, `tag_pipeline_failed`) directly to worker inboxes — no conductor dependency
- Conductor receives events for visibility/dashboard but does NOT dispatch to workers
- `auto-pinard` labeled issues are spawned by the daemon, not the conductor

### Worker lifecycle

1. Spawn: daemon (`autoSpawnForIssue`) or conductor (`spawn_agent` tool) calls `aoc spawn`
2. Work: worker reads issue/prompt, makes changes, opens MR, calls `track_mr`
3. Monitor: daemon watches MR (pipeline, reviews, approvals)
4. Auto-merge: daemon merges when CI passes + approved + no unresolved reviewer threads
5. Post-merge: daemon monitors main pipeline via `merge_commit_sha`
6. Reap: `mrs.go`'s `reapWorker` is the **single deterministic teardown point** (kills tmux + removes KV + drops the MR-watch entry). It fires at the terminal condition — MR merged **and** (post-merge main pipeline succeeded, or post-merge monitoring is disabled), or MR closed — uniformly for process and non-process workers (no more "let the process self-terminate"). It does **not** fire while a `main_pipeline_failed` follow-up is outstanding. (Event-driven respawn + location-aware liveness are not-yet-implemented.)

### Standalone (remote) worker

A worker can run on a different machine that has **no vignoble directory** (e.g. an HPC
node inside the genomics Singularity sandbox), talking to the vignoble's conductor purely
over NATS. Launch with `pinard --worker --vignoble-name <name>` (instead of `--vignoble
<path>`): the launcher skips vignoble-dir resolution and all `$VIGNOBLE`-derived setup.

- NATS creds come from `~/.config/pinard/credentials.yaml` (via `aoc env-exports`) — **not**
  the vignoble. `--vignoble-name` only supplies the NATS namespace (`NATS_VIGNOBLE`).
- Standalone requires an explicit concrete `--model <id>` (model resolution needs the
  vignoble) and gets babysitter args from `--args`, or synthesizes them via the
  vignoble-free `aoc vigne-args --repo <group/proj> …`.
- The process definition (build/release) is baked into the project repo at
  `pinard/<proc>/process.js` and resolves from `$(pwd)/pinard/<proc>/process.js`.
- **Resume after failure:** babysitter resumes a run from `<runs-dir>/<RUN_ID>/run.json`,
  so a standalone worker must get a **stable RUN_ID** (defaults to `<project>-<process>`,
  not the PID-based session) and a **persistent `--runs-dir`** (env vars don't cross
  `singularity --containall`; `--runs-dir` is an arg, point it at a bound persistent
  path). Re-running with the same RUN_ID continues where it left off. Daemon-side
  orphan-recovery does not manage remote runs.
- Entry point for genomics: `~/genomics-workers/pinard/genomics-build/run.sh` (env-driven:
  `VIGNOBLE_NAME`, `MODEL`, `PROJECT`, `PROCESS`, `REPO`, …).

### Three communication channels (conductor → worker)

- **Main inbox** (`…inbox`): actionable work via JetStream. Durable, survives restarts.
- **BTW** (`…btw`): parallel questions. Real-time core NATS, no persistence.
- **Interrupt** (`…interrupt`): cancels current turn. Real-time core NATS.

### NATS JetStream

- All event publish MUST use JetStream publish (not core NATS) for stream-captured subjects
- Daemon ensures streams exist on connect (creates/updates if missing)
- **Agent subjects are parcelle-scoped** via a literal `parcelles` segment: `pinard.<v>.parcelles.<parcelle>.agents.<sid>.{events.<type>,inbox,btw,interrupt}`. Agent events always carry a parcelle (a worker is spawned with one, defaulting to the project). Build/parse them with the centralized helpers in `internal/pnats/subjects.go` (Go) and the `agentBase`/`parseAgentSubject` helpers in the extensions — never hand-format. Vignoble-level subjects (`issues`/`schedules`/`notifications`) are **not** parcelle-scoped (régisseur-consumed).
- Stream subjects use wildcards: `pinard.*.parcelles.*.agents.*.events.>` (all vignobles/parcelles); maître consumers filter to `pinard.<v>.parcelles.<parcelle>.agents.*.events.>`
- Consumers are durable, `deliver_policy: "all"` for catch-up on restart
- BTW and interrupt channels are real-time only (no stream)

### Worker session naming & tmux topology

- One tmux server per vignoble (socket `pinard-<vignoble>`). The `conductor` session holds the `[régisseur]` window (the general lane) + one window per active parcelle maître. Workers are flat tmux sessions on the same server.
- Worker session names are **parcelle-leading**: `<parcelle>--<project>-<id><rand>` (`workerSessionName` in `cmd_spawn.go`), so `tmux ls` / the prefix+`f` picker is self-describing and filterable by parcelle. The vignoble token is dropped (socket already scopes it). Any name/parcelle used as a tmux target is passed through `session.SanitizeName` (forbids `.`/`:`/whitespace).
- Two-level view: `tmux -L pinard-<v> ls` → the `conductor` session + workers; `tmux -L pinard-<v> list-windows -t conductor` (`aoc maitre list`) → maîtres.

### Permissions (pi-permission-system)

- Conductor: `<vignoble>/.pi/agent/pi-permissions.jsonc` — bash: allow all
- Workers: `<worktree>/.pi/agent/pi-permissions.jsonc` — bash: allow, external_directory: deny
- Per-vigne override: `<vignoble>/vignes/<project>/.pi/agent/pi-permissions.jsonc` (symlinked into worktree at spawn)

### Daemon lifecycle & hot-reload (no systemd)

- The daemon self-daemonizes: `aoc daemon start` re-execs `aoc daemon` detached (`Setsid`), logging to `logs/aoc-daemon.log`, and records `.state/daemon.pid`. `aoc daemon {stop,restart,status}` manage it; `aoc status` reads the PID file + liveness (signal 0).
- The detached child inherits secrets from `~/.config/pinard/env` and an augmented `PATH` (nvm node bin so it can find `pi`) — see `daemonChildEnv` in `cmd_daemon_ctl.go`.
- Hot-reload is in-process: a `reloader` tick polls the mtimes of the `aoc` binary, `vignes.yaml`, and `schedules.yaml`; on any change it `syscall.Exec`s itself in place (same PID, same log fd, same cwd) — equivalent to the old systemd restart.
- No boot persistence: the daemon does not auto-start on login. `aoc init` starts it once (`aoc daemon restart`) and tears down any legacy systemd units (`removeLegacySystemdUnits`).

### Git authorship

- `GIT_AUTHOR_*` = user (from `git config --global`) — they directed the work
- `GIT_COMMITTER_*` = pinard service account (from credentials.yaml) — it pushed
- `GITLAB_TOKEN` (not GLAB_TOKEN) for glab API calls as pinard user

### Web terminal access (read-only browser view of tmux over NATS)

Browser read-only view of a vendangeur's tmux session, reached via a signed link.
Phases 1 (signed-link transport) and 2 (OIDC SSO + operator discovery) are built;
the control-room index and writable "steer" are later phases (not built yet).

Transport (never JetStream — terminal bytes are ephemeral):

```
browser ⇄ WebSocket ⇄ gateway (k8s) ⇄ core NATS ⇄ responder (tmux host) ⇄ tmux attach -r
```

- **Gateway** (`cmd/webterm-gateway`, `internal/webterm/gateway.go`): k8s service
  serving the embedded xterm.js frontend + WebSocket. Verifies the signed link,
  mints a short-lived HMAC **grant**, and bridges the WS to per-viewer NATS
  subjects. No inbound path to the tmux host is needed. Deployed by
  `charts/pinard-webterm-gateway` at `Host(pinard.example.com) &&
  PathPrefix(/sessions)` (`web` entrypoint, internal-only host).
- **Responder** (`internal/webterm/responder.go`): runs on every tmux host —
  in-process in the **daemon** (pinard host) and via `aoc webterm-responder` on
  standalone/HPC hosts (started by `bin/pinard --worker`). Subscribes to
  `pinard.<v>.webterm.req`, **verifies the gateway grant before attaching**
  (anyone with NATS creds could publish), then `tmux attach -r` via an embedded
  PTY (`creack/pty`) and streams frames back with flow control (coalescing,
  bounded buffer, rate cap). Tears the PTY down on disconnect/idle/target-exit.
- **Signed links** (`internal/webterm/link.go`): `…/sessions?v=&target=&exp=&sig=`
  (`sig = hmac-sha256(v|target|exp)`, where `v` is the vignoble). Built Go-side by
  `aoc track-mr` (auto-post on MRs, gated by `webterm.post_links`), `aoc webterm-link`
  (CLI), and the régisseur/maître **`/webterm [name]`** slash command (on-demand,
  picker when no name). Once SSO is enabled a posted/shared link is not anonymously
  usable — the viewer must still authenticate; the signature scopes them to that one
  session.
- **Multi-vignoble**: one gateway serves many vignobles. The link + grant carry the
  vignoble `v`; per request the gateway checks `v` is served, routes to
  `pinard.<v>.webterm.*`, and resolves the owner for `v`. The **served set is
  self-maintaining**: with `gateway.vignobles` empty (default) it serves any vignoble
  that has published an owner to the `pinard-vignobles` KV — a new vignoble's daemon
  publishes on startup, so no gateway redeploy; unknown vignoble → 403. An explicit
  `gateway.vignobles` / `WEBTERM_VIGNOBLES` list is an optional allowlist. A single
  (per-tenant) secret verifies all of a tenant's vignobles — the vignoble is bound
  into the signature; responders reject a grant whose vignoble ≠ their own.
  (Per-tenant secrets / isolation = the separate `multi-tenant-webterm` change.)
- **Subjects** (`internal/webterm/subjects.go`): per-viewer `…webterm.<id>.{out,in,ctl,evt}`.
- **Auth (Phase 2, `internal/webterm/auth.go`)**: when `webterm.auth` is configured
  (issuer + client_id) the gateway runs the **Cognito OIDC login flow itself**
  (Authorization Code + **PKCE**, public client — no secret; not oauth2-proxy/
  Traefik), validates the ID token against the pool JWKS (`iss`/`aud`/`exp`/
  `token_use=id`), and stores a stateless HMAC-signed session cookie. Absent →
  Phase-1 signed-link-only. Routes: `/sessions/auth/{login,callback}`.
- **Authorization** (`authz.go`, deny-by-default): **operator** (JWT
  `preferred_username` == vignoble owner) → any target; **viewer** (valid signed
  link) → that target; else denied. Owner is discovered from the `pinard-vignobles`
  KV (`operator.go`), published by the daemon/responder from `credentials.yaml`
  `nats.user` (or the `owner:` override) — no manual mapping. Never uses the AD
  `groups` claim.
- **Control-room index** (`web/index.html`, `gateway.go`): `/sessions` with no
  `target` serves an authed **operator-only** two-pane index — sidebar of the
  vignobles you own (`OwnerStore.OwnedBy`), main pane of the selected vignoble's
  **live** sessions (régisseur, maître windows, vendangeurs) enumerated over NATS
  (`ListSubject` → responder `tmux list-sessions`/`list-windows`; grant-verified,
  silent on failure). APIs: `/sessions/api/{vignobles,sessions}`. Vendangeur rows
  are enriched with parcelle (name prefix) + state (`pinard-agents` KV). Each row
  links to the read-only terminal view. Viewers (scoped-link holders) never see it.
- **Secrets** (`credentials.yaml` `webterm:` block): `base_url`, `link_secret[_env]`
  (link builder ↔ gateway), `grant_secret[_env]` (gateway ↔ every responder),
  `link_ttl`, `idle_timeout`, `max_viewers`, `post_links`; and `auth:` with
  `issuer`, `client_id`, `redirect_url` (default `<base_url>/sessions/auth/callback`,
  must be registered in Cognito), `scopes`, `cookie_secret[_env]`, `session_ttl`.
  The gateway needs link+grant+cookie secrets; a responder needs only the grant secret.
- **Writable "steer"** (`authz.go`, `responder.go`, `web/term.html`): operator-only,
  opt-in via `?mode=rw` (a toggle in the terminal header). The gateway mints a
  `ModeRW` grant only for an operator who requested it (viewers are always RO),
  forwards browser keystrokes to `InSubject`, and the responder attaches **without
  `-r`** and writes input to the PTY — single-writer, gated on the grant at both
  ends; audited as `mode=rw`.
- **Grouped-session navigation** (`responder.go`): a window target
  (`session:window`, e.g. `conductor:3`) is viewed via a per-viewer **grouped
  session** (`tmux new-session -t <base>` + `select-window`), so opening a
  régisseur/maître window doesn't move the operator's active window; torn down with
  the viewer. Plain vendangeur sessions attach directly.
- **Supervised (HPC) backend attach repaint + PTY size policy**
  (`internal/webterm/supervisor.go`): `RunSupervised`'s `supervisedBackend` has
  no tmux to repaint an attaching viewer's pane, so `ptyFanout` keeps a bounded
  (`replayCapBytes`, 128 KB) rolling buffer of recent PTY output and replays it
  to every new `subscribe()` call *before* any live chunk, so a late-joining or
  re-attaching viewer sees the current screen immediately instead of blank until
  the child next writes. Separately, the supervised PTY has two independent size
  sources — the local controlling terminal's SIGWINCH-driven `syncSize()` and a
  browser viewer's `CtlResize`/attach size — so the arbitration policy is an
  explicit decision, **viewer-attached wins**: while any viewer is attached
  (`supervisedBackend`'s atomic `viewers` counter > 0), `syncSize()` is a no-op;
  when the last viewer detaches, `syncSize()` runs once more to resync to the
  local terminal. Among multiple concurrent viewers, whichever resizes most
  recently wins, as before. `TmuxBackend` is unaffected by either change.
- **Known limitations:** one shared NATS connection (a dedicated account for terminal
  traffic is a follow-up). Grouped sessions pin `window-size manual` so viewer
  dimensions do not affect the operator's terminal.

## File layout

```
cmd/aoc/              — Go CLI binary
  main.go             — cobra root command
  daemon.go           — `aoc daemon` (all watchers as goroutines)
  cmd_spawn.go        — `aoc spawn` (worktree + tmux + policy + env)
  cmd_config.go       — `aoc config set/get` (dot-path YAML manipulation)
  cmd_status.go       — `aoc status` (tracked MRs, issues, workers, schedules)
  cmd_init.go         — `aoc init` (scaffold vignoble; starts daemon, removes legacy systemd units)
  cmd_daemon_ctl.go   — `aoc daemon start/stop/restart/status` (self-daemonize + PID file)
  cmd_notify.go       — `aoc notify`
  cmd_track_mr.go     — `aoc track-mr`, `aoc untrack-mr`
  cmd_maitre.go     — `aoc maitre spawn/attach/list` (per-parcelle maître windows)
  cmd_webterm.go      — `aoc webterm-responder` (host responder), `aoc webterm-link` (signed URL)
cmd/webterm-gateway/  — k8s web-terminal gateway binary (HTTP+WS bridge over NATS)
internal/
  config/             — credentials.yaml, vignes.yaml, schedules.yaml
  state/              — Write-through state (atomic file writes, flock, reload-before-update)
  pnats/              — NATS client (WebSocket, JetStream publish, stream ensure, KV); subjects.go = parcelle-scoped subject builders/parser
  gitlab/             — GitLab API (direct HTTP, MR/issue/note/pipeline/approval/discussion)
  git/                — Git operations (fetch, pull, worktree)
  session/            — tmux session/window management (SpawnWorker, EnsureWindow, SanitizeName)
  cron/               — Cron expression matching
  watcher/            — MR, issue, scheduler, orphan/maître recovery logic
  webterm/            — Web terminal (Phase 1): signed links, HMAC grants, NATS subjects, host responder, k8s gateway + embedded xterm.js
bin/
  pinard              — Launcher (régisseur, --maitre <parcelle>, or --worker mode)
pi-extension/
  pinard/             — Régisseur/maître extension (JetStream consumers, tools); parcelle-scoped when PINARD_PARCELLE is set
    index.ts          — Entry point (event handling, tools, NATS setup)
lib/
  logic.ts            — Pure functions (formatEventMessage, buildDedupeKey, resolveEventParcelle)
  classify.ts         — Event classification + subject builders (shared with tests)
  worker/             — Worker extension (inbox consumer, aoc_notify, KV state)
    index.ts          — Entry point
  shared/             — Shared tools (read_issue, update_issue, track_mr, proxy provider)
charts/               — Helm charts (pinard-nats, pinard-engram, pinard-website, pinard-webterm-gateway)
tests/                — Four-layer test harness
```

## Vignoble layout

```
vignoble-<name>/
  vignes.yaml          — Vigne registry + models config
  schedules.yaml       — Scheduled spawns (cron)
  .state/              — mr-watcher.yaml, issue-watcher.yaml, scheduler-runs.yaml, daemon.pid
  .pi/agent/           — Conductor pi-permissions.jsonc
  .engram/             — Engram memory database (per-vignoble)
  logs/                — aoc-daemon.log, conductor.log, nats-events.log
  vignes/              — Per-vigne config (VIGNE.md, .pi/agent/pi-permissions.jsonc)
  PINARD.md            — Conductor system prompt (symlink to pinard repo)
```

## Engram cloud replication

Each vignoble has a local-first Engram memory store at `.engram/engram.db`. Cloud replication to `https://engram.example.com` is opt-in, configured in `credentials.yaml`:

```yaml
engram:
  server: https://engram.example.com
  cloud_token_env: ENGRAM_CLOUD_TOKEN  # env var holding the bearer token
  # cloud_token: <literal>             # alternative: literal value
```

**Per-vignoble store isolation**: `bin/pinard` derives a deterministic `ENGRAM_PORT` for each vignoble — `cksum(vignoble-name) % 1000 + 7500` — and exports it before launching pi and any child processes. gentle-engram (the `mem_*` MCP backend) checks this port rather than the global default (7437), so a stray `engram serve` on 7437 cannot capture another vignoble's memories. If nothing is on the computed port, `engram serve` is spawned with the inherited `ENGRAM_DATA_DIR` and `ENGRAM_PORT`, giving fully isolated per-vignoble storage. An explicit `ENGRAM_PORT` in the environment overrides the computed value (for operators who need to pin a port). The launcher warns at startup if a serve is detected on 7437 (informational) or if the computed port is already occupied by a serve for a different vignoble (collision warning).

**Token source-of-truth**: `ENGRAM_CLOUD_TOKEN` env var (exported by `aoc env-exports` from `credentials.yaml`). The `.engram/cloud.json` file intentionally has an empty `token` field — `engram cloud config` only accepts `--server`, not `--token`. Engram autosync reads `ENGRAM_CLOUD_TOKEN` from the environment directly. An empty `token` in `cloud.json` is not a mismatch — it is expected.

**Startup flush**: `bin/pinard` calls `engram sync --cloud --project <vignoble>` after enrollment on every start. This drains any pending mutations accumulated while autosync was unavailable (e.g., daemon restart without the token set). Non-fatal — failure logs a warning; autosync retries in the background.

**Periodic flush**: A background `_engram_sync_loop` (every 5 min by default, overridable via `ENGRAM_SYNC_INTERVAL` in seconds) runs while the agent is alive, pushing writes that accumulate during a long session. Self-exits when the parent shell exits.

**Exit flush**: After pi exits, `_run_with_engram_flush` fires a final `engram sync --cloud` to drain any writes made in the last interval before the process dies.

**Manual flush**: `engram sync --cloud --project <name>` (requires `ENGRAM_CLOUD_TOKEN` and `ENGRAM_CLOUD_SERVER` in env).

**Autosync**: `ENGRAM_CLOUD_AUTOSYNC=1` is set by the launcher; the engram MCP server (running under pi) picks it up and replicates each `mem_save` in the background.

**Failure visibility**: `enroll` and `sync` failures are now logged as warnings to stderr (instead of being silently swallowed), so replication problems are visible in the session log.

## Config: vignes.yaml

```yaml
gitlab_host: gitlab.example.com
gitlab_group: exohub
auto_merge: true

models:
  conductor:
    id: claude-opus-4-6
  worker:
    id: claude-sonnet-4-6

vignes:
  exo-cli:
    path: ~/exo-cli
    repo: exohub/exo-cli
    auto_merge: true
    model:
      id: claude-opus-4-6   # override worker model for this vigne
```

## Building

```bash
cd cmd/aoc && make install   # builds + installs to ~/.local/bin/aoc
go test ./internal/... ./cmd/aoc/   # Go tests
cd tests && npm test         # TS unit + integration + contract tests
cd pi-extension && npm run typecheck   # type-check the Pi extensions
```

### Pi extension type-checking

Pi loads extension `.ts` files **untranspiled** — there is no build step, so a
typo like `proj is not defined` would otherwise only surface at runtime inside a
worker. `pi-extension/tsconfig.json` + `npm run typecheck` (`tsc --noEmit`) is the
only gate that catches this. A pre-commit hook (`scripts/pre-commit-typecheck.sh`,
installed by `./install`) blocks commits touching `pi-extension/*.ts` when it fails;
bypass with `git commit --no-verify`.

**`pi-extension/pinard-pi-augment.d.ts`** augments Pi's `ExtensionAPI` with
`emit`, `setStatus`, and a custom-event `on()` overload — capabilities Pinard uses
at runtime that the published `@earendil-works/pi-coding-agent` types omit. Without
it the type-check drowns in false errors. The devDeps in `pi-extension/package.json`
are pinned to the **runtime** Pi version so the check reflects what actually runs;
when bumping Pi, bump them together and trim the augment file if upstream types
have since caught up.

**Runtime versions:** Pi **≥0.80.2** (currently `0.80.6`) and Node **24** (`>=24 <26`).
The Node floor is driven by the `pi-sessions` package (Pi's own floor is 22.19). The
global Pi install / `make dist`-vendored runtime and `nvm default` must be on these —
keep them in lockstep with `pi-extension/package.json`. `.nvmrc` pins 24.

## Distribution (`make dist`)

`make dist` produces `dist/pinard-linux-x64.run` — a single self-extracting
[makeself](https://github.com/megastep/makeself) archive (~172M) for **Linux/glibc
x64 only**. It bundles the static `aoc` binary, the launcher, the Pi extensions
(with `node_modules`), and a **vendored Node + Pi runtime** under `runtime/`, so the
target machine needs no Node/nvm/npm — only the thin host CLIs `tmux git glab fzf`.

- `dist/build.sh` stages the tree, vendors `node` + the global Pi install, prunes
  non-Linux native artifacts (`darwin*`/`win32*` prebuilds) and non-runtime bloat
  (`deps/babysitter/library`, docs, dev typecheck tooling), then packages via the
  **patched** vendored makeself in `dist/.makeself/` (see its README — stock makeself
  truncates archives over ~100k files; the patch uses `tar --null -T -`). GNU tar
  format is required for deep `node_modules` paths.
- `dist/pinard-setup.sh` is the post-extract hook: installs to `$PINARD_HOME`
  (default `~/.pinard`), symlinks `bin/{aoc,pinard,pinard-picker}` into `~/.local/bin`,
  and scaffolds user config (credentials template, pi-permissions, worker-policy, CA
  certs). It shares this logic with `install` (dev-from-source path).
- The launcher (`bin/pinard`) resolves its runtime as **bundled > nvm > PATH**, and
  delegates all YAML/JSON parsing to `aoc` subcommands (`resolve-model`, `vigne-args`,
  `env-exports`) — no `python3` at runtime. Keep the bundled Pi version in lockstep
  with the type-check devDeps.
- Build temp goes to `/data/storage/tmp` (override `PINARD_DIST_TMPDIR`); `/tmp` is
  too small for the ~1G staging tarball.

## Testing

| Layer | What it tests |
|-------|---------------|
| Unit (`tests/unit/`) | Pure functions: dedup keys, formatting, tool ownership |
| Integration (`tests/integration/`) | NATS consumer lifecycle, KV, auth |
| Contract (`tests/contract/`) | Full event flows with real NATS |
| Go unit (`internal/*/..._test.go`) | State, watcher logic, cron, config |

