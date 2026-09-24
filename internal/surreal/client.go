// Package surreal provides a SurrealDB HTTP client for the pinard memory layer.
//
// Uses the SurrealDB HTTP JSON-RPC API (v2+).  Each client is scoped to a
// single namespace+database (group_id).
//
// Environment variables:
//
//	SURREAL_URL  — SurrealDB endpoint (default: http://localhost:8000)
//	SURREAL_USER — root username      (default: root)
//	SURREAL_PASS — root password      (required)
package surreal

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed schema.surql
var schemaFS embed.FS

// Namespace used by the pinard memory layer.
const Namespace = "pinard"

// EmbeddingDim is the expected embedding vector dimension.
const EmbeddingDim = 1024

// Error is returned on SurrealDB query/API errors.
type Error struct {
	Message string
}

func (e *Error) Error() string { return "surreal: " + e.Message }

func surErr(msg string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(msg, args...)}
}

// rpcResult holds one statement result from the RPC response.
type rpcResult struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result"`
}

// rpcResponse is the envelope returned by the SurrealDB HTTP RPC endpoint.
type rpcResponse struct {
	ID     any              `json:"id"`
	Result []rpcResult      `json:"result"`
	Error  *json.RawMessage `json:"error"`
}

// Client is a synchronous SurrealDB client scoped to a group_id database.
type Client struct {
	groupID string
	url     string
	user    string
	pass    string
	hc      *http.Client

	schemaMu      sync.Mutex
	schemaApplied map[string]bool
}

// New creates a Client for group_id using environment defaults.
func New(groupID string) (*Client, error) {
	url := strings.TrimRight(envOr("SURREAL_URL", "http://localhost:8000"), "/")
	user := envOr("SURREAL_USER", "root")
	pass := os.Getenv("SURREAL_PASS")
	return NewWithConfig(groupID, url, user, pass)
}

// NewWithConfig creates a Client with explicit connection parameters.
func NewWithConfig(groupID, url, user, pass string) (*Client, error) {
	c := &Client{
		groupID:       groupID,
		url:           strings.TrimRight(url, "/"),
		user:          user,
		pass:          pass,
		hc:            &http.Client{Timeout: 30 * time.Second},
		schemaApplied: make(map[string]bool),
	}
	return c, nil
}

// Close is a no-op (HTTP connections are pooled by net/http).
func (c *Client) Close() {}

// ── Internal HTTP/RPC ─────────────────────────────────────────────────────────

// queryRaw sends a SurrealQL string and returns the raw per-statement results.
func (c *Client) queryRaw(sql string, vars map[string]any) ([]rpcResult, error) {
	type rpcReq struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	params := []any{sql}
	if len(vars) > 0 {
		params = append(params, vars)
	}
	body, err := json.Marshal(rpcReq{Method: "query", Params: params})
	if err != nil {
		return nil, surErr("marshal: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.url+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, surErr("new request: %v", err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Surreal-NS", Namespace)
	req.Header.Set("Surreal-DB", c.groupID)

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, surErr("http: %v", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, surErr("HTTP %d: %s", resp.StatusCode, string(rawBody))
	}

	var rpc rpcResponse
	if err := json.Unmarshal(rawBody, &rpc); err != nil {
		return nil, surErr("unmarshal response: %v — body: %s", err, string(rawBody))
	}
	if rpc.Error != nil {
		return nil, surErr("rpc error: %s", string(*rpc.Error))
	}
	return rpc.Result, nil
}

// Query executes SurrealQL and returns all result sets decoded as []any.
func (c *Client) Query(sql string, vars map[string]any) ([]any, error) {
	stmts, err := c.queryRaw(sql, vars)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(stmts))
	for _, s := range stmts {
		if s.Status == "ERR" {
			var msg string
			_ = json.Unmarshal(s.Result, &msg)
			return nil, surErr("statement failed: %s", msg)
		}
		var v any
		_ = json.Unmarshal(s.Result, &v)
		out = append(out, v)
	}
	return out, nil
}

// queryOne executes SurrealQL expecting a single statement and returns its
// result decoded as []map[string]any (records) or nil.
func (c *Client) queryOne(sql string, vars map[string]any) ([]map[string]any, error) {
	results, err := c.Query(sql, vars)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}
	return toRecords(results[0]), nil
}

// ── Schema ────────────────────────────────────────────────────────────────────

// queryNS sends a SurrealQL string scoped to the namespace only (no database
// header). Used to run DEFINE DATABASE before the database exists.
func (c *Client) queryNS(sql string) ([]rpcResult, error) {
	type rpcReq struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
	}
	body, err := json.Marshal(rpcReq{Method: "query", Params: []any{sql}})
	if err != nil {
		return nil, surErr("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.url+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, surErr("new request: %v", err)
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Surreal-NS", Namespace)
	// Deliberately no Surreal-DB header — we are operating at namespace scope.
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, surErr("http: %v", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, surErr("HTTP %d: %s", resp.StatusCode, string(rawBody))
	}
	var rpc rpcResponse
	if err := json.Unmarshal(rawBody, &rpc); err != nil {
		return nil, surErr("unmarshal response: %v — body: %s", err, string(rawBody))
	}
	if rpc.Error != nil {
		return nil, surErr("rpc error: %s", string(*rpc.Error))
	}
	return rpc.Result, nil
}

// isRetryableConflict reports whether a SurrealDB error message represents a
// retryable write conflict that the caller should back off and retry.
func isRetryableConflict(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "write conflict") || strings.Contains(lower, "can be retried")
}

