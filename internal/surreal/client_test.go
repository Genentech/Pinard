package surreal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockSurrealServer returns an httptest.Server that responds to RPC requests
// with a fixed result.
func mockSurrealServer(t *testing.T, results []rpcResult) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := rpcResponse{Result: results}
		json.NewEncoder(w).Encode(resp)
	}))
}

func okResult(v any) rpcResult {
	b, _ := json.Marshal(v)
	return rpcResult{Status: "OK", Result: json.RawMessage(b)}
}

func TestNewWithConfig(t *testing.T) {
	c, err := NewWithConfig("test-group", "http://localhost:8000", "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if c.groupID != "test-group" {
		t.Errorf("groupID = %q, want %q", c.groupID, "test-group")
	}
}

func TestEntityRID(t *testing.T) {
	rid1 := entityRID("decision", "Use Go for memory layer")
	rid2 := entityRID("decision", "Use Go for memory layer")
	if rid1 != rid2 {
		t.Error("entityRID not deterministic")
	}
	rid3 := entityRID("artifact", "Use Go for memory layer")
	if rid1 == rid3 {
		t.Error("entityRID not role-sensitive")
	}
}

func TestSanitizeIdentifier(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"depends_on", "depends_on"},
		{"depends-on", "dependson"},
		{"foo bar", "foobar"},
		{"DROP TABLE entity", "DROPTABLEentity"},
		{"", ""},
	}
	for _, c := range cases {
		got := sanitizeIdentifier(c.in)
		if got != c.want {
			t.Errorf("sanitizeIdentifier(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestQueryOK(t *testing.T) {
	srv := mockSurrealServer(t, []rpcResult{okResult([]map[string]any{{"name": "test"}})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	results, err := c.Query("SELECT * FROM entity", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
}

func TestQueryERR(t *testing.T) {
	srv := mockSurrealServer(t, []rpcResult{{Status: "ERR", Result: json.RawMessage(`"statement error"`)}})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	_, err := c.Query("BAD SQL", nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(42), 42},
		{int64(99), 99},
		{int(7), 7},
		{nil, 0},
	}
	for _, c := range cases {
		got := toInt64(c.in)
		if got != c.want {
			t.Errorf("toInt64(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestUpsertEntityViaHTTP(t *testing.T) {
	record := map[string]any{"id": "entity:abc", "role": "decision", "name": "test"}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{record})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	row, err := c.UpsertEntity("decision", "test", "desc", nil, nil, "1.0.0", "test")
	if err != nil {
		t.Fatal(err)
	}
	if row["role"] != "decision" {
		t.Errorf("role = %v, want decision", row["role"])
	}
}

// TestUpsertEntityRecordIDCoercion guards against the SurrealDB 3.x bug where
// a bound string parameter matching 'IDENT:IDENT' is coerced to a record ID
// instead of staying a string. The fix is type::string() casts in the SET
// clause. This test verifies:
//  1. The generated SQL contains type::string($description) (not bare $description).
//  2. UpsertEntity does not error when name/description contain 'TOKEN:word'.
func TestUpsertEntityRecordIDCoercion(t *testing.T) {
	// Capture per-call SQL to inspect the generated SurrealQL.
	// UpsertEntity makes 3 RPC calls:
	//   call 0: precheck SELECT (manual_edit check)
	//   call 1: UPSERT ... RETURN NONE
	//   call 2: FetchEntityByRoleName SELECT
	var capturedCalls []string
	var capturedVars map[string]any

	record := map[string]any{"id": "entity:abc", "role": "log_pattern",
		"name": "ERROR: Engram HTTP fetch", "description": "ERROR: Engram HTTP fetch — connection refused"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var sql string
		if len(req.Params) >= 1 {
			_ = json.Unmarshal(req.Params[0], &sql)
			capturedCalls = append(capturedCalls, sql)
		}
		if len(req.Params) >= 2 && capturedVars == nil {
			_ = json.Unmarshal(req.Params[1], &capturedVars)
		}
		w.Header().Set("Content-Type", "application/json")
		// Return empty for precheck and UPSERT; return record for final SELECT.
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{
			okResult([]any{record}),
		}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	// These values start with 'IDENT:IDENT' — they would be mis-parsed as
	// record IDs by SurrealDB 3.x without the type::string() cast.
	problemName := "ERROR: Engram HTTP fetch — connection refused"
	problemDesc := "ERROR: Engram HTTP fetch — connection refused — indicates ENGRAM_URL points to a stopped service"
	_, err := c.UpsertEntity("log_pattern", problemName, problemDesc, nil, nil, "1.0.0", "test")
	if err != nil {
		t.Fatalf("UpsertEntity with IDENT:IDENT content: %v", err)
	}

	// call 1 is the UPSERT (index 1 after the precheck at index 0).
	if len(capturedCalls) < 2 {
		t.Fatalf("expected ≥2 RPC calls, got %d", len(capturedCalls))
	}
	upsertSQL := capturedCalls[1]

	// Verify the generated SurrealQL uses type::string() to force string coercion.
	if !strings.Contains(upsertSQL, "type::string($description)") {
		t.Errorf("UPSERT SQL missing type::string($description) cast — SurrealDB 3.x coerces 'ERROR:...' as record ID\nSQL: %s", upsertSQL)
	}
	if !strings.Contains(upsertSQL, "type::string($name)") {
		t.Errorf("UPSERT SQL missing type::string($name) cast\nSQL: %s", upsertSQL)
	}

	// Verify values are passed as bound parameters, never interpolated into SQL.
	for _, call := range capturedCalls {
		if strings.Contains(call, problemName) {
			t.Errorf("name value interpolated into SQL — must be a bound parameter\nSQL: %s", call)
		}
		if strings.Contains(call, problemDesc) {
			t.Errorf("description value interpolated into SQL — must be a bound parameter\nSQL: %s", call)
		}
	}
}

// TestUpsertEntityManualEditNone verifies that UpsertEntity succeeds when the
// pre-existing record has manual_edit=NONE (the SurrealDB 3.x coercion bug).
// The fix is `manual_edit=manual_edit??false` in the UPSERT SET clause, which
// coalesces NONE→false without touching an existing true value.
func TestUpsertEntityManualEditNone(t *testing.T) {
	var capturedCalls []string

	// Simulate a pre-existing record whose manual_edit is NONE (JSON null/nil).
	// The precheck SELECT returns manual_edit=nil; the UPSERT must still succeed.
	precheckRow := map[string]any{"manual_edit": nil} // NONE decoded as nil by Go JSON
	entityRow := map[string]any{
		"id": "entity:abc", "role": "artifact", "name": "legacy",
		"manual_edit": false,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Params) >= 1 {
			var sql string
			_ = json.Unmarshal(req.Params[0], &sql)
			capturedCalls = append(capturedCalls, sql)
		}
		w.Header().Set("Content-Type", "application/json")
		// call 0: precheck → row with manual_edit=nil (NONE)
		// call 1: UPSERT  → empty (RETURN NONE)
		// call 2: final SELECT → full entity row
		callIdx := len(capturedCalls) - 1
		var result []any
		switch callIdx {
		case 0:
			result = []any{precheckRow}
		case 2:
			result = []any{entityRow}
		}
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{okResult(result)}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	row, err := c.UpsertEntity("artifact", "legacy", "desc", nil, nil, "1.0.0", "test")
	if err != nil {
		t.Fatalf("UpsertEntity with manual_edit=NONE pre-existing record: %v", err)
	}
	if row == nil {
		t.Fatal("expected non-nil row")
	}

	// Verify the UPSERT SQL (call index 1) contains the NONE coalesce.
	if len(capturedCalls) < 2 {
		t.Fatalf("expected ≥2 RPC calls, got %d", len(capturedCalls))
	}
	upsertSQL := capturedCalls[1]
	if !strings.Contains(upsertSQL, "manual_edit=manual_edit??false") {
		t.Errorf("UPSERT SQL missing manual_edit coalesce — SurrealDB 3.x will reject NONE for TYPE bool\nSQL: %s", upsertSQL)
	}
}

func TestRelateViaHTTP(t *testing.T) {
	record := map[string]any{"id": "depends_on:abc", "confidence": 1.0}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{record})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	row, err := c.Relate("task", "Build pinard", "depends_on", "artifact", "go.mod", 1.0, "depends on Go module", nil)
	if err != nil {
		t.Fatal(err)
	}
	if row["confidence"] != 1.0 {
		t.Errorf("confidence = %v, want 1.0", row["confidence"])
	}
}

func TestRelateInvalidRelation(t *testing.T) {
	c, _ := NewWithConfig("g", "http://unused", "u", "p")
	_, err := c.Relate("task", "foo", "DROP TABLE entity", "artifact", "bar", 1.0, "", nil)
	if err == nil {
		t.Fatal("expected error for relation name that sanitizes to empty")
	}
}

func TestGetSetIngestCursor(t *testing.T) {
	var storedSeq float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// SELECT VALUE returns a JSON array of scalars [42], not [{"seq":42}].
		// Reflect the actual SurrealDB 3.x SELECT VALUE response shape.
		resp := rpcResponse{Result: []rpcResult{
			okResult([]any{storedSeq}),
		}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")

	// SetIngestCursor: mock always OK.
	if err := c.SetIngestCursor("engram_pg", 42); err != nil {
		t.Fatal(err)
	}
	storedSeq = 42

	seq, err := c.GetIngestCursor("engram_pg")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 {
		t.Errorf("seq = %d, want 42", seq)
	}
}

// TestGetSetIngestCursorPerGroup verifies that Set/Get round-trips with the
// per-group key format ("engram_pg:<group>") used by the ingester and status
// handler — guards against the bug where the status handler read "engram_pg"
// while the ingester wrote "engram_pg:<group>", giving cursor=0 always.
func TestGetSetIngestCursorPerGroup(t *testing.T) {
	var storedSeq float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := rpcResponse{Result: []rpcResult{
			okResult([]any{storedSeq}),
		}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c, _ := NewWithConfig("mygroup", srv.URL, "u", "p")

	const key = "engram_pg:mygroup"
	if err := c.SetIngestCursor(key, 77); err != nil {
		t.Fatalf("SetIngestCursor(%q, 77): %v", key, err)
	}
	storedSeq = 77

	seq, err := c.GetIngestCursor(key)
	if err != nil {
		t.Fatalf("GetIngestCursor(%q): %v", key, err)
	}
	if seq != 77 {
		t.Errorf("GetIngestCursor(%q) = %d, want 77", key, seq)
	}
}

func TestGetIngestCursorZeroOnEmpty(t *testing.T) {
	// SELECT VALUE returns [] (empty scalar array) when no record is found.
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	seq, err := c.GetIngestCursor("engram_pg")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0 for missing cursor", seq)
	}
}

// TestCursorRIDDeterministic verifies that cursorRID is deterministic and
// produces distinct values for distinct inputs.
func TestCursorRIDDeterministic(t *testing.T) {
	r1 := cursorRID("engram_pg:misc")
	r2 := cursorRID("engram_pg:misc")
	if r1 != r2 {
		t.Error("cursorRID not deterministic")
	}
	r3 := cursorRID("engram_pg:vignoble-misc")
	if r1 == r3 {
		t.Error("cursorRID collision: engram_pg:misc == engram_pg:vignoble-misc")
	}
	// All values must be safe record-ID characters (hex only).
	for _, rid := range []string{r1, r3} {
		for _, ch := range rid {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
				t.Errorf("cursorRID contains unsafe character %q in %q", ch, rid)
			}
		}
	}
}

// TestCursorRIDHyphenatedRoundTrip is a regression test for the SurrealDB 3.x
// record-ID mangling bug: a source like "engram_pg:vignoble-misc" contains `:` and
// `-` that SurrealDB mis-parses as extra record-ID separators when the raw string is
// used as the record ID suffix.  The fix hashes the source, so the bound $rid
// parameter is a safe hex string and $source retains the original value.
//
// This test verifies:
//  1. SetIngestCursor with a hyphenated+colon source does not error.
//  2. The SurrealQL uses $rid (hash) in the record specifier, not $source.
//  3. The $source parameter carries the original string for the source field.
func TestCursorRIDHyphenatedRoundTrip(t *testing.T) {
	hyph := "engram_pg:vignoble-misc"
	expectedRID := cursorRID(hyph)

	var capturedSQL string
	var capturedVars map[string]any

	var storedSeq float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Params) >= 1 {
			_ = json.Unmarshal(req.Params[0], &capturedSQL)
		}
		if len(req.Params) >= 2 {
			_ = json.Unmarshal(req.Params[1], &capturedVars)
		}
		w.Header().Set("Content-Type", "application/json")
		// GetIngestCursor uses SELECT VALUE → returns scalar array.
		// SetIngestCursor UPSERT → returns record array.
		// Return an appropriate shape based on the SQL verb.
		var result json.RawMessage
		if strings.HasPrefix(capturedSQL, "SELECT VALUE") {
			result, _ = json.Marshal([]any{storedSeq})
		} else {
			result, _ = json.Marshal([]any{map[string]any{"id": "ingest_cursor:" + expectedRID, "source": hyph, "seq": storedSeq}})
		}
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: result}}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")

	// Set cursor for a hyphenated+colon source.
	if err := c.SetIngestCursor(hyph, 9999); err != nil {
		t.Fatalf("SetIngestCursor(%q, 9999): %v", hyph, err)
	}

	// Verify the SQL used $rid (the hash), not $source, in the record specifier.
	// The record specifier is the fragment "type::record('ingest_cursor',...)"
	// — it must use $rid (the safe hex hash), never $source (which contains
	// colons and hyphens that SurrealDB mis-parses as record-ID separators).
	if !strings.Contains(capturedSQL, "$rid") {
		t.Errorf("SetIngestCursor SQL missing $rid in record specifier — wants hash-keyed ID\nSQL: %s", capturedSQL)
	}
	// Extract the text inside type::record('ingest_cursor', ...) and check it
	// does NOT reference $source there.
	if idx := strings.Index(capturedSQL, "type::record"); idx >= 0 {
		close := strings.Index(capturedSQL[idx:], ")")
		if close < 0 {
			close = len(capturedSQL) - idx
		}
		recordCall := capturedSQL[idx : idx+close+1]
		if strings.Contains(recordCall, "$source") {
			t.Errorf("SetIngestCursor uses $source inside type::record() — hyphenated sources will be mangled by SurrealDB\ntype::record call: %s", recordCall)
		}
	}

	// Verify the vars include rid (hash) and source (original string) separately.
	if rid, ok := capturedVars["rid"].(string); !ok || rid != expectedRID {
		t.Errorf("SetIngestCursor vars[rid] = %v, want hash %q", capturedVars["rid"], expectedRID)
	}
	if src, ok := capturedVars["source"].(string); !ok || src != hyph {
		t.Errorf("SetIngestCursor vars[source] = %v, want %q", capturedVars["source"], hyph)
	}

	// Simulate the DB returning the stored seq.
	storedSeq = 9999
	seq, err := c.GetIngestCursor(hyph)
	if err != nil {
		t.Fatalf("GetIngestCursor(%q): %v", hyph, err)
	}
	if seq != 9999 {
		t.Errorf("GetIngestCursor(%q) = %d, want 9999 (round-trip failed — hyphenated source mangled)", hyph, seq)
	}
}

func TestRecallViaHTTP(t *testing.T) {
	entity := map[string]any{"role": "decision", "name": "Use Go", "dist": 0.1}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{entity})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	vec := make([]float64, EmbeddingDim)
	rows, err := c.Recall(vec, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["role"] != "decision" {
		t.Errorf("role = %v, want decision", rows[0]["role"])
	}
}

func TestRecallWithRoleFilter(t *testing.T) {
	entity := map[string]any{"role": "artifact", "name": "go.mod", "dist": 0.05}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{entity})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	vec := make([]float64, EmbeddingDim)
	rows, err := c.Recall(vec, 5, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["role"] != "artifact" {
		t.Errorf("unexpected rows: %v", rows)
	}
}

func TestRecallCosineScan(t *testing.T) {
	entity := map[string]any{"role": "task", "name": "Bootstrap infra", "score": 0.9}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{entity})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	vec := make([]float64, EmbeddingDim)
	rows, err := c.RecallCosineScan(vec, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

func TestLookupViaHTTP(t *testing.T) {
	entity := map[string]any{"role": "decision", "name": "Use JetStream", "score": 0.8}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{entity})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	rows, err := c.Lookup("JetStream", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["name"] != "Use JetStream" {
		t.Errorf("unexpected rows: %v", rows)
	}
}

func TestTraceViaHTTP(t *testing.T) {
	neighbor := map[string]any{"role": "artifact", "name": "go.mod"}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{
		map[string]any{"neighbors": []any{neighbor}},
	})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	rows, err := c.Trace("task", "Build pinard", "depends_on")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

func TestUpsertEntityStagingViaHTTP(t *testing.T) {
	record := map[string]any{"id": "entity_staging:abc", "name": "unknown_thing", "proposed_role": "artifact"}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{record})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	row, err := c.UpsertEntityStaging("unknown_thing", "artifact", "some desc", "obs_type unknown", "engram", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if row["name"] != "unknown_thing" {
		t.Errorf("name = %v, want unknown_thing", row["name"])
	}
}

func TestUpsertEntityStagingWithEmbedding(t *testing.T) {
	record := map[string]any{"id": "entity_staging:xyz", "name": "embedded"}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{record})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	vec := make([]float64, EmbeddingDim)
	row, err := c.UpsertEntityStaging("embedded", "artifact", "desc", "rationale", "src", nil, vec)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("expected non-nil row")
	}
}

func TestEnsureSchemaIdempotent(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		// Return two ok results to satisfy both the NS/DB bootstrap statement
		// (DEFINE NAMESPACE + DEFINE DATABASE) and the schema DDL statements.
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{
			okResult([]any{}),
			okResult([]any{}),
		}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	if err := c.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	callsAfterFirst := calls
	if callsAfterFirst < 2 {
		// First call must make at least 2 HTTP requests:
		// one for NS/DB bootstrap (namespace scope) and one for schema DDL (db scope).
		t.Errorf("EnsureSchema first call made %d HTTP requests, want ≥2", callsAfterFirst)
	}
	// Second call should be idempotent (no additional HTTP requests).
	if err := c.EnsureSchema(); err != nil {
		t.Fatal(err)
	}
	if calls != callsAfterFirst {
		t.Errorf("EnsureSchema second call made %d extra HTTP requests, want 0", calls-callsAfterFirst)
	}
}

func TestIsRetryableConflict(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"Transaction conflict: Write conflict, retry the transaction. This transaction can be retried", true},
		{"Write conflict detected", true},
		{"can be retried", true},
		{"WRITE CONFLICT upper case", true},
		{"CAN BE RETRIED upper case", true},
		{"statement failed: some other error", false},
		{"", false},
		{"timeout", false},
		{"record not found", false},
	}
	for _, tc := range cases {
		got := isRetryableConflict(tc.msg)
		if got != tc.want {
			t.Errorf("isRetryableConflict(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestEnsureSchemaRetryOnConflict(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		var result []rpcResult
		if calls == 1 {
			// First call: return a retryable write conflict on the NS/DB bootstrap.
			result = []rpcResult{{Status: "ERR", Result: json.RawMessage(`"Write conflict, retry the transaction. This transaction can be retried"`)}}
		} else {
			// Subsequent calls: succeed.
			result = []rpcResult{okResult([]any{}), okResult([]any{})}
		}
		json.NewEncoder(w).Encode(rpcResponse{Result: result})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("retry-group", srv.URL, "u", "p")
	if err := c.EnsureSchema(); err != nil {
		t.Fatalf("EnsureSchema failed after retry: %v", err)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 HTTP calls (retry), got %d", calls)
	}
}

// TestEntityNoneToleranceViaOptionString verifies that schema.surql declares
// description, version, and provenance on entity as OVERWRITE option<string>.
// OVERWRITE re-defines the field on existing DBs (IF NOT EXISTS is a no-op when
// the field already exists as TYPE string from an older deploy), so existing
// scopes actually get the type upgrade. option<string> tolerates NONE rows
// without a backfill UPDATE.
func TestEntityNoneToleranceViaOptionString(t *testing.T) {
	sql, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	sqlStr := string(sql)

	// These fields can hold NONE in legacy rows; they must use OVERWRITE +
	// option<string> so DEFINE FIELD actually upgrades the type on existing DBs.
	for _, field := range []string{"description", "version", "provenance"} {
		// Look for the entity-scoped DEFINE FIELD OVERWRITE with option<string>.
		// The line looks like: DEFINE FIELD OVERWRITE <field>  ON entity TYPE option<string>
		pat := "DEFINE FIELD OVERWRITE " + field
		pos := strings.Index(sqlStr, pat)
		if pos < 0 {
			t.Errorf("schema.surql missing DEFINE FIELD OVERWRITE for entity.%s (IF NOT EXISTS is a no-op on existing fields)", field)
			continue
		}
		// Extract the rest of that line and verify it contains option<string>.
		lineEnd := strings.Index(sqlStr[pos:], "\n")
		if lineEnd < 0 {
			lineEnd = len(sqlStr) - pos
		}
		line := sqlStr[pos : pos+lineEnd]
		if !strings.Contains(line, "option<string>") {
			t.Errorf("entity.%s: DEFINE FIELD OVERWRITE must use TYPE option<string> to tolerate legacy NONE rows; got: %s", field, strings.TrimSpace(line))
		}
	}

	// Verify no UPDATE entity SET … backfill remains (would fail on a fresh SCHEMAFULL DB).
	for _, field := range []string{"description", "version", "provenance"} {
		backfillPat := "UPDATE entity SET " + field
		if strings.Contains(sqlStr, backfillPat) {
			t.Errorf("schema.surql still contains backfill UPDATE for entity.%s — remove it; option<string> makes it unnecessary and it breaks fresh-DB deployments", field)
		}
	}
}

// TestCursorSurvivesEnsureSchema is a regression test for the bug where
// schema.surql contained "DELETE ingest_cursor WHERE id != type::record(...)"
// which nuked every cursor row (plain and hyphenated) on each EnsureSchema call,
// resetting seq to 0 for all sources (misc, pinard, vignoble-misc, …).
//
// It verifies that none of the SQL statements sent during EnsureSchema contain
// a DELETE on the ingest_cursor table.
func TestCursorSurvivesEnsureSchema(t *testing.T) {
	var capturedSQL []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Params) >= 1 {
			var sql string
			_ = json.Unmarshal(req.Params[0], &sql)
			capturedSQL = append(capturedSQL, sql)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{
			okResult([]any{}),
			okResult([]any{}),
		}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("cursor-survive", srv.URL, "u", "p")
	if err := c.EnsureSchema(); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	// None of the DDL statements may contain an active (non-comment) DELETE on ingest_cursor.
	for _, sql := range capturedSQL {
		for _, line := range strings.Split(sql, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "--") {
				continue
			}
			if strings.Contains(trimmed, "DELETE ingest_cursor") {
				t.Errorf("EnsureSchema SQL contains active DELETE on ingest_cursor — resets all cursors each cycle\nLine: %s", trimmed)
			}
		}
	}
}

// TestSetIngestCursorIsIdempotent verifies that calling SetIngestCursor twice
// for the same source does not return an error. The UPSERT must be idempotent.
func TestSetIngestCursorIsIdempotent(t *testing.T) {
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{
		map[string]any{"id": "ingest_cursor:engram_pg", "source": "engram_pg", "seq": float64(10)},
	})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	if err := c.SetIngestCursor("engram_pg", 10); err != nil {
		t.Fatalf("first SetIngestCursor: %v", err)
	}
	if err := c.SetIngestCursor("engram_pg", 10); err != nil {
		t.Fatalf("second SetIngestCursor (idempotent): %v", err)
	}
}

// TestUpsertEntityIsIdempotent verifies that calling UpsertEntity twice with the
// same (role, name) does not return an error. Guards against the SurrealDB-3.x
// self-conflict where the UNIQUE index on (role, name) rejected its own record
// on the second UPSERT, silently dropping entity updates.
func TestUpsertEntityIsIdempotent(t *testing.T) {
	record := map[string]any{"id": "entity:abc", "role": "diagnosis", "name": "What:Live"}
	srv := mockSurrealServer(t, []rpcResult{okResult([]any{record})})
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	if _, err := c.UpsertEntity("diagnosis", "What:Live", "desc", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("first UpsertEntity: %v", err)
	}
	if _, err := c.UpsertEntity("diagnosis", "What:Live", "updated desc", nil, nil, "1.0.0", "test"); err != nil {
		t.Fatalf("second UpsertEntity (idempotent): %v", err)
	}
}

// TestSchemaDoesNotContainCursorDeleteByRawSource guards against the
// regression where schema.surql contained:
//
//	DELETE ingest_cursor WHERE id != type::record('ingest_cursor', source)
//
// That expression was designed for the old scheme where the record ID was the
// raw source string. With the hash-based cursorRID scheme the expression never
// matches any hash ID, so it nuked every cursor row on every EnsureSchema call
// — resetting all cursors (including non-hyphenated ones like "misc", "pinard")
// to seq=0 each cycle. Stale-cursor cleanup is now in DeduplicateCursors().
func TestSchemaDoesNotContainCursorDeleteByRawSource(t *testing.T) {
	sqlBytes, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read embedded schema: %v", err)
	}
	// Check each non-comment line for the harmful DELETE pattern.
	for _, line := range strings.Split(string(sqlBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // skip comment lines
		}
		if strings.Contains(trimmed, "DELETE ingest_cursor") {
			t.Errorf("schema.surql has an active DELETE on ingest_cursor which resets all cursors on every EnsureSchema with hash-based record IDs — remove it; cleanup is in DeduplicateCursors()\nLine: %s", trimmed)
		}
	}
}

