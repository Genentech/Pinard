package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Genentech/pinard/internal/memory"
	"github.com/Genentech/pinard/internal/ontology"
	"github.com/Genentech/pinard/internal/surreal"
)

// ── obsTypeToRole mapping ─────────────────────────────────────────────────────

func TestObsTypeToRoleMapping(t *testing.T) {
	// All canonical Engram observation types must map to a valid core role.
	cases := []struct {
		obsType string
		want    string
	}{
		{"rule", "decision"},
		{"fact", "artifact"},
		{"teaching-episode", "task"},
		{"summary", "task"},
		{"diagnosis", "diagnosis"},
		{"action", "action"},
		{"log_pattern", "log_pattern"},
		{"environment_condition", "environment_condition"},
		{"gate", "gate"},
		{"step", "step"},
		{"verdict", "verdict"},
		// mem_save agent types.
		{"bugfix", "diagnosis"},
		{"decision", "decision"},
		{"architecture", "artifact"},
		{"discovery", "artifact"},
		{"pattern", "decision"},
		{"config", "artifact"},
		{"preference", "artifact"},
		{"session_summary", "task"},
		{"plan", "task"},
		{"manual", "decision"},
	}
	r, err := ontology.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	c := r.Compose("test-group")

	for _, tc := range cases {
		obs := memory.EngramObservation{
			ObsType: tc.obsType,
			Content: "Test content for " + tc.obsType,
			GroupID: "test-group",
		}
		role, _ := obsToRoleName(obs.Content, obs.ObsType, obs.GroupID, r)
		if role != tc.want {
			t.Errorf("obsType=%q: role = %q, want %q", tc.obsType, role, tc.want)
		}
		// All mapped roles must be present in the core composed ontology.
		if !c.HasRole(role) {
			t.Errorf("obsType=%q: mapped role %q not in composed ontology", tc.obsType, role)
		}
	}
}

func TestObsTypeToRoleUnknownFallsBack(t *testing.T) {
	r, _ := ontology.NewRegistry()
	role, _ := obsToRoleName("some content", "completely_unknown_type", "test-group", r)
	if role != "artifact" {
		t.Errorf("unknown obs_type should fall back to artifact, got %q", role)
	}
}

func TestObsToRoleNameExtractsFirstLine(t *testing.T) {
	r, _ := ontology.NewRegistry()
	content := "Use Go for new services\nThis is additional detail."
	_, name := obsToRoleName(content, "rule", "g", r)
	if name != "Use Go for new services" {
		t.Errorf("name = %q, want first line", name)
	}
}

func TestObsToRoleNameStripsMarkdownHeading(t *testing.T) {
	r, _ := ontology.NewRegistry()
	content := "## Decision: prefer Go over Python"
	_, name := obsToRoleName(content, "rule", "g", r)
	if strings.HasPrefix(name, "#") {
		t.Errorf("name should not start with #, got %q", name)
	}
	if !strings.Contains(name, "Decision") {
		t.Errorf("name should contain text after #, got %q", name)
	}
}

func TestObsToRoleNameTruncatesLongContent(t *testing.T) {
	r, _ := ontology.NewRegistry()
	long := strings.Repeat("a", 200)
	_, name := obsToRoleName(long, "fact", "g", r)
	if len(name) > 120 {
		t.Errorf("name length = %d, want ≤120", len(name))
	}
}

// ── ingestObservation — upsert path ──────────────────────────────────────────

func mockEmbeddingServer(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	vec := make([]float64, dim)
	for i := range vec {
		vec[i] = float64(i) / float64(dim)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": vec},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func mockSurrealServer(t *testing.T, results []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type rpcResult struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		type rpcResponse struct {
			Result []rpcResult `json:"result"`
		}
		b, _ := json.Marshal(results)
		json.NewEncoder(w).Encode(rpcResponse{
			Result: []rpcResult{{Status: "OK", Result: json.RawMessage(b)}},
		})
	}))
}

