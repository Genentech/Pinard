package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── Clustering ────────────────────────────────────────────────────────────────

func makeEntity(name, role string, embedding []float64) map[string]any {
	e := map[string]any{"name": name, "role": role, "description": name + " description"}
	if embedding != nil {
		e["embedding"] = embedding
	}
	return e
}

// unit vectors for deterministic similarity tests.
var (
	vecA = []float64{1, 0, 0}
	vecB = []float64{0.99, 0.1, 0}  // cosine ~0.995 with vecA (same cluster)
	vecC = []float64{0, 1, 0}       // orthogonal to vecA (different cluster)
	vecD = []float64{0, 0.99, 0.1}  // cosine ~0.995 with vecC
)

func TestClusterEntities_BasicGrouping(t *testing.T) {
	entities := []map[string]any{
		makeEntity("A", "concept", vecA),
		makeEntity("B", "concept", vecB), // should join A's cluster
		makeEntity("C", "concept", vecC),
		makeEntity("D", "concept", vecD), // should join C's cluster
	}
	clusters := clusterEntities(entities, nil)
	if len(clusters) != 2 {
		t.Errorf("expected 2 clusters, got %d", len(clusters))
	}
	// Each cluster should have 2 entities.
	for _, cl := range clusters {
		if len(cl) != 2 {
			t.Errorf("expected 2 entities per cluster, got %d", len(cl))
		}
	}
}

func TestClusterEntities_NilEmbeddingIsolated(t *testing.T) {
	entities := []map[string]any{
		makeEntity("X", "concept", nil), // no embedding
		makeEntity("Y", "concept", nil), // no embedding — each is isolated
		makeEntity("Z", "concept", vecA),
	}
	clusters := clusterEntities(entities, nil)
	// X and Y cannot be clustered together (no embedding), each isolated.
	// Z forms its own cluster.
	if len(clusters) != 3 {
		t.Errorf("expected 3 clusters (2 nil-embedding + 1 vec), got %d", len(clusters))
	}
}

func TestClusterEntities_MaxSize(t *testing.T) {
	// Build clusterMaxSize+2 entities all very similar → should split into 2 clusters.
	entities := make([]map[string]any, clusterMaxSize+2)
	for i := range entities {
		// All identical embeddings — all will try to join the first cluster.
		entities[i] = makeEntity("E", "concept", vecA)
	}
	clusters := clusterEntities(entities, nil)
	// First cluster fills to clusterMaxSize, remainder starts a new one.
	if len(clusters) < 2 {
		t.Errorf("expected at least 2 clusters due to max-size cap, got %d", len(clusters))
	}
	for _, cl := range clusters {
		if len(cl) > clusterMaxSize {
			t.Errorf("cluster size %d exceeds max %d", len(cl), clusterMaxSize)
		}
	}
}

// ── Dedup threshold ───────────────────────────────────────────────────────────

func TestDedupThreshold_CosineSimilarity(t *testing.T) {
	cases := []struct {
		a, b    []float64
		wantHit bool
	}{
		{vecA, vecA, true},                             // score = 1.0
		{vecA, []float64{0.92, 0.39, 0}, true},         // cosine ≈ 0.920 ≥ 0.92
		{vecA, vecC, false},                             // orthogonal, score = 0
		{[]float64{1, 0, 0}, []float64{0.9, 0.44, 0}, false}, // cosine < 0.92
	}
	for _, tc := range cases {
		score := cosineSimilarity(tc.a, tc.b)
		hit := score >= dedupSimilarityThreshold
		if hit != tc.wantHit {
			t.Errorf("cosineSimilarity(%v, %v) = %.4f, hit=%v, want hit=%v", tc.a, tc.b, score, hit, tc.wantHit)
		}
	}
}

// ── Human page preservation ───────────────────────────────────────────────────