// ── Wiki curator cursor tests ─────────────────────────────────────────────────

func TestWikiCursorRIDDeterministic(t *testing.T) {
	r1 := wikiCursorRID("misc")
	r2 := wikiCursorRID("misc")
	if r1 != r2 {
		t.Error("wikiCursorRID is not deterministic")
	}
	r3 := wikiCursorRID("other")
	if r1 == r3 {
		t.Error("wikiCursorRID collision between different groupIDs")
	}
}

func TestCamelToSnake(t *testing.T) {
	cases := []struct{ in, want string }{
		{"camelCase", "camel_case"},
		{"CamelCase", "camel_case"},
		{"already_snake", "already_snake"},
		{"MentionedIn", "mentioned_in"},
	}
	for _, tc := range cases {
		got := camelToSnake(tc.in)
		if got != tc.want {
			t.Errorf("camelToSnake(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGetWikiCuratorCursor_NoRecord(t *testing.T) {
	// Empty result → (zero, false, nil).
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: json.RawMessage(`[]`)},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	ts, ok, err := c.GetWikiCuratorCursor("g")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok || !ts.IsZero() {
		t.Errorf("expected (zero, false, nil), got (%v, %v, nil)", ts, ok)
	}
}

func TestGetWikiCuratorCursor_WithRecord(t *testing.T) {
	result := json.RawMessage(`[{"last_synced_at":"2026-08-30T00:00:00Z"}]`)
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: result},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	ts, ok, err := c.GetWikiCuratorCursor("g")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected ok=true")
	}
	if ts.Year() != 2026 {
		t.Errorf("unexpected timestamp year: %d", ts.Year())
	}
}

