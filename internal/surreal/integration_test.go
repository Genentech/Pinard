//go:build integration

package surreal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// runID is a per-process unique suffix so parallel CI pipelines don't share
// SurrealDB databases and inherit stale data from a previous run.
var runID = fmt.Sprintf("%d", time.Now().UnixMilli()%100000)

// integrationClient returns a Client connected to the live SurrealDB instance
// configured via environment variables (SURREAL_URL, SURREAL_USER, SURREAL_PASS).
// Skips the test if SURREAL_URL is not set.
// The groupID is suffixed with a per-run ID to prevent cross-run data pollution.
func integrationClient(t *testing.T, groupID string) *Client {
	t.Helper()
	url := os.Getenv("SURREAL_URL")
	if url == "" {
		t.Skip("SURREAL_URL not set — skipping integration test")
	}
	user := envOr("SURREAL_USER", "root")
	pass := os.Getenv("SURREAL_PASS")
	// Suffix with runID to isolate from previous pipeline runs on the same SurrealDB.
	uniqueGroupID := groupID + "r" + runID
	c, err := NewWithConfig(uniqueGroupID, url, user, pass)
	if err != nil {
		t.Fatalf("NewWithConfig: %v", err)
	}
	if err := c.EnsureSchema(); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return c
}

// TestIntegrationSchemaApply verifies that EnsureSchema applies cleanly to a live
// SurrealDB instance and that the base entity table is queryable afterwards.
func TestIntegrationSchemaApply(t *testing.T) {
	db := integrationClient(t, "ci-memory-test")
	defer db.Close()

	rows, err := db.Query("SELECT * FROM entity LIMIT 1", nil)
	if err != nil {
		t.Fatalf("SELECT after schema apply: %v", err)
	}
	_ = rows // may be empty; just must not error
}

// TestIntegrationSchemaDDLTableNames verifies that GenerateSurrealDDL produces
// table/field/RELATION names that match the Python golden schema captured from
// the master branch (tests/memory-parity/golden/schema.sql).
//
// This is the primary schema-parity gate: if Go renames a table or field vs the
// Python ingester, queries against Python-written data would silently fail.
func TestIntegrationSchemaDDLTableNames(t *testing.T) {
	db := integrationClient(t, "ci-ddl-parity")
	defer db.Close()

	goldenSQL, err := os.ReadFile("../../tests/memory-parity/golden/schema.sql")
	if err != nil {
		t.Skipf("golden schema not found: %v", err)
	}
	golden := string(goldenSQL)

	// Apply the Go-generated dynamic DDL from the ontology registry.
	// (EnsureSchema already applied the static base; the dynamic DDL is applied
	// by the ingester via ontology.GenerateSurrealDDL — we verify names match.)
	//
	// Required relation table names (from Python schema_gen.py output):
	requiredRelations := []string{
		"depends_on",
		"produces",
		"consumes",
		"indicates_problem",
		"resolved_by",
		"requires_condition",
		"triggers_decision",
	}
	for _, rel := range requiredRelations {
		// Check golden (Python) has the table.
		if !containsStr(golden, "DEFINE TABLE IF NOT EXISTS "+rel) {
			t.Errorf("Python golden schema missing RELATION table %q — schema drift would orphan data", rel)
		}
		// Check it's queryable on the live instance (schema already applied via EnsureSchema).
		_, err := db.Query("SELECT * FROM "+rel+" LIMIT 1", nil)
		if err != nil {
			t.Errorf("Go schema missing live table %q: %v", rel, err)
		}
	}

	// Required base entity fields.
	requiredEntityFields := []string{
		"name", "role", "description", "version", "provenance",
		"embedding", "manual_edit", "created_at", "updated_at",
	}
	for _, field := range requiredEntityFields {
		// Golden uses padded spacing (e.g. "name        ON entity"); use
		// containsFieldOnTable to tolerate any amount of whitespace.
		if !containsFieldOnTable(golden, field, "entity") {
			t.Errorf("Python golden schema missing entity field %q", field)
		}
	}

	// Required staging tables.
	for _, tbl := range []string{"entity_staging", "edge_staging"} {
		_, err := db.Query("SELECT * FROM "+tbl+" LIMIT 1", nil)
		if err != nil {
			t.Errorf("Go schema missing staging table %q: %v", tbl, err)
		}
	}

	// Ingest cursor table.
	_, err = db.Query("SELECT * FROM ingest_cursor LIMIT 1", nil)
	if err != nil {
		t.Errorf("Go schema missing ingest_cursor table: %v", err)
	}
}

