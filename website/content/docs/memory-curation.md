---
title: Teaching & Curation
weight: 33
group: Memory
---

> **Status:** The wiki curator, inbound sync, ontology gardener, confidence-gated
> wiki serving, `/lesson`, `/teaching`, curate-on-promote, and MR knowledge ingestion
> are all ✅ **shipped**. The shipped passive-capture write path is covered in
> [Memory & Recall](/docs/memory/).

Capturing knowledge, turning it into something readable, and deciding what deserves
to spread — that is the human-facing end of Pinard's memory.

<figure class="doc-figure">
  <div class="doc-figure-visual">
    <img src="/images/docs/memory-curation-loop.jpg" alt="Three explicit knowledge inputs—pinned lesson, teaching session, and passive observation—flow through typed entities, wiki curation, human review, and recall, with approved wiki knowledge promoted to broader scopes.">
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 11%; --y: 34%;"><code>/lesson</code></span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 12%; --y: 65%;"><code>/teaching</code></span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 13%; --y: 90%;">Passive observation</span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 30%; --y: 28%;">Typed entities & relations</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 43%; --y: 70%;">WikiCurator</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 58%; --y: 43%;">Wiki page</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 68%; --y: 67%;">Human review</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 84%; --y: 68%;">Recall to agent</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 70%; --y: 30%;">Promote approved wiki</span>
    <span class="doc-figure-label doc-figure-label--desktop charcoal" style="--x: 66%; --y: 8%;">Scopes · vigne → vignoble → global</span>
  </div>
  <figcaption><strong>Teach, curate, review, recall.</strong> The three capture sources merge into typed entities and relations. The curator turns those records into a wiki page, a human reviews it, and approved knowledge can then be recalled or promoted.</figcaption>
  <ul class="doc-figure-legend" aria-label="Teaching and curation legend">
    <li><span><strong>Three capture paths</strong> — <code>/lesson</code> pins a fact, <code>/teaching</code> records an explicit teaching session, and passive capture records a session observation.</span></li>
    <li><span><strong>Typed entities & relations</strong> — selected facts become named records connected by explicit relationships.</span></li>
    <li><span><strong>Curate → wiki → human review</strong> — evidence becomes readable, git-tracked pages in a reviewable MR.</span></li>
    <li><span><strong>Recall</strong> — approved, confidence-gated knowledge returns to working agents.</span></li>
    <li><span><strong>Promotion</strong> — synthesized pages rise from vigne to vignoble to global; raw entities do not.</span></li>
  </ul>
</figure>

## Teaching a fleet

Two explicit inputs let you *teach*, plus a passive safety net:

### `/lesson` ✅

Pin, edit, or replace a high-value rule or fact in shared memory:

```
/lesson <text>                          # pin a new lesson
/lesson --edit                          # interactive picker — choose a lesson to edit
/lesson --edit --entity=<id>            # edit a specific lesson by entity id
/lesson --replace --entity=<id> <text>  # replace the text of a specific lesson
```

A lesson is stored with `type: rule` and `confidence: 0.95`. It is published to the
**NATS memory pipeline** (`pinard.<vignoble>.memory.rules`), where the memory
ingester upserts it directly into SurrealDB — no Engram HTTP call, no LLM extraction.
This makes lessons reliable and side-effect-free.

The lesson is also **immediately injected into the active agent session** as a new
turn, so the agent acknowledges and applies the rule right away — no restart needed.

Lessons in SurrealDB auto-qualify for `auto_serve` in recall and are candidates for
promotion to a broader scope (below). Entity ids appear in `/recall` hit labels
(e.g. `ref: entity:entity:abc123`) and can be passed to `--edit --entity=` or
`--replace --entity=` directly.

Available in: conductor (`/lesson`) and vendangeur (`/lesson`).

### `/forget --entity=<id>` ✅

Delete a pinned lesson by its SurrealDB entity id:

```
/forget --entity=<id>        # prompts for confirmation, then deletes
```