// EnsureSchema applies the embedded schema.surql idempotently (once per groupID
// per process). On SurrealDB 3.x the database must exist before tables can be
// defined; this function creates it if absent (idempotent, IF NOT EXISTS).
// Retries on retryable SurrealDB write conflicts (up to 5 attempts, exponential
// backoff with jitter) — concurrent callers on the same namespace can race on
// DEFINE NAMESPACE/DATABASE in SurrealDB 3.x.
func (c *Client) EnsureSchema() error {
	c.schemaMu.Lock()
	defer c.schemaMu.Unlock()
	if c.schemaApplied[c.groupID] {
		return nil
	}

	// Step 1: ensure the namespace + database exist at root/NS scope.
	// DEFINE NAMESPACE must run without any database context;
	// DEFINE DATABASE must run in the namespace but without a DB context.
	// Both are IF NOT EXISTS so they are safe to re-run.
	// Backtick-quote the identifiers: SurrealDB 3.x requires quoting for
	// names that contain hyphens (e.g. "vignoble-misc", "ci-memory-test").
	bootDDL := fmt.Sprintf(
		"DEFINE NAMESPACE IF NOT EXISTS `%s`; DEFINE DATABASE IF NOT EXISTS `%s`;",
		Namespace, c.groupID,
	)
	const maxAttempts = 5
	baseDelay := 50 * time.Millisecond
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			jitter := time.Duration(rand.Int63n(int64(baseDelay)))
			time.Sleep(baseDelay + jitter)
			baseDelay *= 2
		}
		stmts, err := c.queryNS(bootDDL)
		if err != nil {
			if isRetryableConflict(err.Error()) && attempt < maxAttempts-1 {
				continue
			}
			return surErr("create namespace/database: %v", err)
		}
		var retry bool
		var stmtErr error
		for _, s := range stmts {
			if s.Status == "ERR" {
				var msg string
				_ = json.Unmarshal(s.Result, &msg)
				if isRetryableConflict(msg) && attempt < maxAttempts-1 {
					retry = true
					break
				}
				stmtErr = surErr("create namespace/database: %s", msg)
				break
			}
		}
		if retry {
			continue
		}
		if stmtErr != nil {
			return stmtErr
		}
		break
	}

	// Step 2: apply the table/field/index DDL scoped to the group database.
	sql, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		return surErr("read embedded schema: %v", err)
	}
	baseDelay = 50 * time.Millisecond
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			jitter := time.Duration(rand.Int63n(int64(baseDelay)))
			time.Sleep(baseDelay + jitter)
			baseDelay *= 2
		}
		stmts, qErr := c.queryRaw(string(sql), nil)
		if qErr != nil {
			if isRetryableConflict(qErr.Error()) && attempt < maxAttempts-1 {
				continue
			}
			return surErr("apply schema: %v", qErr)
		}
		var retry bool
		var stmtErr error
		for _, s := range stmts {
			if s.Status == "ERR" {
				var msg string
				_ = json.Unmarshal(s.Result, &msg)
				if isRetryableConflict(msg) && attempt < maxAttempts-1 {
					retry = true
					break
				}
				stmtErr = surErr("schema statement failed: %s", msg)
				break
			}
		}
		if retry {
			continue
		}
		if stmtErr != nil {
			return stmtErr
		}
		break
	}
	c.schemaApplied[c.groupID] = true
	return nil
}

// ── Entity operations ─────────────────────────────────────────────────────────

// entityRID computes the deterministic record ID for (role, name).
func entityRID(role, name string) string {
	h := sha256.Sum256([]byte(role + "\x00" + name))
	return fmt.Sprintf("%x", h[:16])
}

// UpsertEntity upserts an entity record. When manual_edit=true the description
// and embedding are preserved.
//
// SurrealDB 3.x notes:
//   - Bare field references (e.g. `description`) in SET expressions are
//     ambiguous — they can be misread as record IDs (table:id). Use the
//     `this.` prefix to reference the current record's existing field value.
//   - option<array<float>> fields only accept NONE or a real array; JSON null
//     maps to SurrealDB NULL which the type-checker rejects. Emit the NONE
//     keyword in SurrealQL when embedding is absent rather than binding nil.
// upsertEntityPrecheck checks if an entity exists and has manual_edit=true.
// Returns (true, nil) if we should preserve existing description/embedding.
func (c *Client) upsertEntityPrecheck(rid string) (bool, error) {
	rows, err := c.queryOne(`SELECT manual_edit FROM type::record('entity',$rid) LIMIT 1`,
		map[string]any{"rid": rid})
	if err != nil || len(rows) == 0 {
		return false, err
	}
	me, _ := rows[0]["manual_edit"].(bool)
	return me, nil
}

