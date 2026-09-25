---
title: The SWE Process
weight: 40
group: Applications
---

The SWE process is Pinard's **reference loop** — a
[semi-deterministic loop](/docs/semi-deterministic-loop/) that turns an issue
into a merged pull request: *issue → change → PR → review → merge → reap*. It is a
complete, batteries-included application built on the engine — and a template for
loops of your own.

> This is **one process**, not the whole of Pinard. If your fleet does something
> other than code review, you write a different [babysitter
> process](/docs/authoring-processes/); everything below is how this particular loop
> is wired.
>
> **GitHub and GitLab are both supported.** The loop runs identically on either git
> host — Pinard's [pressoir abstraction](/docs/configuration/#pressoir--git-host)
> handles the provider differences automatically. Set `pressoir.provider: github` in
> `vignes.yaml` to use a GitHub-backed vigne; see the [GitHub Setup
> guide](/docs/github-setup/) for PAT permissions and auto-merge configuration.

## Setup — the Pinard service account

The SWE loop acts through a dedicated service account on your git host.

### GitLab setup

| Action | API | Role |
|--------|-----|------|
| Push branches | SSH | Developer |
| Open / merge / comment on MRs | `/merge_requests`, `/notes` | Developer |
| Create issues, read pipelines | `/issues`, `/pipelines` | Developer |

- **Personal access token** — scopes `api`, `read_repository`, `write_repository`.
- **SSH key** — `ssh-keygen -t ed25519 -C "pinard" -f ~/.ssh/pinard_id_ed25519 -N ""`,
  then add the public key to the account.
- **Branch protection** — the default branch should allow "Developers + Maintainers"
  to merge, otherwise Pinard can't [auto-merge](#auto-merge-optional).

### GitHub setup

See the **[GitHub Setup guide](/docs/github-setup/)** for full step-by-step
instructions. Short summary:

- Create a **fine-grained PAT** with scopes: Metadata (read), Contents (write), Pull
  requests (write), Issues (write), Commit statuses (read), Actions (write), Workflows
  (write).
- No SSH key needed — Pinard authenticates `git push` over HTTPS using the PAT.
- Enable **"Allow auto-merge"** in repo Settings → General → Pull Requests.
- Gate auto-merge on **status checks** (not required approvals) — GitHub prevents a
  bot from approving its own pull request.

## Trigger — assign an issue

The daemon's **issue watcher** scans every vigne for open issues **assigned to the
Pinard user**. Assignment *is* the request — there is no opt-in label and no
confirmation step. When it finds one, it spawns a vendangeur with the issue as
context and labels the issue `in-progress`.

### Owner gate (security)

Pinard only spawns vendangeurs for work the **vignoble owner** has authorized. This
prevents any user with access to your git host from spending your LLM quota by
assigning issues to the Pinard bot.

An issue passes the gate when **either** is true:

- The issue **author is the owner** (you created it yourself).
- The **owner explicitly approves**: leave a comment on the issue @-mentioning the
  Pinard user with an approval keyword (`approve`, `approved`, or `go`), **or** assign
  the Pinard user to the issue yourself (a system assignment note authored by the owner
  counts as approval).

When an issue is assigned to Pinard but does not yet pass the gate, the watcher:

1. Labels the issue `pinard:awaiting-approval`.
2. Posts a comment on the issue explaining what is needed.
3. Polls each cycle — as soon as the owner approves, the vendangeur spawns.

The gate is **fail-closed**: if `owner` is not configured in `credentials.yaml`, no
auto-spawning happens at all.

#### Configuring the gate

Set `owner` in `credentials.yaml` to your username on the git host (GitLab or GitHub):

```yaml
nats:
  user: lelongs   # this becomes the owner automatically
```

The `nats.user` field is used as the owner by default. If you need to specify a
different owner (e.g. when the NATS user and GitLab owner differ), override it with
`owner:` in the credentials file.

To allow the conductor's `spawn_agent` tool to assign issues *as the owner* (so that
assignment itself counts as approval), configure `owner_token_env` on GitLab:

```yaml
gitlab:
  owner_token_env: PINARD_OWNER_GITLAB_TOKEN   # human operator's GitLab PAT
```

When `PINARD_OWNER_GITLAB_TOKEN` is set, the conductor uses it for issue assignment
(the system note is then authored by *you*, not the bot), and Pinard can auto-approve
its own spawns without requiring a separate approval comment.