Only `lesson`-provenance entities can be deleted via `/forget` — the ingester
refuses to delete LLM-extracted entities, preventing accidental data loss.
Entity ids appear in `/recall` hit labels; copy the id from there.

Available in: conductor (`/forget`) and vendangeur (`/forget`).

### `/teaching` mode ✅
A **mode**, not a one-shot. While active, every exchange is captured as a
`teaching-episode` from which a runbook, procedure, or ontology edges are extracted.
Its lifecycle is explicit and safe:

- a **visible banner** (status-line indicator `📚 Teaching`) while it is on,
- turned off explicitly (`/teaching off` or re-toggle) **and**
- **auto-off at session end** — it never persists silently.

**Retroactive capture modes** let you include exchanges that happened *before* you
thought to enable teaching:

| Variant | What it captures |
|---------|------------------|
| `/teaching` | Activate from now; does not go back |
| `/teaching --all` | Publish the *entire* session history as a teaching episode, then activate |
| `/teaching --from <duration>` | Publish the last N minutes (e.g. `30m`, `1h`), then activate |
| `/teaching off` or re-toggle | Deactivate; session transcript flushed at session end |

At session end, if teaching is still active, the full transcript is published as a
single episode for better extraction context.

Available in: conductor (`/teaching`) only — vendangeurs are task-scoped and do not
have a multi-turn session to teach from.

### Passive capture ✅
Engram session summaries and curated `mem_save` observations are captured
automatically, so lessons are never purely manual. This is the part that runs today.

### MR knowledge ingestion ✅ {#mr-knowledge-ingestion}

When a merge request is **merged**, the daemon automatically assembles a memory event
and publishes it to the memory service. The service runs two extraction passes and
stores durable entities in SurrealDB.

#### Pass 1 — description + issues (provenance `mr`)

The primary pass reads:
- MR title and description (the *what was realized*)
- Closing issues title and description (the *intent context*)
- File paths changed (stored in entity data, not embedded)

Extracted entity roles:
- `decision` — a choice made and *why* ("chose X over Y because Z")
- `artifact` — a durable structural fact the change established
- `diagnosis` — a root cause paired with its fix (issue: cause → MR: fix)
- Re-scope deltas — when the MR diverged from the issue's stated intent

When there is no durable knowledge (trivial mechanical change, uncertain extraction),
**zero entities are written** — that is a success, not a failure.

#### Pass 2 — review delta (provenance `mr-review`)

After Pass 1, the ingester scans the pre-filtered review discussion for **net-new**
durable knowledge not already captured in Pass 1 entities. The LLM is given the
Pass 1 results as context; it only emits an entity when it can justify why that
entity is not covered and not already expressed. The default is zero entities — noise
or chatter yields nothing.

Entities extracted here get `provenance="mr-review"` and a slightly lower confidence
than Pass 1 entities. They appear in recall with the label `[artifact:mr-review ·
scope]` (or `decision:mr-review` / `diagnosis:mr-review`).

Pass 2 does not re-extract what Pass 1 already found. Its primary value is capturing
**forward-looking constraints** flagged by reviewers as "worth noting for the future"
or "not blocking this MR but must hold in a later change" — knowledge that would
otherwise live only in the review thread.

#### `@memory:` review marker

Instead of waiting for LLM classification, a reviewer can pin a fact directly by
prefixing a review note with `@memory:` (case-insensitive):

```
@memory: always split the CA bundle into a per-component secret; sharing one secret across pods causes Vault rate-limit spikes
```