// UpsertEntity upserts an entity record. When manual_edit=true the description
// and embedding are preserved.
//
// SurrealDB 3.x notes:
//   - IF…THEN…ELSE expressions with field references (this.description) in the
//     THEN branch are evaluated eagerly on both paths; on a new record the field
//     value may be coerced as a record-ID if its default is absent. Avoid by
//     doing the manual_edit check in Go before building the SET clause.
//   - option<array<float>> fields reject JSON null; use NONE keyword when absent.
func (c *Client) UpsertEntity(role, name, description string, data map[string]any, embedding []float64, version, provenance string) (map[string]any, error) {
	rid := entityRID(role, name)

	// Check manual_edit flag before building SET — avoids SurrealDB 3.x
	// eager-evaluation issue with IF…THEN this.field ELSE $param END.
	manualEdit, _ := c.upsertEntityPrecheck(rid)

	vars := map[string]any{
		"rid": rid, "role": role, "name": name,
		"version": version, "data": orEmpty(data), "provenance": provenance,
	}

	var descSQL, embSQL string
	if manualEdit {
		// Preserve existing human-edited description and embedding.
		// In SurrealDB 3.x, `this.<field>` inside a SET clause does not
		// reliably read the pre-existing value (evaluates to NONE on some
		// records). Use a self-SELECT subquery — the same pattern used by
		// UpsertEntityStaging for occurrence_count — which reliably returns
		// the stored value and coalesces NONE→"" for legacy rows.
		descSQL = `description=type::string((SELECT VALUE description FROM type::record('entity',$rid))[0]??"")`
		embSQL = `embedding=(SELECT VALUE embedding FROM type::record('entity',$rid))[0]??NONE`
	} else {
		// type::string() coerces the bound value to a string even if SurrealDB
		// 3.x misidentifies it as a record ID (e.g. "ERROR:Engram...").
		descSQL = "description=type::string($description)"
		vars["description"] = description
		if len(embedding) > 0 {
			embSQL = "embedding=$embedding"
			vars["embedding"] = embedding
		} else {
			// option<array<float>> requires NONE, not JSON null.
			embSQL = "embedding=NONE"
		}
	}

	// RETURN NONE avoids SurrealDB 3.x coercion-on-return: when the stored
	// description starts with 'TOKEN:...' the returned UPSERT record is
	// mis-parsed as a record ID, causing a type coercion failure even though
	// the write itself succeeded. We verify success via a separate SELECT.
	//
	// manual_edit=manual_edit??false: coalesces NONE→false so pre-existing
	// records with manual_edit=NONE (created before the field/default existed)
	// no longer fail SurrealDB 3.x bool coercion on re-upsert. Leaves true
	// values intact (true??false == true).
	sql := fmt.Sprintf(`UPSERT type::record('entity', $rid) SET
role=type::string($role), name=type::string($name), version=type::string($version),
data=$data, provenance=type::string($provenance),
updated_at=time::now(), manual_edit=manual_edit??false, %s, %s
RETURN NONE`, descSQL, embSQL)

	_, err := c.queryOne(sql, vars)
	if err != nil {
		return nil, err
	}
	// Fetch the record back to return the stored state.
	return c.FetchEntityByRoleName(role, name)
}