func TestFindSimilarWikiDoc_Empty(t *testing.T) {
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: json.RawMessage(`[]`)},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	path, score, err := c.FindSimilarWikiDoc("decision", []float64{1, 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" || score != 0 {
		t.Errorf("expected ('', 0), got (%q, %f)", path, score)
	}
}

func TestFindSimilarWikiDoc_HitResult(t *testing.T) {
	result := json.RawMessage(`[{"path":"concepts/foo","score":0.95}]`)
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: result},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	path, score, err := c.FindSimilarWikiDoc("decision", []float64{1, 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "concepts/foo" {
		t.Errorf("path = %q, want %q", path, "concepts/foo")
	}
	if score < 0.94 || score > 0.96 {
		t.Errorf("score = %f, want ~0.95", score)
	}
}

// TestUpsertWikiDoc_SetsTypeField (#265): the top-level `type` field must
// mirror frontmatter["type"] so the role-scoped dedup query
// (FindSimilarWikiDoc) can filter on it without unpacking the FLEXIBLE
// frontmatter object.
func TestUpsertWikiDoc_SetsTypeField(t *testing.T) {
	var capturedVars map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Params) >= 2 {
			_ = json.Unmarshal(req.Params[1], &capturedVars)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[{}]`)}}})
	}))
	defer srv.Close()

	c, _ := NewWithConfig("g", srv.URL, "u", "p")
	_, err := c.UpsertWikiDoc("Some Decision", "body", "decisions/some-decision", "summary", 0.9,
		map[string]any{"type": "decision"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if capturedVars["role"] != "decision" {
		t.Errorf("role var = %v, want %q", capturedVars["role"], "decision")
	}
}

// TestSchemaBackfillsWikiDocType (#265 review — BLOCKING): the pre-MR
// UpsertWikiDoc never set the top-level `type` field, so every doc written
// before this change has type="" with the role only in frontmatter.type. The
// role-scoped query (FindSimilarWikiDoc) filters on `type=$role` and would
// silently match nothing for all of them unless schema.surql backfills it.
func TestSchemaBackfillsWikiDocType(t *testing.T) {
	sqlBytes, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read schema.surql: %v", err)
	}
	sql := string(sqlBytes)
	if !strings.Contains(sql, `UPDATE wiki_doc SET type = frontmatter.type`) {
		t.Error("schema.surql missing the wiki_doc.type backfill from frontmatter.type — role-scoped dedup will match nothing for pre-existing docs")
	}
	if !strings.Contains(sql, `WHERE (type = "" OR type = NONE) AND frontmatter.type != NONE`) {
		t.Error("wiki_doc.type backfill must be gated on empty/NONE type + a real frontmatter.type, so it is idempotent and doesn't clobber already-typed rows")
	}
}

// TestSchemaBackfillEnablesRoleScopedDedupForLegacyDocs (#265 review): an
// end-to-end simulation of the reported failure mode — a legacy wiki_doc row
// with type="" (frontmatter.type="decision") is invisible to
// FindSimilarWikiDoc until EnsureSchema runs the backfill migration;
// afterwards it finds it. The fake server applies the SAME backfill
// condition the real migration uses, so this exercises the actual
// EnsureSchema → role-scoped-query pipeline rather than just asserting the
// SQL text is present.
func TestSchemaBackfillEnablesRoleScopedDedupForLegacyDocs(t *testing.T) {
	legacy := struct {
		path, frontmatterType, typ string
		hasEmbedding               bool
	}{
		path:            "decisions/legacy-decision",
		frontmatterType: "decision", typ: "", hasEmbedding: true,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var sql string
		if len(req.Params) >= 1 {
			_ = json.Unmarshal(req.Params[0], &sql)
		}
		var vars map[string]any
		if len(req.Params) >= 2 {
			_ = json.Unmarshal(req.Params[1], &vars)
		}
		w.Header().Set("Content-Type", "application/json")

		switch {
		// Checked before the generic namespace case: schema.surql's own
		// bootstrap line ("DEFINE NAMESPACE IF NOT EXISTS pinard;", no
		// backticks) also matches "DEFINE NAMESPACE", so the more specific
		// backfill statement must be tried first or the big schema-apply
		// request would be misrouted to the namespace-bootstrap branch.
		case strings.Contains(sql, "UPDATE wiki_doc SET type = frontmatter.type"):
			// Apply the exact backfill condition the real migration uses.
			if legacy.typ == "" && legacy.frontmatterType != "" {
				legacy.typ = legacy.frontmatterType
			}
			json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[]`)}}})
		case strings.Contains(sql, "DEFINE NAMESPACE"):
			json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[{}]`)}}})
		case strings.Contains(sql, "vector::similarity::cosine"):
			var rows []map[string]any
			if role, _ := vars["role"].(string); role != "" && role == legacy.typ && legacy.hasEmbedding {
				rows = append(rows, map[string]any{"path": legacy.path, "score": 0.95})
			}
			b, _ := json.Marshal(rows)
			json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(b)}}})
		default:
			json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[]`)}}})
		}
	}))
	defer srv.Close()

	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}

	// Before the backfill runs, the legacy row's top-level type is still "" so
	// the role-scoped signal must find nothing (the exact bug the review
	// flagged: dedup silently goes blind on every pre-existing doc).
	if path, _, _ := c.FindSimilarWikiDoc("decision", []float64{1, 0}); path != "" {
		t.Fatalf("FindSimilarWikiDoc before EnsureSchema = %q, want \"\" (type not yet backfilled)", path)
	}

	if err := c.EnsureSchema(); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	if path, _, err := c.FindSimilarWikiDoc("decision", []float64{1, 0}); err != nil {
		t.Fatalf("FindSimilarWikiDoc: %v", err)
	} else if path != legacy.path {
		t.Errorf("FindSimilarWikiDoc after EnsureSchema = %q, want %q", path, legacy.path)
	}
}