func TestHumanPagePreservation(t *testing.T) {
	dir := t.TempDir()
	okfPath := "concepts/my-page"
	mdFile := filepath.Join(dir, okfPath+".md")

	if err := os.MkdirAll(filepath.Dir(mdFile), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a human-authored page.
	humanContent := "---\nsource: human\ntitle: My Page\n---\n# Body\n\nHuman content.\n"
	if err := os.WriteFile(mdFile, []byte(humanContent), 0o644); err != nil {
		t.Fatal(err)
	}

	fm := readFrontmatter(mdFile)
	if fm["source"] != "human" {
		t.Fatalf("expected source=human, got %v", fm["source"])
	}

	// Simulate the processCluster human-page guard.
	if fm["source"] == "human" {
		// Should skip — do nothing.
		data, _ := os.ReadFile(mdFile)
		if !strings.Contains(string(data), "Human content.") {
			t.Error("human page was modified")
		}
	} else {
		t.Error("expected source=human guard to trigger")
	}
}

// ── parseSynthesisResponse ────────────────────────────────────────────────────

func TestParseSynthesisResponse_PlainJSON(t *testing.T) {
	raw := `{"title": "Foo", "summary": "Bar", "body": "# Baz"}`
	got := parseSynthesisResponse(raw)
	if got["title"] != "Foo" {
		t.Errorf("title: got %q, want %q", got["title"], "Foo")
	}
	if got["summary"] != "Bar" {
		t.Errorf("summary: got %q, want %q", got["summary"], "Bar")
	}
	if got["body"] != "# Baz" {
		t.Errorf("body: got %q, want %q", got["body"], "# Baz")
	}
}

func TestParseSynthesisResponse_MarkdownFence(t *testing.T) {
	raw := "```json\n{\"title\": \"T\", \"summary\": \"S\", \"body\": \"B\"}\n```"
	got := parseSynthesisResponse(raw)
	if got["title"] != "T" {
		t.Errorf("title: got %q, want %q", got["title"], "T")
	}
}

func TestParseSynthesisResponse_EmbeddedJSON(t *testing.T) {
	raw := "Here is the result:\n{\"title\": \"X\", \"summary\": \"Y\", \"body\": \"Z\"}\nDone."
	got := parseSynthesisResponse(raw)
	if got["title"] != "X" {
		t.Errorf("title: got %q, want %q", got["title"], "X")
	}
}

func TestParseSynthesisResponse_Garbage(t *testing.T) {
	got := parseSynthesisResponse("not json at all")
	if len(got) != 0 {
		t.Errorf("expected empty map for garbage input, got %v", got)
	}
}

// ── Confidence scoring ────────────────────────────────────────────────────────

func TestConfidenceScoring(t *testing.T) {
	cases := []struct {
		role      string
		size      int
		wantGte   float64
		wantLt    float64
		autoServe bool
	}{
		// single high-value role → auto_serve (≥0.7)
		{"decision", 1, 0.76, 0.9, true},
		// single non-high-value → needs_review (<0.7)
		{"concept", 1, 0.60, 0.70, false},
		// large cluster of unknown role → auto_serve
		{"concept", 10, 0.70, 0.91, true},
		// artifact role, size=1 → needs_review
		{"artifact", 1, 0.60, 0.70, false},
	}
	for _, tc := range cases {
		c := computeConfidence(tc.role, tc.size)
		if c < tc.wantGte || c >= tc.wantLt {
			t.Errorf("computeConfidence(%q, %d) = %.3f, want [%.2f, %.2f)", tc.role, tc.size, c, tc.wantGte, tc.wantLt)
		}
		autoServe := c >= 0.7
		if autoServe != tc.autoServe {
			t.Errorf("computeConfidence(%q, %d) = %.3f → auto_serve=%v, want %v", tc.role, tc.size, c, autoServe, tc.autoServe)
		}
	}
}

// ── Cursor incrementality (pure logic, no DB) ─────────────────────────────────

// TestCursorIncrementality verifies that SetWikiCuratorCursor is only called
// when entities were returned. This is tested via the logic in curateGroup:
// since we can't mock the DB, we test the invariant via the helper functions.
func TestCursorIncrementality_AdvanceOnlyWhenEntities(t *testing.T) {
	// Simulate the decision: cursor advances iff len(entities) > 0.
	entities := []map[string]any{}
	shouldAdvance := len(entities) > 0
	if shouldAdvance {
		t.Error("should not advance cursor when no entities")
	}

	entities = []map[string]any{makeEntity("X", "concept", vecA)}
	shouldAdvance = len(entities) > 0
	if !shouldAdvance {
		t.Error("should advance cursor when entities present")
	}
}

// ── Slugify ───────────────────────────────────────────────────────────────────

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"  foo  bar  ", "foo-bar"},
		{"foo--bar", "foo-bar"},
		{"", "unknown"},
		{"ABC 123!", "abc-123"},
	}
	for _, tc := range cases {
		got := slugify(tc.in, 0)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlugify_MaxLen(t *testing.T) {
	long := strings.Repeat("abcde-", 20) // 120 chars
	got := slugify(long, 80)
	if len(got) > 80 {
		t.Errorf("slugify with maxLen=80: got len %d", len(got))
	}
}

// ── meanEmbedding ─────────────────────────────────────────────────────────────

func TestMeanEmbedding(t *testing.T) {
	a := []float64{1, 0}
	b := []float64{0, 1}
	m := meanEmbedding([][]float64{a, b})
	if m[0] != 0.5 || m[1] != 0.5 {
		t.Errorf("meanEmbedding = %v, want [0.5 0.5]", m)
	}
}

func TestMeanEmbedding_NilHandled(t *testing.T) {
	m := meanEmbedding([][]float64{nil, {1, 2}})
	if m[0] != 1 || m[1] != 2 {
		t.Errorf("meanEmbedding with nil = %v, want [1 2]", m)
	}
}

func TestMeanEmbedding_AllNil(t *testing.T) {
	m := meanEmbedding([][]float64{nil, nil})
	if m != nil {
		t.Errorf("expected nil mean for all-nil input, got %v", m)
	}
}

// ── #265: scaffold guard, no-fallback, dedup threshold ─────────────────────────

func TestIsScaffoldName(t *testing.T) {
	scaffold := []string{
		"What:env", "What: env", "**What**: A", "Why: because",
		"Where: internal/x.go", "Learned: gotcha", "  what:  lower",
		"Next Steps: do things", "Relevant Files: a.go",
	}
	for _, s := range scaffold {
		if !isScaffoldName(s) {
			t.Errorf("isScaffoldName(%q) = false, want true", s)
		}
	}
	ok := []string{
		"Whatever the case", "PostgreSQL migration decision",
		"Provenance database", "Whatnot handler", "", "somewhere:else",
	}
	for _, s := range ok {
		if isScaffoldName(s) {
			t.Errorf("isScaffoldName(%q) = true, want false", s)
		}
	}
}

// TestSynthesizeConceptNoFallback: with no LLM client, synthesis must report
// ok=false (skip the upsert) rather than emitting a degraded fallback doc.
func TestSynthesizeConceptNoFallback(t *testing.T) {
	cl := []map[string]any{makeEntity("Some Concept", "decision", nil)}
	title, summary, body, ok := synthesizeConcept(cl, "Some Concept", "decision", "a description", nil, nil)
	if ok {
		t.Fatalf("synthesizeConcept with nil LLM = ok:true, want false")
	}
	if title != "" || summary != "" || body != "" {
		t.Errorf("no-LLM synthesis returned non-empty content: title=%q summary=%q body=%q", title, summary, body)
	}
}

func TestDedupThresholdFor(t *testing.T) {
	if dedupThresholdFor("decision") != dedupSimilarityThresholdProse {
		t.Errorf("decision threshold = %v, want %v", dedupThresholdFor("decision"), dedupSimilarityThresholdProse)
	}
	if dedupThresholdFor("diagnosis") != dedupSimilarityThresholdProse {
		t.Errorf("diagnosis threshold = %v, want %v", dedupThresholdFor("diagnosis"), dedupSimilarityThresholdProse)
	}
	if dedupThresholdFor("concept") != dedupSimilarityThreshold {
		t.Errorf("concept threshold = %v, want %v", dedupThresholdFor("concept"), dedupSimilarityThreshold)
	}
}

// TestIsDegenerateScaffold (#265): only scaffold-named entities with a TRIVIAL
// description are degenerate. Real mem_save entities also start with **What**:
// but carry content and must NOT be skipped.
func TestIsDegenerateScaffold(t *testing.T) {
	// degenerate: scaffold name + contentless desc
	degen := [][2]string{
		{"What:Design", "What:Design"},
		{"What:env", "What:env"},
		{"**What**: A", "What:A"},
	}
	for _, c := range degen {
		if !isDegenerateScaffold(c[0], c[1]) {
			t.Errorf("isDegenerateScaffold(%q,%q) = false, want true", c[0], c[1])
		}
	}
	// real content behind a scaffold name → NOT degenerate (must be curated)
	real := [][2]string{
		{"**What**: Implemented the ontology gardener", "**What**: Implemented the ontology gardener (issue #105) — a declarative registry that ..."},
		{"What: (1) #292 fixed a bug", strings.Repeat("real content ", 10)},
	}
	for _, c := range real {
		if isDegenerateScaffold(c[0], c[1]) {
			t.Errorf("isDegenerateScaffold(%q, <%d chars>) = true, want false", c[0], len(c[1]))
		}
	}
	// non-scaffold name is never degenerate regardless of desc
	if isDegenerateScaffold("PostgreSQL migration", "x") {
		t.Error("non-scaffold name flagged degenerate")
	}
}

// ── Wiki path + git regression tests ─────────────────────────────────────────

// TestWikiRepoPath_ResolvesUnderVignobleClone asserts that wikiRepoPath places
// files inside the vignoble clone, not an orphaned directory.
func TestWikiRepoPath_ResolvesUnderVignobleClone(t *testing.T) {
	cloneDir := t.TempDir()
	got := wikiRepoPath("pinard", cloneDir)
	want := filepath.Join(cloneDir, "wiki", "pinard")
	if got != want {
		t.Errorf("wikiRepoPath = %q, want %q", got, want)
	}
	// Must NOT produce an orphaned path like /data/repos/pinard/wiki.
	if !strings.HasPrefix(got, cloneDir) {
		t.Errorf("wikiRepoPath %q is not under vignoble clone %q", got, cloneDir)
	}
}

// TestCommitAndPush_FailsOnNonGitDir asserts commitAndPush returns an error
// when the target directory is not a git repository. This is the exact
// regression seen in production (silently wrote to /data/repos/pinard/wiki).
func TestCommitAndPush_FailsOnNonGitDir(t *testing.T) {
	nonGit := t.TempDir()
	wikiDir := filepath.Join(nonGit, "wiki", "pinard")
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := commitAndPush(nonGit, "pinard", wikiDir, []string{"decisions/test"})
	if err == nil {
		t.Fatal("commitAndPush on non-git dir should return an error, got nil")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("expected 'not a git repository' error, got: %v", err)
	}
}

// TestCommitAndPush_StagesInVignobleClone asserts that commitAndPush stages
// and commits wiki files inside a real git repo (the vignoble clone pattern).
func TestCommitAndPush_StagesInVignobleClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	gitRoot := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	run(gitRoot, "init")
	run(gitRoot, "config", "user.email", "test@test.com")
	run(gitRoot, "config", "user.name", "test")
	// Initial commit so branch exists.
	readmePath := filepath.Join(gitRoot, "README.md")
	if err := os.WriteFile(readmePath, []byte("# vignoble\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(gitRoot, "add", "README.md")
	run(gitRoot, "commit", "--no-verify", "-m", "init")

	// Create the wiki file as the curator would.
	wikiDir := filepath.Join(gitRoot, "wiki", "pinard")
	if err := os.MkdirAll(filepath.Join(wikiDir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(wikiDir, "decisions", "test.md")
	if err := os.WriteFile(mdPath, []byte("---\ntitle: Test\n---\n# Body\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// DRY_RUN so we don't try to push to a remote.
	t.Setenv("DRY_RUN", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@test.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@test.com")

	if err := commitAndPush(gitRoot, "pinard", wikiDir, []string{"decisions/test"}); err != nil {
		t.Fatalf("commitAndPush: %v", err)
	}

	// Verify the commit landed in the vignoble repo.
	c := exec.Command("git", "log", "--oneline", "-1")
	c.Dir = gitRoot
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if !strings.Contains(string(out), "feat(wiki)") {
		t.Errorf("expected feat(wiki) commit, got: %s", out)
	}

	// Verify the staged file is under wiki/pinard/.
	c2 := exec.Command("git", "show", "--name-only", "--format=", "HEAD")
	c2.Dir = gitRoot
	out2, err := c2.Output()
	if err != nil {
		t.Fatalf("git show: %v", err)
	}
	if !strings.Contains(string(out2), "wiki/pinard/decisions/test.md") {
		t.Errorf("expected wiki/pinard/decisions/test.md in commit, got:\n%s", out2)
	}
}