// UpsertEntityStaging upserts a record into entity_staging.
func (c *Client) UpsertEntityStaging(name, proposedRole, description, rationale, provenance string, data map[string]any, embedding []float64) (map[string]any, error) {
	rid := fmt.Sprintf("%x", sha256.Sum256([]byte("staging\x00"+name)))[:32]
	sql := `UPSERT type::record('entity_staging', $rid) SET
name=type::string($name), proposed_role=type::string($proposed_role),
description=type::string($description), rationale=type::string($rationale),
provenance=type::string($provenance), data=$data,
occurrence_count=(SELECT VALUE occurrence_count FROM type::record('entity_staging', $rid))[0]??0+1,
updated_at=time::now()`
	vars := map[string]any{
		"rid": rid, "name": name, "proposed_role": proposedRole,
		"description": description, "rationale": rationale,
		"provenance": provenance, "data": orEmpty(data),
	}
	if embedding != nil {
		sql += ", embedding=$embedding"
		vars["embedding"] = embedding
	}
	rows, err := c.queryOne(sql, vars)
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// UpsertEdgeStaging upserts a record into edge_staging.
func (c *Client) UpsertEdgeStaging(fromName, fromRole, toName, toRole, proposedRelation, description, rationale, provenance string, data map[string]any, embedding []float64) (map[string]any, error) {
	key := "staging_edge\x00" + fromName + "\x00" + proposedRelation + "\x00" + toName
	rid := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))[:32]
	sql := `UPSERT type::record('edge_staging', $rid) SET
from_name=type::string($from_name), from_role=type::string($from_role),
to_name=type::string($to_name), to_role=type::string($to_role),
proposed_relation=type::string($proposed_relation),
description=type::string($description),
rationale=type::string($rationale), provenance=type::string($provenance), data=$data,
occurrence_count=(SELECT VALUE occurrence_count FROM type::record('edge_staging', $rid))[0]??0+1,
updated_at=time::now()`
	vars := map[string]any{
		"rid": rid, "from_name": fromName, "from_role": fromRole,
		"to_name": toName, "to_role": toRole,
		"proposed_relation": proposedRelation, "description": description,
		"rationale": rationale, "provenance": provenance, "data": orEmpty(data),
	}
	if embedding != nil {
		sql += ", embedding=$embedding"
		vars["embedding"] = embedding
	}
	rows, err := c.queryOne(sql, vars)
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// Relate creates a graph edge between two entities.
func (c *Client) Relate(fromRole, fromName, relation, toRole, toName string, confidence float64, description string, data map[string]any) (map[string]any, error) {
	fromRID := entityRID(fromRole, fromName)
	toRID := entityRID(toRole, toName)
	rel := sanitizeIdentifier(relation)
	if rel == "" {
		return nil, surErr("invalid relation name: %q", relation)
	}
	sql := fmt.Sprintf(`RELATE (type::record('entity',$from_rid))->%s->(type::record('entity',$to_rid))
SET confidence=$confidence, description=type::string($description), data=$data`, rel)
	rows, err := c.queryOne(sql, map[string]any{
		"from_rid": fromRID, "to_rid": toRID,
		"confidence": confidence, "description": description, "data": orEmpty(data),
	})
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// ── Staging read (for ontology gardener) ─────────────────────────────────────

// ListEntityStaging returns entity_staging rows with occurrence_count >= minOccurrence.
func (c *Client) ListEntityStaging(minOccurrence, limit int) ([]map[string]any, error) {
	sql := `SELECT * FROM entity_staging WHERE occurrence_count>=$min ORDER BY occurrence_count DESC LIMIT $limit`
	return c.queryOne(sql, map[string]any{"min": minOccurrence, "limit": limit})
}

// ListEdgeStaging returns edge_staging rows with occurrence_count >= minOccurrence.
func (c *Client) ListEdgeStaging(minOccurrence, limit int) ([]map[string]any, error) {
	sql := `SELECT * FROM edge_staging WHERE occurrence_count>=$min ORDER BY occurrence_count DESC LIMIT $limit`
	return c.queryOne(sql, map[string]any{"min": minOccurrence, "limit": limit})
}

// ── Ingest cursor ─────────────────────────────────────────────────────────────

// cursorRID returns a safe, deterministic record ID for an ingest_cursor row.
//
// SurrealDB 3.x parses the suffix of `type::record('ingest_cursor', $id)` as a
// raw record-ID string.  When the source contains `:` or `-` (e.g.
// "engram_pg:vignoble-misc") SurrealDB mis-parses additional `:` as a second
// table:id separator, silently truncating the stored source to "engram_pg:vignoble".
// Using a hex-encoded SHA-256 hash (first 16 bytes = 32 hex chars) as the record
// ID avoids any special characters while remaining deterministic and collision-free
// for all practical source strings.  The actual source string is always stored in
// the `source` field so it is human-readable in the DB.
func cursorRID(source string) string {
	h := sha256.Sum256([]byte("ingest_cursor\x00" + source))
	return fmt.Sprintf("%x", h[:16])
}

// GetIngestCursor returns the last ingested seq for source, or 0.
// Uses SELECT VALUE which returns a JSON array of scalars (not records);
// the result is decoded via queryRaw to avoid toRecords() wrapping issues.
func (c *Client) GetIngestCursor(source string) (int64, error) {
	rid := cursorRID(source)
	sql := `SELECT VALUE seq FROM type::record('ingest_cursor',$rid)`
	stmts, err := c.queryRaw(sql, map[string]any{"rid": rid})
	if err != nil {
		return 0, err
	}
	if len(stmts) == 0 || stmts[0].Status == "ERR" {
		return 0, nil
	}
	// SELECT VALUE returns a JSON array of scalars: [42] or []
	var vals []any
	if err := json.Unmarshal(stmts[0].Result, &vals); err != nil {
		return 0, nil
	}
	if len(vals) == 0 {
		return 0, nil
	}
	return toInt64(vals[0]), nil
}

// SetIngestCursor persists seq as the last ingested position for source.
func (c *Client) SetIngestCursor(source string, seq int64) error {
	rid := cursorRID(source)
	sql := `UPSERT type::record('ingest_cursor',$rid) SET source=type::string($source), seq=$seq, updated_at=time::now()`
	_, err := c.queryOne(sql, map[string]any{"rid": rid, "source": source, "seq": seq})
	return err
}

// GetAllCursors returns all ingest_cursor rows, ordered by source.
func (c *Client) GetAllCursors() ([]map[string]any, error) {
	sql := `SELECT id, source, seq, updated_at FROM ingest_cursor ORDER BY source`
	return c.queryOne(sql, nil)
}

// DeduplicateCursors removes duplicate ingest_cursor rows for each source,
// keeping only the row with the highest seq.  It also removes rows whose
// source field is empty (legacy records written before the hash-ID scheme).
//
// Background: before the cursorRID hash fix, using the raw source string as the
// record-ID suffix caused SurrealDB to mangle colon-separated sources
// ("engram_pg:misc" stored OK, but "engram_pg:vignoble-misc" was truncated and
// its record ID collided with "engram_pg:vignoble").  Old deployments may have
// accumulated duplicate rows or rows with a source field that no longer matches
// the new hash-derived record ID.  This method cleans up both cases.
func (c *Client) DeduplicateCursors() error {
	// Step 1: delete rows with an empty source field (legacy orphans).
	sql := `DELETE ingest_cursor WHERE source = "" OR source = NONE`
	if _, err := c.queryOne(sql, nil); err != nil {
		return surErr("DeduplicateCursors: delete empty-source rows: %v", err)
	}

	// Step 2: For each distinct source value, keep only the row with the highest
	// seq and delete any others.  This resolves duplicates that arose when the
	// same logical source was written under two different record IDs (old raw-id
	// scheme vs. new hash scheme) with the source field correctly populated.
	rows, err := c.queryOne(`SELECT source, math::max(seq) AS max_seq, count() AS cnt FROM ingest_cursor GROUP BY source HAVING cnt > 1`, nil)
	if err != nil {
		// Non-fatal: dedup is best-effort cleanup.
		return nil
	}
	for _, row := range rows {
		src, _ := row["source"].(string)
		if src == "" {
			continue
		}
		maxSeq := toInt64(row["max_seq"])
		// Delete all rows for this source that are NOT the canonical hash-keyed record
		// and do NOT have the max seq (they are stale duplicates).
		// We keep the canonical record (via cursorRID) and delete any others.
		canonicalRID := cursorRID(src)
		// First ensure the canonical record exists and has the max seq.
		if err := c.SetIngestCursor(src, maxSeq); err != nil {
			continue
		}
		// Then delete all rows for this source whose record ID is NOT the canonical one.
		sql := `DELETE ingest_cursor WHERE source = $src AND id != type::record('ingest_cursor', $rid)`
		if _, err := c.queryOne(sql, map[string]any{"src": src, "rid": canonicalRID}); err != nil {
			// Best-effort: log nothing here (callers log warnings).
			continue
		}
	}
	return nil
}

// ── Recall (vector search) ────────────────────────────────────────────────────

// Recall finds nearest entities using the HNSW KNN index.
func (c *Client) Recall(embedding []float64, limit int, roleFilter string) ([]map[string]any, error) {
	k := limit
	ef := 100
	var sql string
	vars := map[string]any{"vec": embedding}
	if roleFilter != "" {
		sql = fmt.Sprintf(`SELECT *, vector::distance::knn() AS dist FROM entity WHERE embedding <|%d,%d|> $vec AND role=$role ORDER BY dist`, k, ef)
		vars["role"] = roleFilter
	} else {
		sql = fmt.Sprintf(`SELECT *, vector::distance::knn() AS dist FROM entity WHERE embedding <|%d,%d|> $vec ORDER BY dist`, k, ef)
	}
	return c.queryOne(sql, vars)
}

// RecallCosineScan does a full-scan cosine fallback.
func (c *Client) RecallCosineScan(embedding []float64, limit int, roleFilter string) ([]map[string]any, error) {
	where := "WHERE embedding IS NOT NULL"
	vars := map[string]any{"vec": embedding, "limit": limit}
	if roleFilter != "" {
		where += " AND role=$role"
		vars["role"] = roleFilter
	}
	sql := fmt.Sprintf(`SELECT *, vector::similarity::cosine(embedding,$vec) AS score FROM entity %s ORDER BY score DESC LIMIT $limit`, where)
	return c.queryOne(sql, vars)
}

// ── Lookup (FTS) ──────────────────────────────────────────────────────────────

// Lookup finds entities by full-text match.
func (c *Client) Lookup(text string, limit int) ([]map[string]any, error) {
	sql := `SELECT *, math::max([search::score(1),search::score(2)]) AS score FROM entity WHERE name @1@ $text OR description @2@ $text ORDER BY score DESC LIMIT $limit`
	return c.queryOne(sql, map[string]any{"text": text, "limit": limit})
}

// ── Trace (graph traversal) ───────────────────────────────────────────────────

// Trace traverses graph edges from an entity.
func (c *Client) Trace(fromRole, fromName, relation string) ([]map[string]any, error) {
	rel := sanitizeIdentifier(relation)
	sql := fmt.Sprintf(`SELECT ->%s->entity.* AS neighbors FROM entity WHERE role=$role AND name=$name LIMIT 1`, rel)
	return c.queryOne(sql, map[string]any{"role": fromRole, "name": fromName})
}

// ── Wiki operations ───────────────────────────────────────────────────────────

// RecallWiki finds nearest wiki_doc pages using HNSW.
func (c *Client) RecallWiki(embedding []float64, limit int, includeNeedsReview bool) ([]map[string]any, error) {
	k := limit
	ef := 100
	var sql string
	if includeNeedsReview {
		sql = fmt.Sprintf(`SELECT path,title,type,summary,body,confidence,status,vector::distance::knn() AS dist FROM wiki_doc WHERE embedding <|%d,%d|> $vec ORDER BY dist`, k, ef)
	} else {
		sql = fmt.Sprintf(`SELECT path,title,type,summary,body,confidence,status,vector::distance::knn() AS dist FROM wiki_doc WHERE embedding <|%d,%d|> $vec AND status='auto_serve' ORDER BY dist`, k, ef)
	}
	return c.queryOne(sql, map[string]any{"vec": embedding})
}

// LookupWiki finds wiki_doc pages by full-text.
func (c *Client) LookupWiki(text string, limit int, includeNeedsReview bool) ([]map[string]any, error) {
	var sql string
	if includeNeedsReview {
		sql = `SELECT path,title,type,summary,body,confidence,status,math::max([search::score(1),search::score(2)]) AS score FROM wiki_doc WHERE title @1@ $text OR body @2@ $text ORDER BY score DESC LIMIT $limit`
	} else {
		sql = `SELECT path,title,type,summary,body,confidence,status,math::max([search::score(1),search::score(2)]) AS score FROM wiki_doc WHERE (title @1@ $text OR body @2@ $text) AND status='auto_serve' ORDER BY score DESC LIMIT $limit`
	}
	return c.queryOne(sql, map[string]any{"text": text, "limit": limit})
}

// UpsertWikiDoc upserts a wiki document. The top-level `type` field mirrors
// frontmatter["type"] (the entity role, e.g. "decision") so the role-scoped
// dedup query (FindSimilarWikiDoc) can filter without unpacking the FLEXIBLE
// frontmatter object (#265).
func (c *Client) UpsertWikiDoc(title, body, path, summary string, confidence float64, frontmatter map[string]any, embedding []float64) (map[string]any, error) {
	status := "auto_serve"
	if confidence < 0.7 {
		status = "needs_review"
	}
	rid := fmt.Sprintf("%x", sha256.Sum256([]byte("wiki_doc\x00"+path)))[:32]
	role, _ := frontmatter["type"].(string)
	sql := `UPSERT type::record('wiki_doc',$rid) SET
title=type::string($title),type=type::string($role),body=type::string($body),
summary=type::string($summary),frontmatter=$frontmatter,
path=type::string($path),confidence=$confidence,
status=type::string($status),updated_at=time::now()`
	vars := map[string]any{
		"rid": rid, "title": title, "role": role, "body": body, "summary": summary,
		"frontmatter": orEmpty(frontmatter), "path": path,
		"confidence": confidence, "status": status,
	}
	if embedding != nil {
		sql += ",embedding=$embedding"
		vars["embedding"] = embedding
	}
	rows, err := c.queryOne(sql, vars)
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// FetchWikiByPath returns a single wiki_doc by path, or nil.
func (c *Client) FetchWikiByPath(path string) (map[string]any, error) {
	sql := `SELECT path,title,body,confidence,status FROM wiki_doc WHERE path=$path LIMIT 1`
	rows, err := c.queryOne(sql, map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// FetchEntityByRoleName returns a single entity by (role, name), or nil.
func (c *Client) FetchEntityByRoleName(role, name string) (map[string]any, error) {
	rid := entityRID(role, name)
	sql := `SELECT * FROM type::record('entity',$rid) LIMIT 1`
	rows, err := c.queryOne(sql, map[string]any{"rid": rid})
	if err != nil {
		return nil, err
	}
	return firstRow(rows), nil
}

// DeleteWikiChunksByPath deletes all wiki_chunk rows for a path.
func (c *Client) DeleteWikiChunksByPath(path string) (int, error) {
	sql := `DELETE wiki_chunk WHERE parent_path=$path RETURN BEFORE`
	rows, err := c.queryOne(sql, map[string]any{"path": path})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// UpsertWikiChunks upserts a list of wiki_chunk rows.
func (c *Client) UpsertWikiChunks(chunks []WikiChunk) error {
	for _, ch := range chunks {
		rid := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("wiki_chunk\x00%s\x00%d", ch.ParentPath, ch.ChunkIndex))))[:32]
		sql := `UPSERT type::record('wiki_chunk',$rid) SET parent_path=type::string($parent_path),heading=type::string($heading),chunk_index=$chunk_index,text=type::string($text),created_at=time::now()`
		vars := map[string]any{
			"rid": rid, "parent_path": ch.ParentPath,
			"heading": ch.Heading, "chunk_index": ch.ChunkIndex, "text": ch.Text,
		}
		if ch.Embedding != nil {
			sql += ",embedding=$embedding"
			vars["embedding"] = ch.Embedding
		}
		if _, err := c.queryOne(sql, vars); err != nil {
			return err
		}
	}
	return nil
}

// RecallWikiChunks finds nearest wiki_chunk rows using HNSW.
func (c *Client) RecallWikiChunks(embedding []float64, limit int) ([]map[string]any, error) {
	k := limit
	ef := 100
	sql := fmt.Sprintf(`SELECT parent_path,heading,chunk_index,text,vector::distance::knn() AS dist FROM wiki_chunk WHERE embedding <|%d,%d|> $vec ORDER BY dist`, k, ef)
	return c.queryOne(sql, map[string]any{"vec": embedding})
}

// WikiChunk is a single chunk of a wiki document.
type WikiChunk struct {
	ParentPath string
	Heading    string
	ChunkIndex int
	Text       string
	Embedding  []float64
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func toRecords(v any) []map[string]any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{t}
	}
	return nil
}

func firstRow(rows []map[string]any) map[string]any {
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func nilIfEmpty(v []float64) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func sanitizeIdentifier(s string) string {
	var b strings.Builder
	for _, ch := range s {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
			b.WriteRune(ch)
		}
	}
	return b.String()
}

// ── Memory status helpers ─────────────────────────────────────────────────────

// IngestStats holds the per-cycle ingestion counters for a source.
type IngestStats struct {
	Source     string    `json:"source"`
	Ingested   int64     `json:"ingested"`
	Failed     int64     `json:"failed"`
	LastIngest time.Time `json:"last_ingest"`
}

// GetIngestStats returns the ingest_stats row for source, or zero values.
func (c *Client) GetIngestStats(source string) (IngestStats, error) {
	rows, err := c.queryOne(
		`SELECT source, ingested, failed, last_ingest FROM type::record('ingest_stats',$source)`,
		map[string]any{"source": source},
	)
	if err != nil || len(rows) == 0 {
		return IngestStats{Source: source}, err
	}
	r := rows[0]
	st := IngestStats{
		Source:   source,
		Ingested: toInt64(r["ingested"]),
		Failed:   toInt64(r["failed"]),
	}
	if s, ok := r["last_ingest"].(string); ok && s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			st.LastIngest = t
		}
	}
	return st, nil
}