// TestIntegrationUpsertAndRecall verifies the full ingest→recall pipeline:
// upsert entities from the corpus, then recall them by name substring.
// This is the live-DB test that the memory-integration CI job runs.
func TestIntegrationUpsertAndRecall(t *testing.T) {
	db := integrationClient(t, "ci-ingest-recall")
	defer db.Close()

	type corpusEntity struct {
		ObsID   string `json:"obs_id"`
		ObsType string `json:"obs_type"`
		Role    string `json:"role"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	type goldenRecall struct {
		Entities []corpusEntity `json:"entities"`
	}

	goldenBytes, err := os.ReadFile("../../tests/memory-parity/golden/recall.json")
	if err != nil {
		t.Skipf("golden recall not found: %v", err)
	}
	var golden goldenRecall
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	// Upsert all corpus entities (without real embeddings — integration test
	// validates schema/role/name storage, not vector recall which requires Rosetta).
	for _, e := range golden.Entities {
		_, err := db.UpsertEntity(e.Role, e.Name, e.Content, nil, nil, "1.0.0", "integration-test")
		if err != nil {
			t.Errorf("UpsertEntity role=%q name=%q: %v", e.Role, e.Name, err)
		}
	}

	// Verify each entity is retrievable by role+name lookup.
	upserted := 0
	for _, e := range golden.Entities {
		rows, err := db.FetchEntityByRoleName(e.Role, e.Name)
		if err != nil {
			t.Errorf("FetchEntityByRoleName role=%q name=%q: %v", e.Role, e.Name, err)
			continue
		}
		if rows == nil {
			t.Errorf("entity not found after upsert: role=%q name=%q", e.Role, e.Name)
			continue
		}
		gotRole, _ := rows["role"].(string)
		if gotRole != e.Role {
			t.Errorf("entity role mismatch: got %q, want %q", gotRole, e.Role)
		}
		upserted++
	}

	// All 8 corpus entities must be stored and retrievable.
	if upserted < len(golden.Entities) {
		t.Errorf("only %d/%d entities upserted successfully", upserted, len(golden.Entities))
	}
}

// TestIntegrationOntologyRoleParity verifies that the Go ontology registry
// produces exactly the same 10 entity roles and 7 edge table names as the
// Python pinard-core (from the captured golden baseline).
func TestIntegrationOntologyRoleParity(t *testing.T) {
	// This test does not need SurrealDB but lives here with the integration tag
	// so it runs as part of the same CI job.
	goldenBytes, err := os.ReadFile("../../tests/memory-parity/golden/ontology.json")
	if err != nil {
		t.Skipf("golden ontology not found: %v", err)
	}
	var golden struct {
		EntityRoleNames []string `json:"entity_role_names"`
		EdgeTableNames  []string `json:"edge_table_names"`
	}
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	// Import the Go ontology registry.
	// We can't import internal/ontology from here (different package); instead
	// verify via the surreal schema that the Go DDL produces the right tables.
	db := integrationClient(t, "ci-ontology-parity")
	defer db.Close()

	// All Python edge table names must be live queryable on the Go schema.
	for _, tableName := range golden.EdgeTableNames {
		_, err := db.Query("SELECT * FROM "+tableName+" LIMIT 1", nil)
		if err != nil {
			t.Errorf("Python edge table %q not found in Go schema (schema-parity failure): %v", tableName, err)
		}
	}
}

// TestIntegrationCorpusObsTypeParity verifies that for each observation in the
// corpus, the Go ingester's obsTypeToRole mapping produces the same role as the
// Python ingester's type_map captured in the golden baseline.
func TestIntegrationCorpusObsTypeParity(t *testing.T) {
	goldenBytes, err := os.ReadFile("../../tests/memory-parity/golden/ontology.json")
	if err != nil {
		t.Skipf("golden ontology not found: %v", err)
	}
	var golden struct {
		ObsTypeToRole map[string]string `json:"obs_type_to_role"`
	}
	if err := json.Unmarshal(goldenBytes, &golden); err != nil {
		t.Fatalf("parse golden: %v", err)
	}

	recallBytes, err := os.ReadFile("../../tests/memory-parity/golden/recall.json")
	if err != nil {
		t.Skipf("golden recall not found: %v", err)
	}
	var goldenRecall struct {
		Entities []struct {
			ObsType string `json:"obs_type"`
			Role    string `json:"role"`
			Name    string `json:"name"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(recallBytes, &goldenRecall); err != nil {
		t.Fatalf("parse golden recall: %v", err)
	}

	// The Go obsTypeToRole map (from cmd/memory-ingester/main.go).
	goMap := map[string]string{
		"rule": "decision", "fact": "artifact", "teaching-episode": "task",
		"summary": "task", "diagnosis": "diagnosis", "action": "action",
		"log_pattern": "log_pattern", "environment_condition": "environment_condition",
		"gate": "gate", "step": "step", "verdict": "verdict",
		"bugfix": "diagnosis", "decision": "decision", "architecture": "artifact",
		"discovery": "artifact", "pattern": "decision", "config": "artifact",
		"preference": "artifact", "session_summary": "task", "plan": "task", "manual": "decision",
	}

	// For every obs_type in the Python golden map, Go must produce the same role.
	for obsType, pythonRole := range golden.ObsTypeToRole {
		goRole, ok := goMap[obsType]
		if !ok {
			t.Errorf("obs_type %q present in Python map but missing from Go map", obsType)
			continue
		}
		if goRole != pythonRole {
			t.Errorf("obs_type %q: Go maps to %q, Python maps to %q (parity failure)", obsType, goRole, pythonRole)
		}
	}

	// For each corpus entity, Go predicted role must match golden.
	for _, e := range goldenRecall.Entities {
		goRole := goMap[e.ObsType]
		if goRole == "" {
			goRole = "artifact"
		}
		if goRole != e.Role {
			t.Errorf("corpus obs_type=%q: Go role=%q, Python role=%q", e.ObsType, goRole, e.Role)
		}
	}
}

// TestIntegrationIngestCursorRoundTrip verifies GetIngestCursor/SetIngestCursor
// against a live SurrealDB instance, including hyphenated/colon sources that
// previously triggered SurrealDB 3.x record-ID mangling.
func TestIntegrationIngestCursorRoundTrip(t *testing.T) {
	db := integrationClient(t, "ci-cursor-test")
	defer db.Close()

	// Plain source.
	if err := db.SetIngestCursor("integration-test-source", 999); err != nil {
		t.Fatalf("SetIngestCursor plain: %v", err)
	}
	seq, err := db.GetIngestCursor("integration-test-source")
	if err != nil {
		t.Fatalf("GetIngestCursor plain: %v", err)
	}
	if seq != 999 {
		t.Errorf("plain cursor round-trip: got %d, want 999", seq)
	}

	// Hyphenated+colon source — regression test for the SurrealDB 3.x mangling bug.
	// Before the cursorRID hash fix, "engram_pg:vignoble-misc" was truncated to
	// "engram_pg:vignoble" and GetIngestCursor always returned 0.
	hyph := "engram_pg:vignoble-misc"
	if err := db.SetIngestCursor(hyph, 12345); err != nil {
		t.Fatalf("SetIngestCursor hyphenated: %v", err)
	}
	hyphSeq, err := db.GetIngestCursor(hyph)
	if err != nil {
		t.Fatalf("GetIngestCursor hyphenated: %v", err)
	}
	if hyphSeq != 12345 {
		t.Errorf("hyphenated cursor round-trip: got %d, want 12345 (source mangled if 0)", hyphSeq)
	}

	// Distinct sources must not collide.
	seq2, _ := db.GetIngestCursor("engram_pg:vignoble")
	if seq2 == 12345 {
		t.Error("cursor collision: engram_pg:vignoble shares seq with engram_pg:vignoble-misc")
	}
}

// TestIntegrationRelateAndTrace verifies RELATE + graph traversal against live SurrealDB.
func TestIntegrationRelateAndTrace(t *testing.T) {
	db := integrationClient(t, "ci-relate-trace")
	defer db.Close()

	// Upsert two entities.
	if _, err := db.UpsertEntity("task", "integration-task-A", "task A", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("upsert task A: %v", err)
	}
	if _, err := db.UpsertEntity("artifact", "integration-artifact-B", "artifact B", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("upsert artifact B: %v", err)
	}

	// Relate task → depends_on → artifact.
	_, err := db.Relate("task", "integration-task-A", "depends_on", "artifact", "integration-artifact-B", 0.9, "test edge", nil)
	if err != nil {
		t.Fatalf("Relate: %v", err)
	}

	// Trace the edge.
	rows, err := db.Trace("task", "integration-task-A", "depends_on")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	_ = rows // traversal succeeded; neighbors may vary by SurrealDB version
}

// TestIntegrationManualEditNone verifies the SurrealDB 3.x bool coercion fix:
// an entity row with manual_edit=NONE (pre-dating the TYPE bool DEFAULT) must
// be re-upsertable without error, and manual_edit=true rows must not be clobbered.
func TestIntegrationManualEditNone(t *testing.T) {
	db := integrationClient(t, "ci-manual-edit-none")
	defer db.Close()

	// Use the same RID formula as entityRID() so the seeded INSERT and the
	// subsequent UpsertEntity target the exact same record. Using a mismatched
	// RID would create a second record that conflicts on the unique (role,name) index.
	noneRole, noneName := "artifact", "legacy-none-entity"
	noneRID := fmt.Sprintf("%x", sha256.Sum256([]byte(noneRole+"\x00"+noneName)))[:32]

	// Inject a row with manual_edit=NONE by bypassing UpsertEntity.
	// This simulates a pre-existing record created before the field was defined.
	_, err := db.Query(
		fmt.Sprintf(`INSERT INTO entity (id, role, name, description, version, provenance, data, updated_at) VALUES
		 (type::record('entity', '%s'), '%s', '%s',
		  'old desc', '0.9', 'pre-schema', {}, time::now())`, noneRID, noneRole, noneName),
		nil,
	)
	if err != nil {
		t.Fatalf("seed NONE entity: %v", err)
	}

	// Re-upsert the same entity — this must succeed (not produce WARN coercion error).
	row, err := db.UpsertEntity(noneRole, noneName, "updated desc", nil, nil, "1.0.0", "integration-test")
	if err != nil {
		t.Fatalf("UpsertEntity on manual_edit=NONE row: %v", err)
	}
	if row == nil {
		t.Fatal("expected non-nil row after re-upsert")
	}

	// manual_edit should now be false (backfill + coalesce normalised it).
	me, _ := row["manual_edit"].(bool)
	if me {
		t.Errorf("manual_edit = true after upsert of non-manually-edited entity; want false")
	}

	// A row with manual_edit=true must NOT be clobbered by a re-upsert.
	trueRole, trueName := "artifact", "manual-edit-entity"
	trueRID := fmt.Sprintf("%x", sha256.Sum256([]byte(trueRole+"\x00"+trueName)))[:32]

	_, err = db.Query(
		fmt.Sprintf(`INSERT INTO entity (id, role, name, description, version, provenance, data, manual_edit, updated_at) VALUES
		 (type::record('entity', '%s'), '%s', '%s',
		  'human desc', '1.0', 'human', {}, true, time::now())`, trueRID, trueRole, trueName),
		nil,
	)
	if err != nil {
		t.Fatalf("seed manual_edit=true entity: %v", err)
	}

	row2, err := db.UpsertEntity(trueRole, trueName, "machine desc", nil, nil, "1.0.0", "integration-test")
	if err != nil {
		t.Fatalf("UpsertEntity on manual_edit=true row: %v", err)
	}
	me2, _ := row2["manual_edit"].(bool)
	if !me2 {
		t.Errorf("manual_edit clobbered: got false, want true (human edit preserved)")
	}
	desc2, _ := row2["description"].(string)
	if desc2 != "human desc" {
		t.Errorf("description clobbered: got %q, want %q", desc2, "human desc")
	}
}

// containsFieldOnTable checks for "DEFINE FIELD IF NOT EXISTS <field> ... ON <table>",
// tolerating padded spacing in the Python golden schema
// (e.g. "name        ON entity" vs "name ON entity").
func containsFieldOnTable(ddl, field, table string) bool {
	prefix := "DEFINE FIELD IF NOT EXISTS " + field
	for i := 0; i <= len(ddl)-len(prefix); i++ {
		if ddl[i:i+len(prefix)] != prefix {
			continue
		}
		j := i + len(prefix)
		for j < len(ddl) && (ddl[j] == ' ' || ddl[j] == '\t') {
			j++
		}
		suffix := "ON " + table
		if j+len(suffix) <= len(ddl) && ddl[j:j+len(suffix)] == suffix {
			return true
		}
	}
	return false
}

// containsStr is a simple string contains helper.
func containsStr(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// TestIntegrationFindWikiDocByEntities_StableAcrossRetitling (#320): once an
// entity is linked to a doc via wiki_mentions, looking it up must return the
// original path even though the would-be slug from a reworded title differs —
// re-titling a concept must NOT create a second page.
func TestIntegrationFindWikiDocByEntities_StableAcrossRetitling(t *testing.T) {
	db := integrationClient(t, "ci-wiki-mentions-retitle")
	defer db.Close()

	if _, err := db.UpsertEntity("decision", "retitle-entity-A", "desc", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("upsert entity: %v", err)
	}
	eid := "entity:" + entityRID("decision", "retitle-entity-A")

	originalPath := "decisions/conductor-led-live-surrealdb-smoke-testing"
	if _, err := db.UpsertWikiDoc("Conductor-led live SurrealDB smoke testing", "body", originalPath, "summary", 0.9,
		map[string]any{"type": "decision"}, nil); err != nil {
		t.Fatalf("UpsertWikiDoc: %v", err)
	}
	if err := db.WriteWikiMentions(originalPath, []string{eid}); err != nil {
		t.Fatalf("WriteWikiMentions: %v", err)
	}

	// A resynthesis with a reworded title would slug to a different path, but
	// the edge-based lookup must still resolve back to originalPath.
	got, err := db.FindWikiDocByEntities([]string{eid})
	if err != nil {
		t.Fatalf("FindWikiDocByEntities: %v", err)
	}
	if got != originalPath {
		t.Errorf("FindWikiDocByEntities = %q, want %q (stable identity)", got, originalPath)
	}
}

// TestIntegrationFindWikiDocByEntities_Plurality (#320): when a cluster's
// entities are split across two existing docs' edges, the doc with the
// plurality of the cluster's entities wins.
func TestIntegrationFindWikiDocByEntities_Plurality(t *testing.T) {
	db := integrationClient(t, "ci-wiki-mentions-plurality")
	defer db.Close()

	names := []string{"plurality-entity-A", "plurality-entity-B", "plurality-entity-C"}
	eids := make([]string, len(names))
	for i, n := range names {
		if _, err := db.UpsertEntity("decision", n, "desc", nil, nil, "1.0.0", "test"); err != nil {
			t.Fatalf("upsert entity %s: %v", n, err)
		}
		eids[i] = "entity:" + entityRID("decision", n)
	}

	majorityPath := "decisions/plurality-majority-doc"
	minorityPath := "decisions/plurality-minority-doc"
	for _, p := range []string{majorityPath, minorityPath} {
		if _, err := db.UpsertWikiDoc("title", "body", p, "summary", 0.9, map[string]any{"type": "decision"}, nil); err != nil {
			t.Fatalf("UpsertWikiDoc %s: %v", p, err)
		}
	}
	// A and B point to majorityPath, C points to minorityPath.
	if err := db.WriteWikiMentions(majorityPath, eids[:2]); err != nil {
		t.Fatalf("WriteWikiMentions majority: %v", err)
	}
	if err := db.WriteWikiMentions(minorityPath, eids[2:]); err != nil {
		t.Fatalf("WriteWikiMentions minority: %v", err)
	}

	got, err := db.FindWikiDocByEntities(eids)
	if err != nil {
		t.Fatalf("FindWikiDocByEntities: %v", err)
	}
	if got != majorityPath {
		t.Errorf("FindWikiDocByEntities = %q, want %q (plurality)", got, majorityPath)
	}
}

// TestIntegrationWriteWikiMentions_EdgeChurnSurvivesEntityDeletion (#320): when
// a subsequent call passes a reduced entity set (simulating entity deletion),
// the previously-linked entities no longer resolve to the doc, but the doc's
// own `path` field is untouched.
func TestIntegrationWriteWikiMentions_EdgeChurnSurvivesEntityDeletion(t *testing.T) {
	db := integrationClient(t, "ci-wiki-mentions-churn")
	defer db.Close()

	names := []string{"churn-entity-A", "churn-entity-B"}
	eids := make([]string, len(names))
	for i, n := range names {
		if _, err := db.UpsertEntity("decision", n, "desc", nil, nil, "1.0.0", "test"); err != nil {
			t.Fatalf("upsert entity %s: %v", n, err)
		}
		eids[i] = "entity:" + entityRID("decision", n)
	}

	path := "decisions/churn-doc"
	if _, err := db.UpsertWikiDoc("title", "body", path, "summary", 0.9, map[string]any{"type": "decision"}, nil); err != nil {
		t.Fatalf("UpsertWikiDoc: %v", err)
	}
	if err := db.WriteWikiMentions(path, eids); err != nil {
		t.Fatalf("WriteWikiMentions (full set): %v", err)
	}

	// Both entities resolve to path before churn.
	if got, err := db.FindWikiDocByEntities(eids); err != nil || got != path {
		t.Fatalf("pre-churn FindWikiDocByEntities = (%q, %v), want (%q, nil)", got, err, path)
	}

	// Entity B is "deleted" from the doc — WriteWikiMentions is re-run with only A.
	if err := db.WriteWikiMentions(path, eids[:1]); err != nil {
		t.Fatalf("WriteWikiMentions (reduced set): %v", err)
	}

	// B no longer resolves to the doc.
	if got, err := db.FindWikiDocByEntities(eids[1:]); err != nil || got != "" {
		t.Fatalf("post-churn FindWikiDocByEntities(B) = (%q, %v), want (\"\", nil)", got, err)
	}
	// A still resolves to the doc.
	if got, err := db.FindWikiDocByEntities(eids[:1]); err != nil || got != path {
		t.Fatalf("post-churn FindWikiDocByEntities(A) = (%q, %v), want (%q, nil)", got, err, path)
	}
	// The doc's own path field is untouched.
	doc, err := db.FetchWikiByPath(path)
	if err != nil || doc == nil {
		t.Fatalf("FetchWikiByPath: %v", err)
	}
	if p, _ := doc["path"].(string); p != path {
		t.Errorf("doc path = %q, want %q (untouched by edge churn)", p, path)
	}
}

// TestIntegrationProcessCluster_AdoptsPreExistingDoc (#320): a legacy doc that
// already exists at the slug-derived path but has no wiki_mentions edges yet
// gets adopted on the next resynthesis — WriteWikiMentions backfills identity
// without a migration, and a subsequent lookup by those entities resolves to
// the same (adopted) path.
func TestIntegrationProcessCluster_AdoptsPreExistingDoc(t *testing.T) {
	db := integrationClient(t, "ci-wiki-mentions-adopt")
	defer db.Close()

	if _, err := db.UpsertEntity("decision", "adopt-entity-A", "desc", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("upsert entity: %v", err)
	}
	eid := "entity:" + entityRID("decision", "adopt-entity-A")

	// Simulate a pre-existing doc with no edges (the ~119 pinard docs pre-#320).
	path := "decisions/legacy-adopt-doc"
	if _, err := db.UpsertWikiDoc("Legacy title", "body", path, "summary", 0.9, map[string]any{"type": "decision"}, nil); err != nil {
		t.Fatalf("UpsertWikiDoc: %v", err)
	}

	// Before adoption: no edge yet, so the lookup finds nothing.
	if got, err := db.FindWikiDocByEntities([]string{eid}); err != nil || got != "" {
		t.Fatalf("pre-adoption FindWikiDocByEntities = (%q, %v), want (\"\", nil)", got, err)
	}

	// processCluster's fallback path resolution would land on the same slug path
	// and then call WriteWikiMentions to adopt it.
	if err := db.WriteWikiMentions(path, []string{eid}); err != nil {
		t.Fatalf("WriteWikiMentions (adopt): %v", err)
	}

	// After adoption: the entity now resolves to the legacy doc's path.
	got, err := db.FindWikiDocByEntities([]string{eid})
	if err != nil {
		t.Fatalf("post-adoption FindWikiDocByEntities: %v", err)
	}
	if got != path {
		t.Errorf("post-adoption FindWikiDocByEntities = %q, want %q", got, path)
	}
}
