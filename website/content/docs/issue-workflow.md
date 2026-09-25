---
title: Issue Workflow
weight: 52
group: Operations
---

This page covers the operator-facing mechanics of driving [the SWE
process](/docs/swe-process/) from a GitLab (or GitHub) issue: how a vendangeur
gets spawned in the first place, and — the part that trips people up — how to
make one **spawn again** on an issue that's already been worked.

## The normal flow

1. An issue is assigned to the pinard service account (by a human, or by the
   conductor's `spawn_agent` tool).
2. The issue-watcher (part of `aoc daemon`) picks it up on its next poll,
   checks the [owner gate](/docs/swe-process/#owner-gate-security)
   and any [spawn-gating labels](/docs/swe-process/#labels-that-gate-spawning),
   and — if clear — runs `aoc spawn` for it.
3. The issue-watcher records the issue as `status: spawned` in
   `.state/issue-watcher.yaml` so it is never spawned a second time by the
   normal poll loop.

That last point is the crux of the next section: once an issue is `spawned`,
the watcher will not touch it again on its own — even if the vendangeur died,
its MR was closed, or you just want it redone.

## Respawning an already-spawned issue

Use `aoc respawn <vigne> <issue>` to force an immediate respawn, one command,
no waiting on the poll cycle:

```bash
aoc respawn my-vigne 336
```

This is atomic — it does not touch the git host or the issue-watcher's poll
loop, it just runs once, synchronously:

1. **Clears stale labels** — `pinard:discarded` and `in-progress`, if present.
2. **Reaps any leftover worker** — kills the tmux session and git worktree
   from the previous spawn (if it's still around) and removes its KV entry, so
   the new run doesn't collide with it.
3. **Resets the watcher state** to `seen`.
4. **Spawns immediately**, subject to the same [owner
   gate](/docs/swe-process/#owner-gate-security) as a normal spawn — an
   unapproved issue is held (with an `@owner approve` prompt), not
   force-spawned.

It prints a short summary of what it did (labels cleared, worker reaped,
spawn result) and exits non-zero with an explanatory reason if the spawn
didn't happen (issue is `blocked`, or still awaiting owner approval).

### Open-MR guard

If the previous worker still has an open, unmerged MR, `aoc respawn` refuses
by default — mirroring [`aoc gc`](/docs/cli-reference/)'s open-MR guard.
Reaping that worker unconditionally would only delete its *local* branch: the
remote branch and the MR would survive with no worker attached to answer
review feedback, and the fresh worker would then open a **second** MR for the
same issue.

Pass `--force` to explicitly abandon the open MR and respawn anyway. This also
drops the stale MR-watcher entry for the reaped worker, so the watcher doesn't
keep polling a session that no longer exists — but it does not close the
orphaned MR itself; do that by hand (or merge it) if you don't want it lingering.

```bash
aoc respawn my-vigne 336 --force
```

### From the conductor

The conductor has a `respawn_issue` tool (with the same `force` option) that
wraps the same command, and `spawn_agent` calls it for you automatically: if
you ask the conductor to spawn a vendangeur on an issue that's already tracked
as `spawned`, it detects that and respawns instead of silently re-assigning
and doing nothing. `spawn_agent` never passes `force` on your behalf — an
open-MR refusal is surfaced back to you (with a pointer to `respawn_issue
force:true`) rather than silently bypassed. If the issue is `blocked` or
awaiting owner approval, `spawn_agent` says so explicitly instead of reporting
a false success.

## Manual fallback: the `pinard:discarded` label

If you don't have shell/`aoc` access to the vignoble host, or aren't talking
to the conductor, you can still trigger a respawn purely through git-host
labels — the issue-watcher polls for this on every cycle:

1. Add the `pinard:discarded` label to the issue. On its next poll, the
   watcher resets the issue from `spawned` back to `seen` and posts a comment
   confirming the reset.
2. Remove the `pinard:discarded` label (and re-assign the issue to the pinard
   user if it was unassigned). On the *next* poll after that, the watcher
   spawns a fresh vendangeur.

This works, but it is slower and non-atomic than `aoc respawn` — two poll
cycles (up to ~2 minutes total, depending on your daemon's poll interval)
instead of one synchronous command, with a window in between where the issue
visibly carries a `pinard:discarded` label. Prefer `aoc respawn` /
`respawn_issue` whenever you have access to either.

## Labels at a glance

| Label | Effect |
|-------|--------|
| `blocked` | Skipped — no vendangeur spawned, and `aoc respawn` refuses too |
| `pinard:discarded` | Manual reset trigger (see above); also auto-cleared by `aoc respawn` |
| `in-progress` | Set once a vendangeur is spawned; auto-cleared by `aoc respawn` |
| `pinard:awaiting-approval` | Held pending owner approval; removed automatically once approved |
| `capsule:awaiting-funding` | Capsule-gated — see [Buddy Capsules](/docs/capsules/) |

See [The SWE Process](/docs/swe-process/) for the full lifecycle (issue →
change → PR → review → merge → reap) and the owner-approval / capsule funding
gates that both the normal flow and `aoc respawn` go through.
