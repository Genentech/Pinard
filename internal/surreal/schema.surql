-- pinard memory layer — SurrealDB schema (static base only)
-- Apply against any group_id database after defining the namespace/database.
-- Run: surreal sql --endpoint <url> --user root --pass <pass> --file schema.surql
--
-- Namespace: pinard
-- Database:  one per group_id (vigne/pipeline scope), e.g. USE DB `genomics-build`;
-- The schema below uses the `pinard` namespace and a placeholder database `_template`.
-- The ingester applies it to each group_id database dynamically.
--
-- NOTE: Graph edge RELATION tables are no longer hardcoded here.
-- They are generated dynamically from the composed ontology (core + domain) by
-- internal/ontology.GenerateSurrealDDL() (replaces the deleted schema_gen.py).
-- This file is the static base; dynamic DDL is applied per group_id at startup.

-- ─── Namespace ────────────────────────────────────────────────────────────────
DEFINE NAMESPACE IF NOT EXISTS pinard;
-- Note: no `USE NS` here — the client sets NS/DB context via SDK .use() before
-- executing this file, so a bare `USE NS` would wipe the database context.

-- ─── Template database (applied per group_id by the ingester) ────────────────
-- The ingester calls .use(namespace, group_id) then applies these definitions.

-- ─── Entities ─────────────────────────────────────────────────────────────────
-- Core entity types mirror the pinard-core Pydantic ontology.

DEFINE TABLE IF NOT EXISTS entity SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS name        ON entity TYPE string;
DEFINE FIELD IF NOT EXISTS role        ON entity TYPE string;
-- option<string> tolerates legacy NONE rows without re-coercion on DEFINE FIELD
-- (mirrors the option<array<float>> pattern used for the embedding field).
-- New upserts always store real string values; Go readers yield "" for NONE.
DEFINE FIELD OVERWRITE description ON entity TYPE option<string> DEFAULT "";
DEFINE FIELD OVERWRITE version     ON entity TYPE option<string> DEFAULT "1.0.0";
DEFINE FIELD OVERWRITE provenance  ON entity TYPE option<string> DEFAULT "";
DEFINE FIELD IF NOT EXISTS created_at  ON entity TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS updated_at  ON entity TYPE datetime DEFAULT time::now();
-- Role-specific fields (stored as flexible record; absent when not applicable).
DEFINE FIELD IF NOT EXISTS data        ON entity TYPE object FLEXIBLE DEFAULT {};
-- Manual-edit guard: prevents re-extraction from clobbering human edits.
DEFINE FIELD IF NOT EXISTS manual_edit ON entity TYPE bool DEFAULT false;
-- 1024-dim embedding (Rosetta qwen3-emb-0.6b).
DEFINE FIELD IF NOT EXISTS embedding   ON entity TYPE option<array<float>>;

-- The entity_role_name UNIQUE index is redundant: the record id is already
-- entityRID(role, name) = sha256(role+"\0"+name), which enforces one-entity-
-- per-(role,name). In SurrealDB 3.x it also caused a self-conflict on every
-- UPSERT (the index saw the same (role,name) as a collision), silently
-- dropping entity updates. Drop the UNIQUE constraint; keep the index for the
-- Trace() WHERE role+name lookup.
REMOVE INDEX IF EXISTS entity_role_name ON entity;
DEFINE INDEX IF NOT EXISTS entity_role_name ON entity FIELDS role, name;

-- HNSW vector index for semantic recall (dim=1024, cosine distance).
DEFINE INDEX IF NOT EXISTS entity_embedding_hnsw
  ON entity
  FIELDS embedding
  HNSW DIMENSION 1024 DIST COSINE;

-- Full-text search index for lexical lookup.
DEFINE ANALYZER IF NOT EXISTS pinard_text TOKENIZERS blank, class FILTERS lowercase, snowball(english);
DEFINE INDEX IF NOT EXISTS entity_name_fts
  ON entity
  FIELDS name
  FULLTEXT ANALYZER pinard_text BM25;
DEFINE INDEX IF NOT EXISTS entity_description_fts
  ON entity
  FIELDS description
  FULLTEXT ANALYZER pinard_text BM25;

-- ─── Wiki documents ───────────────────────────────────────────────────────────
-- Obsidian markdown pages: body + frontmatter + backlinks + embedding.

DEFINE TABLE IF NOT EXISTS wiki_doc SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS title        ON wiki_doc TYPE string;
DEFINE FIELD IF NOT EXISTS type         ON wiki_doc TYPE string  DEFAULT "";
DEFINE FIELD IF NOT EXISTS body         ON wiki_doc TYPE string  DEFAULT "";
DEFINE FIELD IF NOT EXISTS frontmatter  ON wiki_doc TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS path         ON wiki_doc TYPE string  DEFAULT "";
DEFINE FIELD IF NOT EXISTS content_hash ON wiki_doc TYPE string  DEFAULT "";
DEFINE FIELD IF NOT EXISTS confidence   ON wiki_doc TYPE float   DEFAULT 1.0;
-- "needs_review" | "auto_serve" (Decision A8: ≥0.7 → auto_serve)
DEFINE FIELD IF NOT EXISTS status       ON wiki_doc TYPE string  DEFAULT "needs_review";
DEFINE FIELD IF NOT EXISTS created_at   ON wiki_doc TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS updated_at   ON wiki_doc TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS embedding    ON wiki_doc TYPE option<array<float>>;