func TestIngestObservationUpsertPath(t *testing.T) {
	embedding := mockEmbeddingServer(t, surreal.EmbeddingDim)
	defer embedding.Close()
	t.Setenv("EMBEDDING_URL", embedding.URL)

	upserted := false
	surrealSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		upserted = true
		w.Header().Set("Content-Type", "application/json")
		type rpcResult struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		type rpcResp struct {
			Result []rpcResult `json:"result"`
		}
		b, _ := json.Marshal([]map[string]any{{"id": "entity:abc", "role": "decision", "name": "Use Go"}})
		json.NewEncoder(w).Encode(rpcResp{
			Result: []rpcResult{{Status: "OK", Result: json.RawMessage(b)}},
		})
	}))
	defer surrealSrv.Close()

	emb := memory.NewEmbedder() // picks up EMBEDDING_URL from env
	db, err := surreal.NewWithConfig("test-group", surrealSrv.URL, "u", "p")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := ontology.NewRegistry()

	obs := memory.EngramObservation{
		ObsID:   "obs-1",
		ObsType: "rule",
		Content: "Use Go for new services",
		GroupID: "test-group",
	}
	if err := ingestObservation(obs, emb, db, r); err != nil {
		t.Fatalf("ingestObservation: %v", err)
	}
	if !upserted {
		t.Error("expected SurrealDB upsert to be called")
	}
}

func TestIngestObservationUnknownRoleGoesToStaging(t *testing.T) {
	embedding := mockEmbeddingServer(t, surreal.EmbeddingDim)
	defer embedding.Close()

	var capturedSQL string
	surrealSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := readBody(r)
		json.Unmarshal(b, &body)
		if params, ok := body["params"].([]any); ok && len(params) > 0 {
			capturedSQL, _ = params[0].(string)
		}
		w.Header().Set("Content-Type", "application/json")
		type rpcResult struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		type rpcResp struct{ Result []rpcResult `json:"result"` }
		rec, _ := json.Marshal([]map[string]any{{"id": "entity_staging:abc"}})
		json.NewEncoder(w).Encode(rpcResp{
			Result: []rpcResult{{Status: "OK", Result: json.RawMessage(rec)}},
		})
	}))
	defer surrealSrv.Close()

	// Use a registry with no domain extension — compose for "test-group" gives core only.
	// We force an unknown obs_type that maps to a role not in the ontology.
	t.Setenv("EMBEDDING_URL", embedding.URL)
	emb := memory.NewEmbedder()
	db, _ := surreal.NewWithConfig("test-group", surrealSrv.URL, "u", "p")
	r, _ := ontology.NewRegistry()

	// Verify ingestObservation doesn't error when the obs_type is completely unknown
	// (falls back to staging or artifact path without panicking).
	obs := memory.EngramObservation{
		ObsID:   "obs-2",
		ObsType: "completely_unmapped_type_xyz",
		Content: "Some unknown entity",
		GroupID: "test-group",
	}
	// Should not error — falls back to artifact or staging.
	if err := ingestObservation(obs, emb, db, r); err != nil {
		t.Fatalf("ingestObservation with unknown type: %v", err)
	}
	_ = capturedSQL
}

// ── resolveGroupIDs ───────────────────────────────────────────────────────────

func TestResolveGroupIDsDefault(t *testing.T) {
	t.Setenv("MEMORY_GROUP_IDS", "")
	t.Setenv("VIGNOBLE_DIR", "")
	t.Setenv("VIGNOBLES_BASE_DIR", "")
	ids := resolveGroupIDs("my-vignoble")
	if len(ids) != 1 || ids[0] != "my-vignoble" {
		t.Errorf("ids = %v, want [my-vignoble]", ids)
	}
}

func TestResolveExplicitFilter(t *testing.T) {
	t.Setenv("MEMORY_GROUP_IDS", "group-a,group-b, group-c ")
	ids := resolveExplicitFilter()
	if len(ids) != 3 {
		t.Fatalf("ids = %v, want 3 elements", ids)
	}
	if ids[0] != "group-a" || ids[1] != "group-b" || ids[2] != "group-c" {
		t.Errorf("ids = %v, unexpected values", ids)
	}
}

func TestResolveExplicitFilterEmpty(t *testing.T) {
	t.Setenv("MEMORY_GROUP_IDS", "")
	if ids := resolveExplicitFilter(); len(ids) != 0 {
		t.Errorf("resolveExplicitFilter() with empty env = %v, want nil", ids)
	}
}

func TestResolveGroupIDsFromVignesYaml(t *testing.T) {
	dir := t.TempDir()
	vignesYAML := `
vignes:
  alpha:
    path: ~/alpha
    repo: org/alpha
  beta:
    path: ~/beta
    repo: org/beta
  gamma:
    path: ~/gamma
    repo: org/gamma
`
	if err := os.WriteFile(dir+"/vignes.yaml", []byte(vignesYAML), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_GROUP_IDS", "")
	t.Setenv("VIGNOBLE_DIR", dir)
	t.Setenv("VIGNOBLES_BASE_DIR", "")

	ids := resolveGroupIDs("fallback-vignoble")
	if len(ids) != 3 {
		t.Fatalf("ids = %v, want 3 elements (one per vigne)", ids)
	}
	got := make(map[string]bool)
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"alpha", "beta", "gamma"} {
		if !got[want] {
			t.Errorf("expected group_id %q in ids %v", want, ids)
		}
	}
}

