<p align="center">
  <img src="website/static/images/brand/pinard-github-logo.png" alt="Pinard" width="220">
</p>

<h1 align="center">Pinard</h1>

<p align="center"><strong>One conductor. Many agents. One harvest.</strong></p>

<p align="center">
  A distributed engine for running fleets of LLM agents through reliable,<br>
  resumable, and auditable semi-deterministic loops.
</p>

<p align="center">
  <a href="website/content/docs/overview.md">Overview</a> ·
  <a href="website/content/docs/getting-started.md">Getting started</a> ·
  <a href="website/content/docs/cli-reference.md">CLI reference</a> ·
  <a href="website/content/docs/architecture.md">Architecture</a>
</p>

---

Pinard wraps capable but unpredictable agents in **semi-deterministic loops**:
ordinary code owns the control flow, while the model supplies bounded judgment
inside individual steps. Every step is journaled, so interrupted work can resume
instead of starting over.

Agents can run across repositories, workstations, and HPC nodes. They communicate
over NATS JetStream, retain distilled knowledge in persistent memory, and remain
observable from a browser without requiring inbound SSH.

Three pillars hold the system together:

> **Deterministic control** · **Non-deterministic agents** · **Persistent memory**

<p align="center">
  <img src="website/static/images/docs/docs-estate-overview-readme.png" alt="A labeled vineyard estate showing central control, agent parcels, deterministic routes, and persistent memory" width="900">
</p>

<p align="center"><em>One coordinated estate: independent workers connected by deterministic routes to shared control and durable memory.</em></p>

## Why Pinard?

| A free-running agent | A Pinard loop |
|---|---|
| Lets the model decide the path | Keeps sequencing, branching, and stopping in code |
| Loses progress when the session crashes | Resumes from a stable run journal |
| Returns prose that downstream steps must interpret | Uses bounded tasks with structured outputs |
| Hides what happened inside a long conversation | Records steps, outcomes, gates, and verdicts |
| Relearns the same operational lessons | Recalls curated knowledge across sessions |
| Is difficult to coordinate across hosts | Runs as one worker in a distributed fleet |

The result is not a more elaborate prompt. It is an execution model for agent work
that needs to survive real infrastructure, real failures, and real review cycles.

## How work flows

<p align="center">
  <img src="website/static/images/docs/semi-deterministic-loop-readme.png" alt="A labeled six-stage agent process interrupted after step three and resumed from step four using its completed run journal" width="900">
</p>

<p align="center"><em>Fixed control, bounded judgment: after a failure, the journal skips completed work and resumes at the next open step.</em></p>

1. A schedule, event, GitLab issue, operator, or conductor triggers work.
2. The daemon spawns a **vendangeur** (worker), normally in an isolated git
   worktree, with a stable run ID.
3. A versioned `process.js` advances through deterministic code steps and bounded
   LLM tasks.
4. Each completed task and breakpoint is written to the run journal.
5. If the worker or host fails, the same run resumes at the next incomplete step.
6. Reusable discoveries are distilled into memory, and the worker is reaped when
   the loop reaches a terminal outcome.

Read [The Semi-Deterministic Loop](website/content/docs/semi-deterministic-loop.md)
for the execution model and
[Authoring Processes](website/content/docs/authoring-processes.md) to build one.

## What Pinard provides

- **Processes as code** — versioned JavaScript runbooks with typed tasks,
  deterministic branches, breakpoint gates, and terminal results.
- **Journaled execution** — stable run IDs make crashes, reboots, and multi-day
  work resumable and auditable.
- **Distributed agents** — workers communicate exclusively over NATS and can run
  locally, on another workstation, or inside an HPC/Singularity environment.
- **Fleet orchestration** — a daemon handles mechanical coordination; optional
  régisseur and maître agents add conversational estate- and workstream-level control.
- **Persistent memory** — local-first, curated knowledge is injected at boot and
  fetched in full only when relevant.
- **GitLab automation** — issue dispatch, isolated worktrees, MR tracking, review
  forwarding, bounded pipeline repair, optional auto-merge, and deterministic cleanup.