// TestSchemaDropsWikiCuratorCursorIndex (#265): the wiki_curator_cursor record id
// is a deterministic hash, so a UNIQUE index on `source` is redundant and
// self-conflicts on UPSERT ("already contains 'wiki_curator:<gid>'"), stalling the
// cursor. schema.surql must drop it so EnsureSchema self-heals live DBs.
func TestSchemaDropsWikiCuratorCursorIndex(t *testing.T) {
	sqlBytes, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read schema.surql: %v", err)
	}
	if !strings.Contains(string(sqlBytes), "REMOVE INDEX IF EXISTS wiki_curator_cursor_source ON wiki_curator_cursor") {
		t.Error("schema.surql missing REMOVE INDEX for wiki_curator_cursor_source — curator cursor will self-conflict and never advance")
	}
}

// TestSchemaWikiDocTitleNotUnique (#265): the wiki_doc_title index must be a
// plain lookup index, never UNIQUE — a UNIQUE title self-conflicts when the
// curator re-titles a doc. schema.surql must OVERWRITE it (to fix live DBs that
// still carry the old UNIQUE definition, which DEFINE ... IF NOT EXISTS skips).
func TestSchemaWikiDocTitleNotUnique(t *testing.T) {
	sqlBytes, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read schema.surql: %v", err)
	}
	sql := string(sqlBytes)
	if !strings.Contains(sql, "DEFINE INDEX OVERWRITE wiki_doc_title ON wiki_doc FIELDS title") {
		t.Error("schema.surql must OVERWRITE wiki_doc_title as a non-unique index to clear the legacy UNIQUE constraint")
	}
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue // skip comments
		}
		if strings.HasPrefix(trimmed, "DEFINE INDEX") && strings.Contains(trimmed, "wiki_doc_title ") && strings.Contains(trimmed, "UNIQUE") {
			t.Errorf("wiki_doc_title must not be UNIQUE: %s", trimmed)
		}
	}
}