// UpdateIngestStats upserts the cumulative ingestion counters for source.
func (c *Client) UpdateIngestStats(source string, ingested, failed int64) error {
	sql := `UPSERT type::record('ingest_stats',$source) SET
source=type::string($source),
ingested=$ingested,
failed=$failed,
last_ingest=time::now(),
updated_at=time::now()`
	_, err := c.queryOne(sql, map[string]any{
		"source":   source,
		"ingested": ingested,
		"failed":   failed,
	})
	return err
}

// WikiStats holds aggregate wiki_doc counts for a group_id database.
type WikiStats struct {
	TotalDocs   int64     `json:"total_docs"`
	AutoServe   int64     `json:"auto_serve"`
	NeedsReview int64     `json:"needs_review"`
	LastRollup  time.Time `json:"last_rollup,omitempty"`
}

// GetWikiStats returns wiki_doc counts grouped by status.
func (c *Client) GetWikiStats() (WikiStats, error) {
	rows, err := c.queryOne(
		`SELECT status, count() AS cnt FROM wiki_doc GROUP BY status`,
		nil,
	)
	if err != nil {
		return WikiStats{}, err
	}
	var st WikiStats
	for _, r := range rows {
		cnt := toInt64(r["cnt"])
		st.TotalDocs += cnt
		switch r["status"] {
		case "auto_serve":
			st.AutoServe = cnt
		case "needs_review":
			st.NeedsReview = cnt
		}
	}
	// last_rollup: most recently updated wiki_doc.
	tRows, err := c.queryOne(`SELECT updated_at FROM wiki_doc ORDER BY updated_at DESC LIMIT 1`, nil)
	if err == nil && len(tRows) > 0 {
		if s, ok := tRows[0]["updated_at"].(string); ok && s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				st.LastRollup = t
			}
		}
	}
	return st, nil
}