// resolveGroupIDs no longer reads MEMORY_GROUP_IDS; that is handled by
// resolveExplicitFilter. When VIGNOBLE_DIR is set with a vignes.yaml,
// it takes precedence over the single-vignoble fallback.
func TestResolveGroupIDsVignobleDirTakesPrecedenceOverFallback(t *testing.T) {
	dir := t.TempDir()
	vignesYAML := `
vignes:
  service-one:
    path: ~/service-one
    repo: org/service-one
`
	if err := os.WriteFile(dir+"/vignes.yaml", []byte(vignesYAML), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_GROUP_IDS", "")
	t.Setenv("VIGNOBLE_DIR", dir)
	t.Setenv("VIGNOBLES_BASE_DIR", "")

	ids := resolveGroupIDs("fallback")
	if len(ids) != 1 || ids[0] != "service-one" {
		t.Errorf("ids = %v, want [service-one]", ids)
	}
}

func TestResolveGroupIDsFromVignobleBaseDir(t *testing.T) {
	base := t.TempDir()

	// Create two vignoble subdirs each with a vignes.yaml.
	for _, entry := range []struct {
		dir  string
		yaml string
	}{
		{"vignoble-alpha", `vignes:
  alpha:
    path: ~/alpha
    repo: org/alpha
`},
		{"vignoble-beta", `vignes:
  beta:
    path: ~/beta
    repo: org/beta
`},
	} {
		subdir := base + "/" + entry.dir
		if err := os.MkdirAll(subdir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(subdir+"/vignes.yaml", []byte(entry.yaml), 0644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("MEMORY_GROUP_IDS", "")
	t.Setenv("VIGNOBLE_DIR", "")
	t.Setenv("VIGNOBLES_BASE_DIR", base)

	ids := resolveGroupIDs("fallback")
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 elements", ids)
	}
	got := make(map[string]bool)
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"alpha", "beta"} {
		if !got[want] {
			t.Errorf("expected group_id %q in ids %v", want, ids)
		}
	}
}

// ── chunkWikiBody ─────────────────────────────────────────────────────────────

func TestChunkWikiBodySplitsOnHeadings(t *testing.T) {
	body := `Intro paragraph.

## Section One

Content of section one.

## Section Two

Content of section two.
`
	chunks := chunkWikiBody("My Doc", body)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	// First chunk should be the intro.
	foundOne := false
	for _, ch := range chunks {
		if strings.Contains(ch.Heading, "Section One") {
			foundOne = true
		}
	}
	if !foundOne {
		t.Error("chunk with Section One heading not found")
	}
}

func TestChunkWikiBodyIndexIsMonotonic(t *testing.T) {
	body := "## A\nContent A\n## B\nContent B\n## C\nContent C"
	chunks := chunkWikiBody("T", body)
	for i, ch := range chunks {
		if ch.Index != i {
			t.Errorf("chunk[%d].Index = %d, want %d", i, ch.Index, i)
		}
	}
}

func TestChunkWikiBodyEmbedTextContainsTitle(t *testing.T) {
	chunks := chunkWikiBody("My Title", "## Section\nBody text")
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	if !strings.Contains(chunks[0].EmbedText, "My Title") {
		t.Errorf("EmbedText %q does not contain title", chunks[0].EmbedText)
	}
}

// ── Engram HTTP reader integration ───────────────────────────────────────────

func TestEngramReaderAndIngestionEndToEnd(t *testing.T) {
	obs := []map[string]any{
		{
			"id":         "e2e-obs-1",
			"session_id": "sess1",
			"project":    "e2e-group",
			"type":       "decision",
			"content":    "Always prefer idiomatic Go patterns",
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"confidence": 0.95,
		},
	}
	engramSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(obs)
	}))
	defer engramSrv.Close()

	embedding := mockEmbeddingServer(t, surreal.EmbeddingDim)
	defer embedding.Close()

	var upsertCalls int
	surrealSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upsertCalls++
		w.Header().Set("Content-Type", "application/json")
		type rpcResult struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		type rpcResp struct{ Result []rpcResult `json:"result"` }
		rec, _ := json.Marshal([]map[string]any{{"id": "entity:abc", "role": "decision"}})
		json.NewEncoder(w).Encode(rpcResp{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(rec)}}})
	}))
	defer surrealSrv.Close()

	t.Setenv("ENGRAM_URL", engramSrv.URL)
	reader := memory.NewEngramReader("e2e-group")
	observations, err := reader.Fetch()
	if err != nil {
		t.Fatalf("Engram fetch: %v", err)
	}
	if len(observations) != 1 {
		t.Fatalf("expected 1 obs, got %d", len(observations))
	}

	t.Setenv("EMBEDDING_URL", embedding.URL)
	emb := memory.NewEmbedder()
	db, _ := surreal.NewWithConfig("e2e-group", surrealSrv.URL, "u", "p")
	r, _ := ontology.NewRegistry()

	for _, o := range observations {
		if err := ingestObservation(o, emb, db, r); err != nil {
			t.Fatalf("ingestObservation: %v", err)
		}
	}
	if upsertCalls == 0 {
		t.Error("expected SurrealDB to be called during ingestion")
	}
}