- **Scheduled work** — cron-driven spawns include missed-run backfill and explicit
  outcome events.
- **Live browser terminals** — observe remote tmux sessions over NATS, read-only by
  default, without opening inbound SSH.
- **Cuvée branches** — combine concurrent changes through an intermediate branch
  before they reach the repository's default branch.
- **Portable funding contracts** — optional Capsule Protocol support can fund a
  worker without coupling the protocol to a specific application.

## Built-in SWE loop

Pinard includes one production application of the engine: a GitLab issue-to-merge
loop.

```text
authorized issue
    → isolated vendangeur
    → implementation and tests
    → merge request
    → review and pipeline feedback
    → merge or explicit terminal failure
    → post-merge monitoring and cleanup
```

Review comments and failed pipelines return to the worker that owns the change.
Pipeline repair is bounded by a circuit breaker. Auto-merge is **off by default**;
when enabled, it still requires a green pipeline, approval, no unresolved threads,
and a non-draft MR.

This is one process, not the definition of Pinard. The same engine can drive data
operations, audits, recurring maintenance, or any workflow expressed as bounded
agent tasks inside deterministic control flow.

See [The SWE Process](website/content/docs/swe-process.md) for the complete lifecycle.

## Quick start

### 1. Install from source

Pinard requires Go, Node.js ≥ 22.19.0, Pi, `git`, `glab`, `tmux`, and `fzf` when
installed from a checkout.

```bash
git clone --recurse-submodules https://github.com/Genentech/pinard.git
cd pinard
./install
```

Linux release bundles include the Node and Pi runtime. See
[Getting Started](website/content/docs/getting-started.md) for both installation
paths and exact prerequisites.

### 2. Pull a container image (optional)

Pre-built images are published to GitHub Container Registry on every release.
No credentials are needed to pull public images.

> **Note on `aoc`:** for a host install, the `aoc` CLI and daemon run on your
> control host (laptop, workstation, or CI runner) — not inside a container;
> install them from the source checkout or a Linux release bundle (see step 1).
> The `pinard` image below is the exception — it bundles `aoc` and the full
> agent runtime for running a worker (or a régisseur/maître) without a host
> install (e.g. on k8s or an HPC node).

| Image | Contents | Use |
|---|---|---|
| `ghcr.io/genentech/pinard` | agent/worker: vendored Node/Pi runtime, `aoc`, `engram` | run a vendangeur (worker), régisseur, or maître in a container; role selected by the args passed to `pinard` |
| `ghcr.io/genentech/pinard-backend` | webterm-gateway + memory services + static docs site | k8s deployment of the Pinard backend services |
| `ghcr.io/genentech/pinard-webterm-gateway` | standalone web-terminal gateway only | lighter k8s deployment when memory services run elsewhere |

```bash
# Agent/worker image (vendangeur/régisseur/maître)
docker pull ghcr.io/genentech/pinard:latest

# k8s backend services image (webterm-gateway + memory services + static docs site)
docker pull ghcr.io/genentech/pinard-backend:latest

# Standalone web-terminal gateway
docker pull ghcr.io/genentech/pinard-webterm-gateway:latest
```

Run a standalone/HPC-style worker:

```bash
docker run --rm \
  -e PINARD_UNCORK_URL=… \
  -e PINARD_POUR_URL=… \
  ghcr.io/genentech/pinard:latest \
  --worker --vignoble-name myproject --model claude-sonnet-4-6
```

Run the standalone web-terminal gateway:

```bash
docker run --rm \
  -e NATS_VIGNOBLE=myproject \
  -e PINARD_NATS_URL=nats://nats.example.com:4222 \
  -e PINARD_NATS_PASSWORD=… \
  -p 8080:8080 \
  ghcr.io/genentech/pinard-webterm-gateway:latest
```