// GetEntityCount returns the count of entities in this group_id database.
func (c *Client) GetEntityCount() (int64, error) {
	rows, err := c.queryOne(`SELECT count() AS cnt FROM entity GROUP ALL`, nil)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	return toInt64(rows[0]["cnt"]), nil
}

// ── Wiki curator cursor ───────────────────────────────────────────────────────

func wikiCursorRID(groupID string) string {
	h := sha256.Sum256([]byte("wiki_curator_cursor\x00" + groupID))
	return fmt.Sprintf("%x", h[:16])
}

// GetWikiCuratorCursor returns the last curator run timestamp for groupID.
// Returns (zero, false, nil) when no cursor exists (first run).
func (c *Client) GetWikiCuratorCursor(groupID string) (time.Time, bool, error) {
	rid := wikiCursorRID(groupID)
	rows, err := c.queryOne(
		`SELECT last_synced_at FROM type::record('wiki_curator_cursor',$rid) LIMIT 1`,
		map[string]any{"rid": rid},
	)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(rows) == 0 {
		return time.Time{}, false, nil
	}
	row := rows[0]
	val, ok := row["last_synced_at"]
	if !ok || val == nil {
		return time.Time{}, false, nil
	}
	switch v := val.(type) {
	case string:
		t, err := time.Parse(time.RFC3339Nano, strings.ReplaceAll(v, " ", "T"))
		if err != nil {
			return time.Time{}, false, nil
		}
		return t, true, nil
	case float64:
		// SurrealDB may return epoch milliseconds as a number.
		return time.UnixMilli(int64(v)).UTC(), true, nil
	}
	return time.Time{}, false, nil
}