The daemon intercepts this marker immediately during review forwarding and routes
the note body straight into the `/lesson` pipeline — no Pass 2 LLM call, no
ambiguity. The same extraction runs in `aoc mr-memory` replay (see [CLI
Reference](/docs/cli-reference/#aoc-mr-memory)).

#### Noise filter

**Noise filter:** The daemon skips MRs that are unlikely to contain decisions:
- `sync Ledger`, `bump-image`, `bump-chart` title prefixes
- `^Revert ` prefixes
- Cuvée accumulation merges (source branch starts with `cuvee/`)
- Empty or template descriptions

Override with labels on the MR:

| Label | Effect |
|-------|--------|
| `memory:skip` | Never ingest this MR, regardless of content |
| `memory:capture` | Always ingest, bypassing all noise filters |

In recall, Pass 1 entities carry a `[decision:mr · scope]` label (or `[artifact:mr
· scope]` / `[diagnosis:mr · scope]`). Pass 2 review entities carry `[decision:mr-review
· scope]`. Both expose `url` and `files_changed` when fetched in detail.

## The self-evolving wiki ✅

The wiki keeps two representations in sync:

- **OKF markdown** (`wiki/` in the vignoble repo) is the human-facing, git-tracked,
  durable artifact — still readable if everything else disappears. It follows the
  Open Knowledge Format (OKF) with YAML frontmatter (`type`, `title`, `relations`,
  etc.).
- **SurrealDB** is the index, curation workspace, and serving layer: pages as
  `wiki_doc` documents, page embeddings as vectors, backlinks and typed entity
  references as graph edges — so the wiki is searchable through the same recall path.

### OKF bundle scaffold

When the daemon starts for a vignoble, it calls **`EnsureBundle`**, which
idempotently seeds three reserved files under `<vignoble>/wiki/`:

| File | Purpose |
|------|---------|
| `index.md` | Root index listing the bundle |
| `INSTRUCTIONS.md` | Wiki constitution: type vocabulary, folder conventions, contribution rules |
| `log.md` | Change history |

The scaffold is committed to the vignoble repo automatically (best-effort). After
the initial seed, the daemon does not write to `wiki/` again — all further content
comes from the curator.

### WikiCurator (outbound — SurrealDB → wiki)

The **`WikiCurator`** reads the ontology-typed SurrealDB graph for a `group_id`,
clusters related entities and edges into concept candidates, and synthesizes OKF
markdown pages via the configured LLM. It runs **every 6 hours** inside the memory
service pod.

Key behaviors:

- **Incremental** — tracks a `wiki_curator_cursor`; only re-synthesizes concepts
  whose underlying graph entities changed since the last run. The cursor is **only
  advanced when synthesis succeeds** — if the chat-LLM is unavailable, the curator
  does nothing and leaves the cursor in place so those entities are retried on the
  next cycle.
- **Full-snapshot curator MRs** — each curator MR is a **complete snapshot** of all
  `auto_serve` wiki pages for the scope, not just the pages synthesized this cycle.
  This prevents a pod restart (which might find only one changed entity) from collapsing
  a large, previously-synthesized wiki into a single-page MR. Stale pages that are no
  longer backed by any live entity are pruned from the branch automatically.
- **No-fallback synthesis** — a wiki page is written only when the LLM returns a
  non-empty title and body. If the LLM is unreachable or returns nothing usable, the
  entity is retried next cycle. No degraded placeholder pages are ever written.
- **Scaffold entity guard** — entities whose names are bare mem_save structured-body
  markers (`What:`, `Why:`, `Where:`, `Learned:`, and similar) are only skipped when
  their description is also trivial. Real observations starting with `**What**:` but
  carrying actual content are still curated (the LLM cleans their titles).
- **Stable identity, not re-derived from the title** — a concept's page path is
  resolved from the `wiki_mentions` edges recorded between its source entities and the
  `wiki_doc` on a prior synthesis, so the LLM is free to reword a title on resynthesis
  (better phrasing, a cleaner summary) without minting a new page. Only a genuinely new
  concept (no entity yet linked to any doc) falls back to slugging the synthesized
  title.
- **Deduplication (fallback)** — for the slug-fallback path, a cosine similarity scan
  using the **synthesized title's embedding** prevents near-duplicate pages when the
  edge-based identity above didn't resolve a path; close matches are updated in place
  rather than creating a new page. Thresholds are role-aware: prose-heavy types
  (`decision`, `diagnosis`) use 0.88; all others use 0.92.
- **Human-authored pages are protected** — any page with `source: human` in its
  frontmatter is never overwritten or deleted by the curator.
- **Reserved files** (`index.md`, `log.md`) are always skipped.
- **Per-vigne namespacing** — each vigne writes to `wiki/<vigne-name>/` so vignes
  are isolated; a human edit to one vigne's page only affects that vigne's scope.
- **LLM-synthesized titles and summaries** — the curator requests structured JSON
  `{title, summary, body}` from the LLM; the `summary` field is stored in SurrealDB
  and used in boot manifests (see [Boot injection](/docs/memory/#boot-injection-at-spawn)).

The curator writes pages as a branch + MR in the wiki git repo so all changes are
human-reviewable before landing.

### WikiSyncer (inbound — wiki → SurrealDB)

Editing a wiki page by hand and letting the curator refine it are the same loop. The
**`WikiSyncer`** pulls the wiki repo, diffs changed OKF files, and upserts them into
SurrealDB with embeddings and typed link edges.

- **Idempotent** via `content_hash` — unchanged files are skipped.
- **Loop-safe** — never writes back to git (inbound only).
- Runs every 6 hours, after each curator run.

### Multi-vignoble curator

The memory service can curate multiple vignobles in one pod. Set
`VIGNOBLES_BASE_DIR` to the parent directory of cloned vignoble repos (each
subdir named `vignoble-<name>/` containing a `vignes.yaml`). The curator and
rollup engine iterate all discovered vignobles automatically — no per-vignoble
configuration is needed.

### Confidence gating ✅

Wiki pages are **confidence-gated before being served** in agent recall. Each page
carries a confidence score derived from how many ontology-typed entities back it and
how well-typed those entities are:

- **≥ 0.7** → `auto_serve` — included in agent recall automatically.
- **< 0.7** → `needs_review` — held back from recall until a human reviews and
  promotes the page.

The score is computed from a base of 0.60, a cluster-size bonus (more corroborating
entities → higher confidence), and a role bonus for high-signal types (`decision`,
`diagnosis`, `verdict`, `log_pattern` each add +0.12). A single well-typed decision
or diagnosis entity therefore reaches `auto_serve` on its own; artifact-typed pages
need more corroboration.

In practice, when an agent issues a recall query, the memory service blends wiki
hits (vector + keyword) with entity-graph hits, deduplicates them by session, and
summarizes all `auto_serve` wiki pages alongside entity results in the `[memory]`
context block.

## Ontology gardener ✅

The **`OntologyGardener`** mines the `entity_staging` and `edge_staging` tables for
a `group_id`, clusters proposals that didn't match any existing ontology type, and
applies LLM-driven **Map / Extend / Hold** decisions per cluster:

| Decision | Meaning |
|----------|---------|
| **Map** | The proposal maps to an existing type — no ontology change needed |
| **Extend** | A genuinely new type is warranted — emit a human-gated MR |
| **Hold** | Not enough signal yet — wait for more recurrences |

For **Extend** decisions, the gardener writes an `ontology-proposals/<id>.yaml`
file into the wiki repo and opens an MR there. The MR includes a copy-paste Python
snippet for `register_domain(...)` — a human approves the proposal and manually
applies it to the pinard source repo. **The ontology is never mutated automatically.**

The gardener runs inside the same memory-service pod as the curator, on a periodic
schedule driven by the ingester.

## Scope & promotion ✅ {#scope--promotion}

Knowledge is stored at the **finest grain** (a vigne) and **rolled up** by the
memory service, which knows vignoble membership from `vignes.yaml`:

```
 vigne  ──▶  vignoble  ──▶  cross-vignoble (global)
```

### Curate-on-promote (invariant)

> **Only synthesized wiki_doc entries rise. Raw entities (`artifact`) and
> typed entities never copy upward verbatim.**

When the rollup engine detects cross-vigne overlap — recurring wiki pages or typed
entities that appear in multiple vignes — it synthesizes a **new consolidated
`wiki_doc`** at the vignoble or global scope via the configured LLM, rather than
copying raw content. The promoted page gets an LLM-synthesized title, one-line
summary, and body. Raw `artifact` entities are excluded entirely and never rise.

Curate-on-promote results (vignoble-scoped `wiki_doc` rows) are also synced to
`wiki/_shared/` in the wiki git repo (branch `wiki-curator/_shared`, MR opened for
review), so operators can see the cross-vigne synthesized knowledge in one place.

> **Note:** Directly-ingested Engram observations — captured by the conductor,
> régisseur, or maître under a vignoble-scoped project — are **preserved across
> rollup cycles**. Only entities introduced by the old promotion path
> (`provenance = 'promotion'`) are cleaned up during rollup. Prior to v0.32.2,
> the rollup engine incorrectly deleted *all* entity rows at the vignoble scope on
> each cycle, silently erasing these memories; that bug is now fixed.

### Per-vigne wiki namespacing

Each vigne's curator writes to its own subdirectory: `wiki/<vigne-name>/`. A human
edit to one vigne's page is only ingested into that vigne's SurrealDB scope, not
others.

### A fact that **recurs across many scopes**

A fact that recurs — e.g. the same commit-prefix rule seen in several vignobles —
becomes a **promotion candidate** toward a broader scope, up to and including the
base prompt every agent inherits.

The unifying insight: **scope roll-up, cross-scope rule promotion, and ontology
promotion (domain → core) are the same pattern** — recurrence detection plus review.
Engram's relation-judgment tools (`related` / `scoped` / `supersedes` /
`conflicts_with`) are the candidate engine for detecting recurrence and
contradiction, rather than a custom build.

### Human-gated by design

Anything that mutates **global agent behavior** — most of all the base prompt — is
**human-gated**: a wrong global rule would misbehave every agent at once. Promotion
reviews surface where humans already work: as **checkboxes or forms in the Obsidian
wiki** that translate an approval into a git PR — low-friction review, full audit
trail.

## Admin flags

The memory ingester (`cmd/memory-ingester`) exposes admin flags for recovery and
maintenance. Each flag causes the ingester to perform the operation and exit
immediately, without running the normal ingestion loop:

| Flag | Purpose |
|------|---------|
| `--reingest` | Reset all ingest cursors to seq=0 so the next startup re-reads every Engram observation and re-derives entity roles from scratch. Use after a `type_map` change to re-type an existing corpus. |
| `--recurate` | Delete all non-human `wiki_doc` rows and drop `wiki_curator_cursor` tables across **every scope** (vignes, vignoble-\*, `__global__`, and the current parcelle if set). The next startup regenerates all wiki pages cleanly. Use after a curator or rollup change that alters wiki output and would otherwise be blocked by stale rows. |
| `--rechunk` | Backfill `wiki_chunk` rows for all existing `wiki_doc` pages across every scope, then exit. Idempotent — safe to re-run. Use after a chunking strategy or embedding-model change to rebuild the per-heading chunk index used by semantic recall. |

```bash
# In the memory-ingester pod (exec in or via kubectl run):
memory-ingester --recurate    # wipe stale wiki pages; curator regenerates cleanly
memory-ingester --reingest    # re-ingest all observations and re-derive entity types
memory-ingester --rechunk     # rebuild per-heading wiki chunk embeddings
```

> **Note:** `--recurate` deletes **all auto-generated wiki_doc rows** across all
> scopes. Human-authored pages (`source: human` in frontmatter) are not affected.
> The operation is immediate — run it only when you want the curator to rebuild the
> entire wiki from the current entity graph on its next cycle.

## Next

- **[The Ontology & Portable Memory](/docs/memory-ontology/)** — how promoted types and rules are typed and shipped.
- **[The Layered Memory Architecture](/docs/memory-architecture/)** — the full stack.