// ── wiki_mentions: stable concept identity (#320) ─────────────────────────────

func TestEntityIDPart(t *testing.T) {
	if got := entityIDPart("entity:abc123"); got != "abc123" {
		t.Errorf("entityIDPart(entity:abc123) = %q, want %q", got, "abc123")
	}
	if got := entityIDPart("abc123"); got != "abc123" {
		t.Errorf("entityIDPart(abc123) = %q, want %q (no prefix to strip)", got, "abc123")
	}
}

func TestPluralityPath_Empty(t *testing.T) {
	if got := pluralityPath(map[string]int{}); got != "" {
		t.Errorf("pluralityPath(empty) = %q, want \"\"", got)
	}
}

func TestPluralityPath_ClearWinner(t *testing.T) {
	counts := map[string]int{"decisions/a": 1, "decisions/b": 3, "decisions/c": 2}
	if got := pluralityPath(counts); got != "decisions/b" {
		t.Errorf("pluralityPath = %q, want %q", got, "decisions/b")
	}
}

func TestPluralityPath_TieBreaksLexicographically(t *testing.T) {
	counts := map[string]int{"decisions/z": 2, "decisions/a": 2}
	if got := pluralityPath(counts); got != "decisions/a" {
		t.Errorf("pluralityPath tie-break = %q, want %q", got, "decisions/a")
	}
}