-- path is the OKF concept ID and is the unique key; title is display-only
DEFINE INDEX IF NOT EXISTS wiki_doc_path  ON wiki_doc FIELDS path UNIQUE;
-- wiki_doc_title was UNIQUE in an older schema, which self-conflicts when the
-- curator re-titles a doc (two paths can legitimately share a title mid-
-- migration) — same class as the cursor indexes (#256/#265). OVERWRITE forces it
-- back to a plain (non-unique) lookup index on existing DBs; a plain
-- `DEFINE ... IF NOT EXISTS` would skip the existing UNIQUE definition.
DEFINE INDEX OVERWRITE wiki_doc_title ON wiki_doc FIELDS title;

DEFINE INDEX IF NOT EXISTS wiki_doc_embedding_hnsw
  ON wiki_doc
  FIELDS embedding
  HNSW DIMENSION 1024 DIST COSINE;

DEFINE INDEX IF NOT EXISTS wiki_doc_title_fts
  ON wiki_doc
  FIELDS title
  FULLTEXT ANALYZER pinard_text BM25;
DEFINE INDEX IF NOT EXISTS wiki_doc_body_fts
  ON wiki_doc
  FIELDS body
  FULLTEXT ANALYZER pinard_text BM25;

-- ─── Core graph edge tables (fallback only) ──────────────────────────────────
-- When GenerateSurrealDDL (ontology registry) is used, edge tables are generated
-- from the composed ontology (core + domain) instead.
-- This section covers the static fallback path (schema_path only).

DEFINE TABLE IF NOT EXISTS depends_on SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON depends_on TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON depends_on TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON depends_on TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON depends_on TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS produces SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON produces TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON produces TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON produces TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON produces TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS consumes SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON consumes TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON consumes TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON consumes TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON consumes TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS indicates_problem SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON indicates_problem TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON indicates_problem TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON indicates_problem TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON indicates_problem TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS resolved_by SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON resolved_by TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON resolved_by TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON resolved_by TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON resolved_by TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS requires_condition SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON requires_condition TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON requires_condition TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON requires_condition TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON requires_condition TYPE datetime DEFAULT time::now();

DEFINE TABLE IF NOT EXISTS triggers_decision SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON triggers_decision TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON triggers_decision TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON triggers_decision TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON triggers_decision TYPE datetime DEFAULT time::now();

-- ─── MR knowledge supersession ─────────────────────────────────────────────────
DEFINE TABLE IF NOT EXISTS supersedes SCHEMAFULL TYPE RELATION IN entity OUT entity;
DEFINE FIELD IF NOT EXISTS confidence  ON supersedes TYPE float DEFAULT 1.0;
DEFINE FIELD IF NOT EXISTS description ON supersedes TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS data        ON supersedes TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS created_at  ON supersedes TYPE datetime DEFAULT time::now();

-- ─── Staging tables (ontology gardener accumulates candidates here) ──────────
-- entity_staging: candidate entities observed ≥N times across sessions.
DEFINE TABLE IF NOT EXISTS entity_staging SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS name             ON entity_staging TYPE string;
DEFINE FIELD IF NOT EXISTS proposed_role    ON entity_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS description      ON entity_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS rationale        ON entity_staging TYPE string   DEFAULT "";
DEFINE FIELD OVERWRITE     provenance       ON entity_staging TYPE option<string> DEFAULT "";
DEFINE FIELD IF NOT EXISTS data             ON entity_staging TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS occurrence_count ON entity_staging TYPE int      DEFAULT 0;
DEFINE FIELD IF NOT EXISTS updated_at       ON entity_staging TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS embedding        ON entity_staging TYPE option<array<float>>;

DEFINE INDEX IF NOT EXISTS entity_staging_name ON entity_staging FIELDS name UNIQUE;

-- edge_staging: candidate graph edges observed ≥N times.
DEFINE TABLE IF NOT EXISTS edge_staging SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS from_name          ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS from_role          ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS to_name            ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS to_role            ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS proposed_relation  ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS description        ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD IF NOT EXISTS rationale          ON edge_staging TYPE string   DEFAULT "";
DEFINE FIELD OVERWRITE     provenance         ON edge_staging TYPE option<string> DEFAULT "";
DEFINE FIELD IF NOT EXISTS data               ON edge_staging TYPE object FLEXIBLE DEFAULT {};
DEFINE FIELD IF NOT EXISTS occurrence_count   ON edge_staging TYPE int      DEFAULT 0;
DEFINE FIELD IF NOT EXISTS updated_at         ON edge_staging TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS embedding          ON edge_staging TYPE option<array<float>>;

-- ingest_cursor: watermark table keyed by source (Engram project/session).
DEFINE TABLE IF NOT EXISTS ingest_cursor SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS source     ON ingest_cursor TYPE string;
DEFINE FIELD IF NOT EXISTS seq        ON ingest_cursor TYPE int    DEFAULT 0;
DEFINE FIELD IF NOT EXISTS updated_at ON ingest_cursor TYPE datetime DEFAULT time::now();

-- The ingest_cursor_source UNIQUE index is redundant: record id is already
-- a deterministic hash (cursorRID), which enforces one-cursor-per-source.
-- It also caused a SurrealDB-3.x self-conflict on every UPSERT, keeping the
-- cursor at 0 and making LAG always equal to engram_max. Drop it.
REMOVE INDEX IF EXISTS ingest_cursor_source ON ingest_cursor;

-- wiki_curator_cursor: same story. The record id is a deterministic hash
-- (wikiCursorRID), so a UNIQUE index on `source` is redundant and self-conflicts
-- on UPSERT ("index wiki_curator_cursor_source already contains 'wiki_curator:<gid>'"),
-- leaving the curator cursor stuck so it never advances. Drop it.
REMOVE INDEX IF EXISTS wiki_curator_cursor_source ON wiki_curator_cursor;

-- NOTE: the old schema had a DELETE ingest_cursor WHERE id != type::record(...)
-- cleanup here. That line was designed for the pre-hash scheme where the record
-- ID was the raw source string. With the hash-based cursorRID scheme the
-- expression type::record('ingest_cursor', source) no longer matches any hash
-- ID, so the DELETE was nuking every cursor row on every EnsureSchema call —
-- resetting all cursors to 0 each cycle.
-- Stale-cursor cleanup is now handled in Go by surreal.Client.DeduplicateCursors().

-- ─── Wiki backlinks ───────────────────────────────────────────────────────────
DEFINE TABLE IF NOT EXISTS wiki_references SCHEMAFULL TYPE RELATION IN wiki_doc OUT wiki_doc;
DEFINE FIELD IF NOT EXISTS edge_type  ON wiki_references TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS created_at ON wiki_references TYPE datetime DEFAULT time::now();

-- ─── Wiki ↔ entity cross-references ──────────────────────────────────────────
DEFINE TABLE IF NOT EXISTS wiki_mentions SCHEMAFULL TYPE RELATION IN wiki_doc OUT entity;
DEFINE FIELD IF NOT EXISTS edge_type  ON wiki_mentions TYPE string DEFAULT "";
DEFINE FIELD IF NOT EXISTS created_at ON wiki_mentions TYPE datetime DEFAULT time::now();

-- ─── Data migrations ─────────────────────────────────────────────────────────
-- Normalize entity.manual_edit=NONE rows created before the TYPE bool DEFAULT
-- was defined. SurrealDB 3.x rejects NONE for a TYPE bool field during UPSERT,
-- so these rows could never be updated until backfilled. Idempotent: a second
-- run matches zero rows and is a no-op.
UPDATE entity SET manual_edit = false WHERE manual_edit = NONE;

-- Backfill wiki_doc.type from frontmatter.type for rows written before
-- UpsertWikiDoc started mirroring it into the top-level field (#265). Without
-- this, role-scoped dedup queries (FindSimilarWikiDoc, FindWikiDocByTitle)
-- silently match nothing against every pre-existing doc — dedup goes blind
-- instead of collapsing the reported duplicates. Idempotent: a second run
-- matches zero rows once backfilled.
UPDATE wiki_doc SET type = frontmatter.type
  WHERE (type = "" OR type = NONE) AND frontmatter.type != NONE;

-- ─── Ingest statistics ─────────────────────────────────────────────────────────
-- Per-source ingestion counters updated at the end of each ingest cycle.
DEFINE TABLE IF NOT EXISTS ingest_stats SCHEMAFULL;
DEFINE FIELD IF NOT EXISTS source      ON ingest_stats TYPE string;
DEFINE FIELD IF NOT EXISTS ingested    ON ingest_stats TYPE int      DEFAULT 0;
DEFINE FIELD IF NOT EXISTS failed      ON ingest_stats TYPE int      DEFAULT 0;
DEFINE FIELD IF NOT EXISTS last_ingest ON ingest_stats TYPE datetime DEFAULT time::now();
DEFINE FIELD IF NOT EXISTS updated_at  ON ingest_stats TYPE datetime DEFAULT time::now();

DEFINE INDEX IF NOT EXISTS ingest_stats_source ON ingest_stats FIELDS source UNIQUE;