> **Engram**: the memory backend ([Gentleman-Programming/engram](https://github.com/Gentleman-Programming/engram))
> is a third-party binary. It is bundled in the `pinard` (agent/worker) image
> (a sandboxed worker has no daemon to serve it, so it self-serves), but NOT in
> `pinard-backend`/`pinard-webterm-gateway` — install it separately from the
> upstream release page for those, or mount it into the container.

### 3. Configure credentials

Pinard has no built-in hosts or credentials. Copy the template, provide a dedicated
GitLab service account and NATS connection, then export the referenced secrets:

```bash
mkdir -p ~/.config/pinard
cp credentials.example.yaml ~/.config/pinard/credentials.yaml

export PINARD_GITLAB_TOKEN="glpat-…"
export PINARD_NATS_PASSWORD="…"
```

See [Configuration](website/content/docs/configuration.md) for the complete schema.

### 4. Create an estate and register a repository

```bash
aoc init myproject --gitlab-host gitlab.example.com --gitlab-group mygroup
cd ~/vignoble-myproject

aoc add vigne my-api \
  --path ~/my-api \
  --repo mygroup/my-api

aoc daemon status
```

`aoc init` scaffolds the vignoble and starts its self-supervising daemon. Add
`--auto-merge` to a vigne only when you explicitly want Pinard to merge eligible
MRs automatically.

### 5. Run work

Spawn a worker directly:

```bash
aoc spawn --project my-api --prompt "Fix the authentication bug and open an MR"
```

Or launch the optional conductor control room:

```bash
pinard
```

The built-in SWE workflow can also start from an owner-authorized GitLab issue
assigned to the configured Pinard service account.

## The estate vocabulary

The wine terms describe real scopes and responsibilities rather than decorative
aliases:

| Term | Meaning |
|---|---|
| **Vignoble** | The workspace or estate containing repositories, workstreams, state, and configuration |
| **Vigne** | One registered repository |
| **Parcelle** | A persistent workstream spanning tasks, agents, and days |
| **Régisseur** | Optional top-level conductor for the vignoble |
| **Maître** | Optional conductor scoped to one parcelle |
| **Vendangeur** | A worker agent running one loop to completion |
| **Cuvée** | An intermediate branch that blends several agents' changes before `main` |

User-facing surfaces say **vendangeur**; code, CLI flags, and configuration continue
to use **worker** where that is the established interface.

## Components

| Component | Responsibility |
|---|---|
| `aoc` | Go CLI for setup, daemon operation, spawning, status, schedules, terminals, and administration |
| Daemon | Always-on, model-free engine for watchers, schedules, dispatch, recovery, and memory service ownership |
| `pinard` | Launcher for the régisseur, maîtres, and workers |
| Babysitter | Executes a semi-deterministic process step by step and owns its journal |
| NATS JetStream | The single communication bus between components and hosts |
| Engram + wiki | Local-first memory, curated knowledge, and best-effort cloud replication |

For topology, subjects, state files, and session layout, see
[Architecture](website/content/docs/architecture.md).

## Documentation

| Start here | Then go deeper |
|---|---|
| [Overview](website/content/docs/overview.md) | [Architecture](website/content/docs/architecture.md) |
| [Getting Started](website/content/docs/getting-started.md) | [Orchestration & Parcelles](website/content/docs/orchestration.md) |
| [The Semi-Deterministic Loop](website/content/docs/semi-deterministic-loop.md) | [Memory & Recall](website/content/docs/memory.md) |
| [Authoring Processes](website/content/docs/authoring-processes.md) | [Distributed & Remote Execution](website/content/docs/remote-workers.md) |
| [The SWE Process](website/content/docs/swe-process.md) | [Scheduling](website/content/docs/scheduling.md) |
| [CLI Reference](website/content/docs/cli-reference.md) | [Web Terminal](website/content/docs/web-terminal.md) |
| [Configuration](website/content/docs/configuration.md) | [Capsule Protocol](website/content/docs/capsules.md) |

## Development

```bash
go build ./...
go test ./...

cd pi-extension
npm ci
npm test
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution workflow and DCO
requirements. Report vulnerabilities through the process in
[SECURITY.md](SECURITY.md).

## License

Pinard is released under the [MIT License](LICENSE).

<p align="center">
  <img src="website/static/images/docs/docs-footer-tools.png" alt="" width="400">
</p>