func TestFindWikiDocByEntities_NoEntityIDs(t *testing.T) {
	// Short-circuits without contacting the server.
	c, err := NewWithConfig("g", "http://unused.invalid", "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.FindWikiDocByEntities(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want \"\"", path)
	}
}

func TestFindWikiDocByEntities_NoMatches(t *testing.T) {
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: json.RawMessage(`[]`)},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.FindWikiDocByEntities([]string{"entity:abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want \"\" (no edges yet)", path)
	}
}

// TestFindWikiDocByEntities_PluralityWins (#320): when a cluster's entities
// have accumulated edges to two different existing docs (clusters merge over
// time), the doc covering the plurality of the cluster's entities wins.
func TestFindWikiDocByEntities_PluralityWins(t *testing.T) {
	result := json.RawMessage(`[{"path":"decisions/foo"},{"path":"decisions/foo"},{"path":"decisions/bar"}]`)
	srv := mockSurrealServer(t, []rpcResult{
		{Status: "OK", Result: result},
	})
	defer srv.Close()
	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	path, err := c.FindWikiDocByEntities([]string{"entity:a", "entity:b", "entity:c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "decisions/foo" {
		t.Errorf("path = %q, want %q", path, "decisions/foo")
	}
}

// TestWriteWikiMentions_DeletesThenRelatesPerEntity (#320): WriteWikiMentions
// must clear the doc's existing outbound edges before writing the current set,
// so entities dropped from a cluster (entity deletion) are unlinked rather
// than accumulating stale edges forever.
func TestWriteWikiMentions_DeletesThenRelatesPerEntity(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		bodyBytes, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(bodyBytes))
		_ = json.Unmarshal(bodyBytes, &req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rpcResponse{Result: []rpcResult{{Status: "OK", Result: json.RawMessage(`[{}]`)}}})
	}))
	defer srv.Close()

	c, err := NewWithConfig("g", srv.URL, "root", "pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteWikiMentions("decisions/foo", []string{"entity:a", "entity:b"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One DELETE followed by one RELATE per entity.
	if len(bodies) != 3 {
		t.Fatalf("got %d requests, want 3 (1 DELETE + 2 RELATE)", len(bodies))
	}
	if !strings.Contains(bodies[0], "DELETE wiki_mentions") {
		t.Errorf("first request should be the DELETE, got: %s", bodies[0])
	}
	for _, b := range bodies[1:] {
		if !strings.Contains(b, "RELATE") || !strings.Contains(b, "wiki_mentions") {
			t.Errorf("expected a RELATE ... wiki_mentions request, got: %s", b)
		}
	}
}

