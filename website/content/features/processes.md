---
title: "Semi-Deterministic Processes"
icon: "⚗️"
tag: "process.js"
summary: "Turn a free-form agent into a resumable, checkpointed runbook-as-code — for data pipelines, DevOps, and automation that must be reproducible."
hero_image: "/images/photos/feature-processes.jpg"
hero_position: "center"
weight: 3
---

## Beyond free-form agents

A free-form agent is powerful but unpredictable: given the same task twice, it may take two different paths. That's fine for exploratory coding — but wrong for a nightly data pipeline, a release, or an HPC job that must run the **same way every time**, checkpoint safely, and resume after a crash.

Pinard's **babysitter processes** close that gap. A process is a **runbook written as code**: an ordered set of steps, each a scoped agent task with explicit success criteria. The agent still reasons and adapts *within* a step — but the sequence, the gates, and the definition of "done" are deterministic.

## A runbook, as code

A process lives in the project repo at `pinard/<name>/process.js`. Each step is a `defineTask` with a role, a task, the exact checks to verify before proceeding, and a structured verdict:

```js
import { defineTask } from '@a5c-ai/babysitter-sdk';

export default [
  defineTask('align', () => ({
    kind: 'agent',
    title: 'Align reads → BAM',
    agent: {
      prompt: {
        role: 'You are driving one step of a genomics build on an HPC.',
        task: 'source env.sh, run scripts/align.sh (submits a SLURM array, blocks on spoll).',
        instructions: [
          'Success criteria: every array task exits 0 and the provenance DB shows all BAMs written.',
          'Return ok=true only if the criteria are met; put counts in metrics.',
        ],
        outputFormat: 'A verdict: { ok, summary, metrics, needsAttention }',
      },
    },
  })),
  // …variant-calling, annotation, publish…
];
```

## Checkpoints, idempotency, resumability

- **Human gates.** Wrap irreversible or expensive transitions in `ctx.breakpoint()` — the process pauses for explicit approval before it proceeds.
- **Idempotent.** Steps re-check a provenance DB (or their own outputs) and skip completed work, so a restart resumes rather than redoes.
- **Resumable run journals.** Every run is recorded at `<runs-dir>/<RUN_ID>/run.json`. If a worker crashes — OOM, node failure, network drop — the run resumes from the exact step it left off.

```bash
aoc spawn --project genomics --process genomics-build   # start a run
aoc spawn --run-id genomics-build-42                  # resume where it stopped
```

## What it unlocks

The same orchestration that ships code now drives **automation**:

- **Data pipelines** — multi-step HPC/SLURM builds with per-step verification (a genomics genome build is a live example)
- **DevOps** — releases, migrations, environment provisioning with approval gates
- **Recurring operations** — anything that must be reproducible, auditable, and safe to re-run

Built-in starters ship in `processes/` (`swe`, `multi-step`, `review`, `hello`); real pipelines bake their own into the worker repo.

## The vintner's runbook

Every great cellar runs on a runbook — pick at this Brix, press at this pressure, rack on this day. The winemaker still tastes and judges, but the sequence is disciplined and repeatable. A Pinard process is that runbook: judgment where it matters, determinism where it counts.