// SetWikiCuratorCursor advances the curator cursor to time::now() (server-side).
func (c *Client) SetWikiCuratorCursor(groupID string) error {
	rid := wikiCursorRID(groupID)
	// type::string() forces the value to a string literal; without it SurrealDB
	// parses the `:`-containing value ("wiki_curator:<gid>") as a record link and
	// fails to coerce it into the string `source` field (same fix as SetIngestCursor).
	sql := `UPSERT type::record('wiki_curator_cursor',$rid) SET ` +
		`source=type::string($src), last_synced_at=time::now(), updated_at=time::now()`
	_, err := c.queryOne(sql, map[string]any{"rid": rid, "src": "wiki_curator:" + groupID})
	return err
}

// FetchEntitiesSinceCursor returns up to 500 entities updated after `since`.
// Pass nil to fetch all entities (first run).
func (c *Client) FetchEntitiesSinceCursor(since *time.Time) ([]map[string]any, error) {
	var sql string
	var vars map[string]any
	if since == nil {
		sql = `SELECT *, updated_at FROM entity ORDER BY updated_at ASC LIMIT 500`
		vars = nil
	} else {
		sql = `SELECT *, updated_at FROM entity WHERE updated_at > $since ORDER BY updated_at ASC LIMIT 500`
		vars = map[string]any{"since": since.UTC().Format(time.RFC3339Nano)}
	}
	return c.queryOne(sql, vars)
}