// TestSchemaDefinesEveryFieldUpsertWikiDocWrites guards the class of bug that
// turned master red after !611: wiki_doc is SCHEMAFULL, so any field written by
// UpsertWikiDoc that is not DEFINEd is rejected on a freshly created database
// with "Found field 'X', but no such field exists for table 'wiki_doc'".
//
// It stayed latent because DEFINE TABLE ... IF NOT EXISTS never converts an
// already-existing table, so long-lived DBs kept their permissive (pre-
// SCHEMAFULL) wiki_doc and accepted the write. Only brand-new scope databases —
// CI, and any newly onboarded vigne — hit it.
func TestSchemaDefinesEveryFieldUpsertWikiDocWrites(t *testing.T) {
	sqlBytes, err := schemaFS.ReadFile("schema.surql")
	if err != nil {
		t.Fatalf("read schema.surql: %v", err)
	}
	sql := string(sqlBytes)

	defined := map[string]bool{}
	for _, line := range strings.Split(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || !strings.Contains(trimmed, "ON wiki_doc") {
			continue
		}
		f := strings.Fields(trimmed)
		// DEFINE FIELD IF NOT EXISTS <name> ON wiki_doc ...
		for i, tok := range f {
			if tok == "ON" && i > 0 {
				defined[f[i-1]] = true
			}
		}
	}

	// Every field UpsertWikiDoc assigns must be DEFINEd on the table.
	for _, field := range []string{
		"title", "type", "body", "summary", "path",
		"status", "confidence", "frontmatter", "embedding", "updated_at",
	} {
		if !defined[field] {
			t.Errorf("wiki_doc.%s is written by UpsertWikiDoc but not DEFINEd in schema.surql — "+
				"a freshly created SCHEMAFULL database will reject the write", field)
		}
	}
}