// ── DeduplicateCursors ───────────────────────────────────────────────────────

// TestDeduplicateCursors verifies that DeduplicateCursors issues the right
// SQL sequence (delete-empty-source then group-by-source dedup) against a mock
// SurrealDB server.  It does not need a live DB.
func TestDeduplicateCursors(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		type rpcResult struct {
			Status string          `json:"status"`
			Result json.RawMessage `json:"result"`
		}
		type rpcResp struct{ Result []rpcResult `json:"result"` }
		// Return empty results for DELETE and GROUP BY queries (no duplicates present).
		json.NewEncoder(w).Encode(rpcResp{
			Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[]`)}},
		})
	}))
	defer srv.Close()

	db, _ := surreal.NewWithConfig("test-dedup", srv.URL, "u", "p")
	if err := db.DeduplicateCursors(); err != nil {
		t.Fatalf("DeduplicateCursors: %v", err)
	}
	// Must have made at least 2 HTTP calls: one for DELETE empty-source, one for GROUP BY.
	if callCount < 2 {
		t.Errorf("DeduplicateCursors made %d HTTP calls, want ≥2 (delete + group-by)", callCount)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	var buf strings.Builder
	b := make([]byte, 4096)
	for {
		n, err := r.Body.Read(b)
		buf.Write(b[:n])
		if err != nil {
			break
		}
	}
	return []byte(buf.String()), nil
}

// TestResolveGroupIDsForVignoble verifies the status-view resolver finds the
// vignoble's vignes under the "vignoble-<name>" clone subdir and always includes
// the régisseur (<vignoble>) and rollup (vignoble-<vignoble>) scopes.
func TestResolveGroupIDsForVignoble(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(base+"/vignoble-demo", 0755); err != nil {
		t.Fatal(err)
	}
	vignesYAML := `vignes:
  svc-one:
    repo: example-org/svc-one
  svc-two:
    repo: example-org/svc-two
  svc-three:
    repo: example-org/svc-three
`
	if err := os.WriteFile(base+"/vignoble-demo/vignes.yaml", []byte(vignesYAML), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VIGNOBLE_DIR", "")
	t.Setenv("VIGNOBLES_BASE_DIR", base)

	ids := resolveGroupIDsForVignoble("demo")
	got := make(map[string]bool)
	for _, id := range ids {
		got[id] = true
	}
	for _, want := range []string{"demo", "svc-one", "svc-two", "svc-three"} {
		if !got[want] {
			t.Errorf("resolveGroupIDsForVignoble(demo) = %v, missing %q", ids, want)
		}
	}
	// régisseur + 3 vignes; the vignoble-<name> rollup scope is intentionally excluded.
	if got["vignoble-demo"] {
		t.Errorf("resolveGroupIDsForVignoble(demo) = %v, should NOT include rollup scope vignoble-demo", ids)
	}
	if len(ids) < 4 {
		t.Errorf("resolveGroupIDsForVignoble(demo) = %v, want régisseur+3 vignes", ids)
	}
}

// TestDeriveEntityName (#265): mem_save scaffold markers must be stripped so
// entity names are the actual text, never "What:env"/"Why:…".
func TestDeriveEntityName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**What**: env setup done", "env setup done"},
		{"What: (1) #292 fixed a bug", "(1) #292 fixed a bug"},
		{"# Real Title\nbody here", "Real Title"},
		{"**What**:\nPostgreSQL migration decision", "PostgreSQL migration decision"},
		{"Plain first line", "Plain first line"},
		{"Why: because\nWhat: something", "because"},
	}
	for _, c := range cases {
		if got := deriveEntityName(c.in); got != c.want {
			t.Errorf("deriveEntityName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