// FindSimilarWikiDoc returns the path and cosine score of the most similar
// existing wiki_doc with the same role, or ("", 0, nil) when none exceeds the
// threshold. Scoped by role (#265) to avoid cross-role false positives.
func (c *Client) FindSimilarWikiDoc(role string, embedding []float64) (string, float64, error) {
	sql := `SELECT path, vector::similarity::cosine(embedding, $vec) AS score ` +
		`FROM wiki_doc WHERE embedding IS NOT NULL AND type=$role ` +
		`ORDER BY score DESC LIMIT 1`
	rows, err := c.queryOne(sql, map[string]any{"vec": embedding, "role": role})
	if err != nil {
		return "", 0, err
	}
	if len(rows) == 0 {
		return "", 0, nil
	}
	row := rows[0]
	path, _ := row["path"].(string)
	var score float64
	switch sv := row["score"].(type) {
	case float64:
		score = sv
	case json.Number:
		score, _ = sv.Float64()
	}
	return path, score, nil
}

// FetchAllAutoServeWikiDocs returns all wiki_doc rows with status='auto_serve'.
func (c *Client) FetchAllAutoServeWikiDocs() ([]map[string]any, error) {
	return c.queryOne(
		`SELECT path, title, body, summary, confidence, frontmatter, updated_at FROM wiki_doc WHERE status='auto_serve'`,
		nil,
	)
}

// FetchEntityEdges returns typed outbound edges for the given entity record ID.
func (c *Client) FetchEntityEdges(entityID string, edgeNames []string) ([]map[string]any, error) {
	var edges []map[string]any
	for _, edgeName := range edgeNames {
		table := camelToSnake(edgeName)
		sql := fmt.Sprintf(`SELECT ->%s->entity.* AS targets FROM $eid LIMIT 1`, table)
		rows, err := c.queryOne(sql, map[string]any{"eid": entityID})
		if err != nil {
			continue
		}
		for _, row := range rows {
			targets, ok := row["targets"]
			if !ok || targets == nil {
				continue
			}
			var tlist []any
			switch tv := targets.(type) {
			case []any:
				tlist = tv
			default:
				tlist = []any{tv}
			}
			for _, t := range tlist {
				tm, ok := t.(map[string]any)
				if !ok {
					continue
				}
				name, _ := tm["name"].(string)
				if name == "" {
					continue
				}
				edges = append(edges, map[string]any{
					"edge":        edgeName,
					"target_name": name,
					"target_role": strVal(tm, "role"),
				})
			}
		}
	}
	return edges, nil
}

func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func strVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