> **Security note.** The owner token is **only** emitted for the conductor role
> (`aoc env-exports --role conductor`). Workers receive the bot token only and can
> never hold the operator's PAT — even if it is in the daemon's environment,
> `aoc spawn` passes workers an explicit, allowlisted environment.

> **GitHub.** There is no separate `owner_token` for GitHub — the owner gate still
> applies (via `nats.user`), but assignment-as-approval works only when the vignoble
> owner assigns the issue themselves on GitHub.

### Labels that gate spawning

| Label | Effect |
|-------|--------|
| `blocked` | Skipped — no vendangeur spawned |
| `pinard:discarded` | Skipped; if already spawned, its state resets so it can retry |
| `pinard:awaiting-approval` | Held — owner hasn't approved yet; watcher re-checks each cycle. **Removed automatically** when the owner approves. |
| `capsule:awaiting-funding` | Capsule-gated — contract detected but not yet funded; see [Buddy Capsules](/docs/capsules/) |

**Retry:** an already-`spawned` issue is never re-spawned by the normal poll
loop on its own. See [Issue Workflow](/docs/issue-workflow/) for `aoc respawn`
(one atomic command, no waiting) and the `pinard:discarded` manual label
fallback.

### Labels that route the work

| Label | Effect |
|-------|--------|
| `parcelle:<name>` | Route into a [parcelle](/docs/orchestration/) (else the vigne's own bucket) |
| `target:<branch>` | Target a branch, e.g. `target:cuvee/data-service` (may contain `/`) |

A parcelle can also claim an issue via its `parcelle.yaml`, which may set a
`target_branch:` (the [cuvée](/docs/orchestration/) strategy) without any label.

## The loop — issue to MR

<figure class="doc-figure">
  <div class="doc-figure-visual">
    <img src="/images/docs/swe-process-lifecycle.jpg" alt="A sketched software-work lifecycle from an approved request through an isolated worker and merge-request watcher, with review and failure loops, to merge and worker cleanup.">
    <span class="doc-figure-label mustard" style="--x: 13%; --y: 38%;">Owner gate</span>
    <span class="doc-figure-label charcoal" style="--x: 16%; --y: 57%;">Vendangeur</span>
    <span class="doc-figure-label mustard" style="--x: 38%; --y: 41%;">MR handoff</span>
    <span class="doc-figure-label charcoal" style="--x: 58%; --y: 65%;">MR watcher</span>
    <span class="doc-figure-label terracotta" style="--x: 84%; --y: 30%;">Review feedback</span>
    <span class="doc-figure-label terracotta" style="--x: 81%; --y: 51%;">Pipeline retry</span>
    <span class="doc-figure-label terracotta" style="--x: 49%; --y: 90%;">Circuit breaker</span>
    <span class="doc-figure-label mustard" style="--x: 78%; --y: 91%;">Merge · post-merge · reap</span>
  </div>
  <figcaption><strong>Issue to merge, with one deterministic caretaker.</strong> The watcher routes review and pipeline failures back to the vendangeur, and owns the successful path through merge and cleanup.</figcaption>
  <ul class="doc-figure-legend" aria-label="SWE process legend">
    <li><span><strong>Owner gate</strong> — assignment becomes work only after owner authorization.</span></li>
    <li><span><strong>Vendangeur → MR handoff</strong> — the isolated worker implements and validates, then <code>track_mr</code> transfers care.</span></li>
    <li><span><strong>MR watcher</strong> — reviews and failed pipelines return to the worker; repeated failure reaches the circuit breaker.</span></li>
    <li><span><strong>Merge · post-merge · reap</strong> — approval and green pipelines lead to merge, final monitoring, and deterministic cleanup.</span></li>
  </ul>
</figure>

| Stage | What happens |
|-------|-------------|
| Detected | Watcher publishes `issues_new` |
| Spawned | Vendangeur created in its own git worktree, issue as context |
| In progress | Issue labelled `in-progress` |
| Working | The loop makes the change, validates it, and opens an MR |
| Tracked | The worker calls `track_mr(mr: N)` so the MR watcher takes over |

Comments on a tracked issue are forwarded to the conductor as `issues_comment`
events (Pinard's own comments are filtered, so there's no feedback loop).

## The MR watcher

Once an MR is tracked (written to `.state/mr-watcher.yaml`, polled every ~30s), the
daemon tends it through its whole lifecycle:

```
Worker opens MR → track_mr → MR watcher
    ├── Review comments → forwarded to the worker (threaded via discussion_id)
    ├── Pipeline fails   → dispatched to the worker (attempt X/5)
    ├── Pipeline passes  → informational event to the conductor
    ├── Approved + green  → auto-merge (only if enabled — off by default)
    ├── Merged (human or auto) → post-merge pipeline + tag monitoring
    ├── Circuit breaker (5 failures) → worker killed
    └── Terminal condition → reapWorker (the single teardown point)
```

### Review forwarding

New reviewer notes are published as `review_comment` events carrying
`discussion_id` (for threaded replies) and `file`/`line` (for inline comments), and
dispatched straight to the worker's inbox — including the exact
`glab api …/discussions/<id>/notes` command to reply in-thread.

### CI pipeline failures

When CI fails, the daemon dispatches a `pipeline_failed` event directly to the
vendangeur's inbox (attempt X/5) so it can investigate and fix the issue without
conductor involvement. After 5 consecutive failures on the same MR, a circuit
breaker trips and the vendangeur is killed rather than looping forever.

### Automated maître review (`auto_review`)

When CI goes green on a non-draft MR, the daemon notifies the **owning maître** (the
parcelle conductor) to review the diff. This is on by default — `auto_review: true`
— and is independent of auto-merge. It gives the operator confidence that a Pinard
maître looked at every change, whether or not the vigne is set to auto-merge.

The maître uses `aoc pressoir get-pr-changes` to enumerate changed files and
`aoc pressoir list-pr-notes` to read existing review comments, then chooses one of two
outcomes — never a raw `aoc pressoir comment-pr`/`approve-pr` call:

- **Actionable feedback → `comment_mr`.** Posts a comment signed `🍇 Reviewed by the
  <parcelle> maître`, wrapped by `aoc comment-mr` so it carries the conductor marker.
  The conductor and vendangeur share a git-host identity, so an unmarked note would be
  silently dropped by the mr-watcher; the marker is what makes it forward to the
  vendangeur as review feedback.
- **Nothing to say (ALL-CLEAR) → `mark_mr_reviewed`.** Applies the `pinard:reviewed`
  label (via `aoc mark-mr-reviewed`) and stops — no vendangeur turn, no MR note. This
  keeps a genuine LGTM from waking the vendangeur for nothing.

**Pinard does not approve MRs automatically, on either path.** `mark_mr_reviewed`
only applies the label; approval remains the human owner's or forge's responsibility.
`aoc pressoir approve-pr` is available as a manual CLI tool for operators acting on
explicit instruction only — never invoked from this automatic path.

Reviews are **idempotent**: the watcher tracks the `pinard:reviewed` label against the
current MR HEAD SHA, so a new review only fires after a new push (a stale label from an
older SHA is cleared before a fresh dispatch). A **noise filter** skips trivial and
mechanical MRs automatically — docs sync (`sync Ledger`), image/chart bumps
(`bump-image`, `bump-chart`), and reverts never trigger a full maître review turn.

Opt out per-vigne or vignoble-wide with `auto_review: false` in `vignes.yaml`:

```yaml
# Vignoble-level opt-out (all vignes)
auto_review: false

vignes:
  my-project:
    auto_review: false  # Per-vigne opt-out
```

### Auto-merge (optional)

**Off by default — a human merges.** When enabled via `auto_merge: true` in
[`vignes.yaml`](/docs/configuration/) (per-vigne or global), the watcher merges once
**all** hold: pipeline **success**, at least one **approval**, **no unresolved
threads**, and **not a Draft**. If unapproved, a `needs_approval` event goes to the
conductor. With auto-merge off, none of this runs — the human merges after seeing the
maître's review (comment or silent label).

### Post-merge & reap

When `monitor_post_merge: true` (the default), the watcher monitors the main and tag
pipelines triggered by the merge commit, reporting to the conductor. On success (or
when post-merge monitoring is disabled), it finally calls **`reapWorker`** — the
single, deterministic teardown point that kills the tmux session and cleans up the
worktree. On a post-merge main-pipeline failure, a `main_pipeline_failed` event is
dispatched to the vendangeur's inbox instead, and the reap is held until that's
resolved.

## See also

- **[Issue Workflow](/docs/issue-workflow/)** — respawning an already-spawned issue, and the label-driven manual fallback.
- **[Authoring Processes](/docs/authoring-processes/)** — write your own loop.
- **[Orchestration & Parcelles](/docs/orchestration/)** — group SWE work into workstreams.
- **[Configuration](/docs/configuration/)** — `auto_merge`, `auto_review`, and other toggles.
- **[Buddy Capsules](/docs/capsules/)** — let a colleague fund a vendangeur's quota.
