# Memory Layer — Integration Test Plan (`cuvee/memory` → `master`)

**Epic:** [&2 Memory layer (cuvee/memory)](https://gitlab.example.com/groups/your-group/pinard-agent/-/epics/2)
**Spec:** `openspec/changes/memory-layer/` (proposal, design, `amendment-01-surrealdb.md`, tasks)
**Status:** pre-merge validation of the accumulated `cuvee/memory` branch.

## Purpose & context

The memory layer was built as a stack of layers across 11 issues, all merged to
`cuvee/memory` under conductor (Option-B) review. **pinard's CI runs only
`ts-syntax-check` + `go-test` — there is no Python job**, so none of the Python
memory code was ever exercised by a pipeline. This plan is therefore the *real*
validation gate before `cuvee/memory → master`: a **bottom-up**, layer-by-layer
integration test in a **dedicated, isolated vignoble**.

Gate each stage before proceeding to the next. Capture results inline (check the
boxes) so the run is auditable under epic &2.

## Layer diagram (bottom-up)

```
  Rosetta (embeddings, shared, stateless)        Engram (curated write path, per-vignoble)
              │                                             │
              └───────────────┬─────────────────────────────┘
                              ▼
                     Ingester  (Engram → Rosetta → SurrealDB)
                              │
              ┌───────────────┼───────────────────────────┐
              ▼               ▼                           ▼
        Recall service   Roll-up / promotion       Portable subset (embedded)
        (memory.recall)  (vigne→vignoble→global)   (surrealkv:// + version stamp)
              │
              ▼
     Worker / conductor wiring (/teaching, /lesson, boot injection, per-turn recall)

        SurrealDB 3.2.0 server  = store of record (graph + vector + FTS)
        Graphiti + FalkorDB     = time-boxed KG eval sidecar (Phase-2, optional)
```

## A. Isolation checklist (do this first)

**Isolation = exclusive use, not dedicated instances.** Sharing SurrealDB / Engram /
NATS / Rosetta is fine **provided no other vignoble is using those backends during
the test window.** The goal is simply not to read/corrupt another vignoble's data or
have another vignoble's traffic perturb the test. Because the SurrealDB namespace is
hardcoded `pinard` (DB = `group_id`), use **test-only `group_id`s** so the test data is
partitioned within the shared instance and is trivial to drop afterwards.

| Env var | Requirement |
|---------|-------------|
| `SURREAL_URL` | Shared instance OK **if quiescent** (no other vignoble writing). Test data lives in DBs named by the test `group_id`s within the `pinard` namespace → easy cleanup. |
| `SURREAL_USER` / `SURREAL_PASS` | Valid credentials for the (shared) instance. |
| `NATS_URL` + `NATS_VIGNOBLE` | Shared NATS OK. `NATS_VIGNOBLE=<testname>` already scopes subjects to `pinard.<testname>.memory.>` and consumer `pinard-memory-ingester-<testname>`, so a unique test vignoble name isolates traffic. Verify the `pinard-memory` stream filter doesn't co-mingle with an active vignoble. |
| `ENGRAM_URL` | Shared Engram OK **if quiescent**; reads/writes are `group_id`/project-scoped. Set explicitly — default is inconsistent across modules (`7783` vs `7437`). |
| `ROSETTA_URL` | Shared external — always fine (stateless, read-only embedding). |
| `MEMORY_TOKEN_URL` / `ANTHROPIC_API_KEY` | Shared token OK (read-only LLM use); needed only for extraction/summarization stages. |
| `group_id`s | **Test-only ids**, e.g. `memtest-a`, `memtest-b` — the primary partition on shared backends. |
| `VIGNOBLE_LOGS` | Test vignoble logs dir. |

- [ ] Confirm **no other vignoble is actively using** the shared SurrealDB / Engram /
      NATS during the test window (no other memory ingester/recall running against them).
- [ ] Use test-only `group_id`s and a unique `NATS_VIGNOBLE`.
- [ ] **Exclusivity proof:** after Stage 4, confirm the only new/changed data on the
      shared backends belongs to the test `group_id`s (no other vignoble's data touched).
- [ ] **Cleanup:** drop the test `group_id` DBs (and test Engram obs) when done.

## B. Install

- [ ] Deploy the memory service from `cuvee/memory` into the test vignoble.
- [ ] `pip install pinard-core` from the internal PyPI (push-pypi) **or**
      `pip install -e packages/pinard-core` for in-repo dev.
- [ ] `pip install -r services/memory/requirements.txt`.
- [ ] Ensure SurrealDB 3.2.0 + NATS + Engram are reachable (shared instances are fine if quiescent — see §A); confirm the vignoble's Engram is serving.

## C. Bottom-up stages

Legend: **LLM?** = requires a live Anthropic token (`MEMORY_TOKEN_URL` logged in).
Stages 1–4, 6, 7 need **no** LLM and can run while logged out.

### Stage 1 — Engram read surface  (LLM: no)
- [ ] Seed a few curated observations in the test vignoble (`/lesson` or `mem_save`).
- [ ] `EngramReader(group_id=...).fetch()` returns them, project-filtered.
- **Pass:** observations returned via `GET /observations`; `/search?q=` returns ranked hits.

### Stage 2 — Rosetta embeddings  (LLM: no)
- [ ] `services.memory.embeddings.embed("memory layer test")`.
- **Pass:** returns a 1024-d float vector; latency sane (~120 ms single).

### Stage 3 — SurrealDB + schema  (LLM: no)
- [ ] Deploy the isolated SurrealDB 3.2.0; apply `services/memory/surrealdb/schema.surql`.
- [ ] Manual smoke: upsert an `entity` with an embedding; run KNN (`<|K, EF|>`),
      FTS (`@@` / `FULLTEXT ANALYZER`), and a `RELATE` traversal.
- **Pass:** schema DEFINEs succeed; all three query types return on the deployed instance.

### Stage 4 — Ingester (Engram → SurrealDB)  (LLM: no for curated path)
- [ ] Run `services/memory/ingester.py` against test Engram + SurrealDB + Rosetta.
- [ ] Verify curated observations become `entity` records + vectors + `RELATE` edges.
- [ ] `memory-ingester-status.json` healthy; errors (if any) logged loudly (ERROR).
- [ ] **Exclusivity proof** (see §A): only test `group_id` data changed.
- [ ] *(LLM)* teaching-episode extraction path via `memory.episodes` — defer if logged out.
- **Pass:** SurrealDB populated from Engram; no cross-vignoble writes.

### Stage 5 — Recall service  (LLM: yes, for summarization)
- [ ] NATS request-reply on `pinard.<testname>.memory.recall` for each intent:
  - `recall` → HNSW semantic neighbours
  - `lookup` → FULLTEXT keyword hits
  - `trace` → `RELATE` graph neighbours
- [ ] Server-side Haiku summarization ≤400 tokens; degrades gracefully when LLM down.
- [ ] Fail-open: SurrealDB down → `{"context": null}`, worker proceeds.
- **Pass:** each intent returns relevant results; summarization + fail-open verified.

### Stage 6 — Roll-up & promotion  (LLM: no)
- [ ] Seed the same rule (e.g. "use `fix:`/`feat:` commit prefix") under `memtest-a` and `memtest-b`.
- [ ] Run roll-up + `PromotionCandidateDetector` (Rosetta cosine ≥0.85 / `/search` BM25).
- [ ] Candidate surfaces as an Obsidian card; PR bridge produces a human-gated PR.
- **Pass:** cross-scope recurrence detected; promotion is human-gated (no auto-merge).

### Stage 7 — Portable subset  (LLM: no)
- [ ] `python -m services.memory.surrealdb.subset --group-id memtest-a --out /tmp/memtest-a.surrealkv`.
- [ ] Inspect `subset_meta`: `pinard_core` + domain ontology versions, counts, `exported_at`.
- [ ] Load via `MEMORY_EMBEDDED_SUBSET=/tmp/memtest-a.surrealkv`; run recall from the embedded file.
- **Pass:** recall parity central↔embedded (cosine scan); version stamp correct.

### Stage 8 — Graphiti KG eval  (LLM: yes) — optional / Phase-2
- [ ] `SurrealDB → jsonl → Graphiti` (`export.py` → `ingest.py` → `assess.py`).
- **Pass:** produces measured recall/dedup data → feeds the deferred Phase-2 KG decision
  (keep Graphiti eval-only vs permanent vs Spectron).

### Stage 9 — End-to-end worker/conductor  (LLM: yes)
- [ ] In the test vignoble: `/teaching` a recipe → session-end episode → ingester extracts.
- [ ] Boot a new worker for the same `group_id` → recall injects the recipe at boot.
- [ ] Per-turn recall query returns relevant context; `/lesson` one-shot pin works.
- **Pass:** teach-once / recall-later loop works end to end (genomics reference scenario).

## D. Exit criteria (→ open `cuvee/memory → master`)

- [ ] Stages 1–4, 6, 7 pass (no-LLM core).
- [ ] Stages 5, 9 pass (with a live token).
- [ ] Exclusivity proof holds (only test `group_id` data changed; no other vignoble touched).
- [ ] Stage 8 findings recorded (Phase-2 decision input) — decision itself may remain deferred.
- [ ] (Recommended) the Python CI job is in place so the combined branch has real coverage.

## E. Rollback / restore (if the test goes wrong)

**Key fact:** `cuvee/memory` is 100% additive/standalone. It changes **no**
worker/conductor code (`pi-extension` untouched), so basic vignoble features
(agent spawning, MR flow, webterm, babysitter, core NATS, Engram-as-conductor-memory)
run on code identical to `master` and **cannot be broken by memory-layer code**. The
memory layer only *reads* Engram (no `/lesson`//`teaching` wired yet). The only
runtime binary delta is an **additive** `pinard-memory` JetStream stream (+ a
backward-compatible `maxAge` field; existing streams unchanged). `master` is the
pristine baseline.

Restore in tiers, lightest first:

| Tier | Action | Effect | Cost |
|------|--------|--------|------|
| **0 — nothing** | (inherent) | Basic features never depended on the memory layer; they keep working even during the test. | — |
| **1 — stop sidecars** | Stop the ingester, recall service, graphiti-eval, SurrealDB | Memory features off; base vignoble untouched. | seconds |
| **2 — clean teardown** | `nats stream rm pinard-memory` (+ its consumers); drop the test `group_id` DBs in SurrealDB; delete `memory-*-status.json`, memory logs, subset files, test `.wiki/` | Shared infra back to pre-test state. | minutes |
| **3 — baseline binary** | Redeploy the pinard binary from `master` | Removes even the additive `pinard-memory` stream management. Low urgency (additive). | redeploy |

### Pre-test insurance (do before Stage 1)
- [ ] Record the current deployed pinard ref (the `master` SHA) — the Tier-3 target.
- [ ] Snapshot the vignoble's Engram DB (cheap insurance; memory layer is read-only against Engram today).
- [ ] Note every env var added (`SURREAL_URL`, `SURREAL_USER/PASS`, `NATS_URL`, `NATS_VIGNOBLE`, `ENGRAM_URL`, `MEMORY_TOKEN_URL`, `VIGNOBLE_LOGS`, `group_id`s) so they can be unset.
- [ ] `nats stream ls` before starting — so `pinard-memory` is known to be the only stream to remove.

### Shared-backend caveat
Keep Tier-2 teardown **targeted** (only `pinard-memory` stream + test `group_id` DBs).
Since §A requires exclusive use (no other vignoble active on the shared backends
during the test), even a broad reset is safe — but targeted cleanup is cleaner.

## Notes / known caveats

- **No Python CI today** — this manual pass is the substitute; a Python test job
  (`pip install -e packages/pinard-core && pytest services/memory/tests/`) should
  land before/with the master merge.
- **LLM token dance:** extraction (teaching) + recall summarization depend on
  `MEMORY_TOKEN_URL` being valid (only when logged in). Embeddings (Rosetta) do not.
- **Engram relation judgment is MCP-only** (no HTTP endpoint) — promotion recurrence
  uses Rosetta cosine + `/search`, not a REST judgment call.
- **Phase-2 KG** and **host disk usage** are tracked separately from this plan.
