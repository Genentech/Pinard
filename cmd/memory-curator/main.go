// Command memory-curator is the Go port of services/memory/wiki/curator.py.
//
// For each vigne (group_id) discovered from vignes.yaml, it reads the
// ontology-typed SurrealDB entity graph, clusters related entities by embedding
// similarity, synthesizes OKF wiki pages via the LLM, and commits them to the
// per-vigne wiki git repo as a branch + MR.
//
// Incremental via wiki_curator_cursor: only (re)synthesizes clusters whose
// entities changed since the last cursor. Human-authored pages (frontmatter
// source == "human") are never overwritten.
//
// Environment variables:
//
//	SURREAL_URL           — SurrealDB endpoint
//	SURREAL_USER          — SurrealDB username
//	SURREAL_PASS          — SurrealDB password
//	ROSETTA_URL           — Rosetta embedding endpoint
//	MEMORY_LLM_*          — LLM client config
//	VIGNOBLE_DIR          — vignoble root (vignes.yaml location)
//	VIGNOBLES_BASE_DIR    — parent dir of multiple vignoble clones
//	CURATOR_INTERVAL      — run interval (e.g. 2h, 30m); unset/0 = one-shot
//	WIKI_CLONE_DIR        — base dir for per-group wiki repos (default: /data/repos)
//	GITLAB_TOKEN          — optional GitLab API token for REST MR fallback
//	GITLAB_HOST           — optional GitLab host for REST MR (default: derived from remote)
//	DRY_RUN               — if non-empty, skip git push and MR creation
//	GOOGLE_APPLICATION_CREDENTIALS — path to Google SA JSON for LLM auth
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Genentech/pinard/internal/memory"
	"github.com/Genentech/pinard/internal/ontology"
	"github.com/Genentech/pinard/internal/surreal"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	clusterSimilarityThreshold = 0.75
	clusterMaxSize             = 12
	dedupSimilarityThreshold   = 0.92
	// Prose-heavy roles (decisions/diagnoses) whose semantically-identical pages
	// can embed just below the default threshold get a slightly lower dedup bar.
	dedupSimilarityThresholdProse = 0.88
)

// scaffoldNameRE matches a mem_save structured-body key (What:/Why:/Where:/
// Learned:/…) that leaked into an entity name. Concepts named like this are
// degenerate and must never be curated into wiki pages.
var scaffoldNameRE = regexp.MustCompile(`(?i)^\**\s*(what|why|where|learned|goal|instructions|discoveries|accomplished|next steps|relevant files)\**\s*:`)

func isScaffoldName(s string) bool {
	return scaffoldNameRE.MatchString(strings.TrimSpace(s))
}

// degenerateDescMaxLen: below this, a scaffold-named entity is treated as
// contentless junk. The real data is bimodal with a wide empty gap — degenerate
// scaffold entities have desc<20 (name==desc, e.g. "What:Design") while genuine
// mem_save entities have desc>80 — so 40 cleanly separates them with margin.
const degenerateDescMaxLen = 40

// isDegenerateScaffold reports a scaffold-named entity with NO real content.
// Every mem_save observation starts with **What**:, so ~half of all entities are
// scaffold-named — the vast majority carry real knowledge and MUST be curated
// (the LLM cleans their titles). Only skip when the description is also trivial.
func isDegenerateScaffold(name, desc string) bool {
	return isScaffoldName(name) && len(strings.TrimSpace(desc)) < degenerateDescMaxLen
}

func dedupThresholdFor(role string) float64 {
	if role == "decision" || role == "diagnosis" {
		return dedupSimilarityThresholdProse
	}
	return dedupSimilarityThreshold
}

var reservedNames = map[string]bool{"index.md": true, "log.md": true}

var highValueRoles = map[string]bool{
	"decision": true, "diagnosis": true, "verdict": true, "log_pattern": true,
}

func main() {
	root := &cobra.Command{
		Use:   "memory-curator",
		Short: "Pinard per-vigne wiki curator",
		RunE:  run,
	}
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	emb := memory.NewEmbedder()
	llm, llmErr := memory.BuildLLMClient()
	if llmErr != nil {
		log.Printf("WARN: LLM client unavailable: %v — wiki synthesis disabled", llmErr)
		llm = nil
	}

	var interval time.Duration
	if s := os.Getenv("CURATOR_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			interval = d
		}
	}

	for {
		groups, err := discoverGroups()
		if err != nil {
			log.Printf("ERROR: discover groups: %v", err)
		} else {
			for _, g := range groups {
				if err := curateGroup(g.id, g.vignobleCloneDir, emb, llm); err != nil {
					log.Printf("ERROR: curate %s: %v", g.id, err)
				}
			}
		}
		if interval == 0 {
			return nil
		}
		log.Printf("INFO: next curator run in %s", interval)
		time.Sleep(interval)
	}
}

// ── Group discovery ───────────────────────────────────────────────────────────

type vignesYAML struct {
	Vignes map[string]struct{} `yaml:"vignes"`
}

// groupInfo pairs a group/vigne ID with the vignoble clone root that owns it.
// wikiDir = vignobleCloneDir/wiki/<id> is where files are written.
// Git operations run at vignobleCloneDir (the actual git repo root).
type groupInfo struct {
	id               string
	vignobleCloneDir string
}

func loadGroupIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg vignesYAML
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cfg.Vignes))
	for name := range cfg.Vignes {
		ids = append(ids, name)
	}
	sort.Strings(ids)
	return ids, nil
}

// discoverGroups returns all (groupID, vignobleCloneDir) pairs.
// When VIGNOBLES_BASE_DIR is set each subdirectory is a vignoble clone; groups
// are derived from that clone's vignes.yaml and their vignobleCloneDir is the
// clone root. When only VIGNOBLE_DIR (or VIGNOBLE_YAML) is set there is a
// single vignoble whose clone root IS that directory.
func discoverGroups() ([]groupInfo, error) {
	if baseDir := os.Getenv("VIGNOBLES_BASE_DIR"); baseDir != "" {
		entries, err := os.ReadDir(baseDir)
		if err != nil {
			return nil, fmt.Errorf("read VIGNOBLES_BASE_DIR %s: %w", baseDir, err)
		}
		var groups []groupInfo
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			cloneDir := filepath.Join(baseDir, e.Name())
			vigPath := filepath.Join(cloneDir, "vignes.yaml")
			if _, err := os.Stat(vigPath); os.IsNotExist(err) {
				continue
			}
			gids, err := loadGroupIDs(vigPath)
			if err != nil {
				log.Printf("WARN: load %s: %v", vigPath, err)
				continue
			}
			for _, gid := range gids {
				groups = append(groups, groupInfo{id: gid, vignobleCloneDir: cloneDir})
			}
		}
		return groups, nil
	}
	vigPath := os.Getenv("VIGNOBLE_YAML")
	if vigPath == "" {
		vigDir := envOr("VIGNOBLE_DIR", ".")
		vigPath = filepath.Join(vigDir, "vignes.yaml")
	}
	cloneDir := filepath.Dir(vigPath)
	gids, err := loadGroupIDs(vigPath)
	if err != nil {
		return nil, err
	}
	groups := make([]groupInfo, len(gids))
	for i, gid := range gids {
		groups[i] = groupInfo{id: gid, vignobleCloneDir: cloneDir}
	}
	return groups, nil
}

// ── Main curation pipeline ────────────────────────────────────────────────────

func curateGroup(groupID, vignobleCloneDir string, emb *memory.Embedder, llm *memory.LLMClient) error {
	db, err := surreal.New(groupID)
	if err != nil {
		return fmt.Errorf("connect %s: %w", groupID, err)
	}
	defer db.Close()

	// wikiDir is where markdown files are written; git operations run at vignobleCloneDir.
	repoPath := wikiRepoPath(groupID, vignobleCloneDir)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		return fmt.Errorf("create wiki dir %s: %w", repoPath, err)
	}

	// Phase 1: Incremental LLM synthesis for changed entities.
	since, hasCursor, err := db.GetWikiCuratorCursor(groupID)
	if err != nil {
		log.Printf("WARN: read cursor %s: %v", groupID, err)
	}

	var sincePt *time.Time
	if hasCursor {
		sincePt = &since
	}

	entities, err := db.FetchEntitiesSinceCursor(sincePt)
	if err != nil {
		return fmt.Errorf("fetch entities %s: %w", groupID, err)
	}

	var newlyWritten []string
	// synthesisBlocked = the chat-LLM was unavailable for at least one cluster (or
	// entirely). We then do NOT advance the cursor, so those entities are retried
	// next cycle once the LLM is back. This is independent of the embeddings
	// (Rosetta) service — dedup degrades gracefully without blocking synthesis.
	synthesisBlocked := false

	if llm == nil {
		// Chat-LLM client unavailable → the synthesis step is a clean no-op. Never
		// emit placeholder/fallback docs; leave the cursor untouched so a later run
		// (LLM back) synthesizes these entities. Snapshot below still re-emits any
		// existing good docs (needs neither service).
		if len(entities) > 0 {
			log.Printf("INFO: curator %s: chat-LLM unavailable — synthesis skipped for %d entity/ies (no-op, will retry)", groupID, len(entities))
			synthesisBlocked = true
		}
	} else if len(entities) > 0 {
		// Attach outbound edges to each entity.
		// We need edge names from the ontology — use a best-effort list.
		edgeNames := fetchEdgeNamesForGroup(groupID)
		for i := range entities {
			eid := entityRecordID(entities[i])
			if eid == "" {
				continue
			}
			edges, _ := db.FetchEntityEdges(eid, edgeNames)
			entities[i]["_edges"] = edges
		}

		clusters := clusterEntities(entities, emb)
		log.Printf("INFO: curator %s: %d entity/ies → %d cluster(s)", groupID, len(entities), len(clusters))

		for _, cl := range clusters {
			path, retry, err := processCluster(cl, db, repoPath, groupID, emb, llm)
			if retry {
				synthesisBlocked = true
			}
			if err != nil {
				log.Printf("WARN: process cluster %s: %v", groupID, err)
				continue
			}
			if path != "" {
				newlyWritten = append(newlyWritten, path)
			}
		}
	} else {
		log.Printf("INFO: curator %s: no changed entities since cursor — snapshot only", groupID)
	}

	// Phase 2: Snapshot — emit all auto_serve wiki_doc rows to disk.
	allDocs, err := db.FetchAllAutoServeWikiDocs()
	if err != nil {
		return fmt.Errorf("fetch wiki_docs %s: %w", groupID, err)
	}
	snapshotPaths := emitAllWikiDocs(allDocs, repoPath)

	// Union: newly-written paths take precedence.
	allPaths := unionPaths(newlyWritten, snapshotPaths)
	pruneStaleWikiFiles(allPaths, repoPath, groupID)

	if len(allPaths) > 0 {
		if err := commitAndPush(vignobleCloneDir, groupID, repoPath, allPaths); err != nil {
			log.Printf("WARN: git push %s: %v", groupID, err)
		}
	}

	// Advance the cursor only when there were changed entities AND synthesis was
	// not blocked by chat-LLM unavailability. Advancing while blocked would strand
	// those entities behind the cursor forever (never re-synthesized).
	if len(entities) > 0 && !synthesisBlocked {
		if err := db.SetWikiCuratorCursor(groupID); err != nil {
			log.Printf("WARN: set cursor %s: %v", groupID, err)
		}
	}

	log.Printf("INFO: curator %s done: synthesized=%d snapshot=%d", groupID, len(newlyWritten), len(snapshotPaths))
	return nil
}

// ── Entity helpers ────────────────────────────────────────────────────────────

func entityRecordID(e map[string]any) string {
	switch v := e["id"].(type) {
	case string:
		return v
	case map[string]any:
		// SurrealDB record ID as object {"tb": "entity", "id": {...}}
		tb, _ := v["tb"].(string)
		id := v["id"]
		if tb != "" && id != nil {
			return fmt.Sprintf("%s:%v", tb, id)
		}
	}
	return ""
}

// fetchEdgeNamesForGroup returns edge names from the ontology for a group.
// Falls back to an empty list on error (edges are best-effort).
func fetchEdgeNamesForGroup(groupID string) []string {
	reg, err := ontology.NewRegistry()
	if err != nil {
		return nil
	}
	composed := reg.Compose(groupID)
	if composed == nil {
		return nil
	}
	return composed.EdgeNames()
}

// ── Clustering ────────────────────────────────────────────────────────────────

type cluster struct {
	entities []map[string]any
	centroid []float64
}

func clusterEntities(entities []map[string]any, emb *memory.Embedder) [][]map[string]any {
	// Sort for determinism.
	sorted := make([]map[string]any, len(entities))
	copy(sorted, entities)
	sort.Slice(sorted, func(i, j int) bool {
		ri, _ := sorted[i]["role"].(string)
		rj, _ := sorted[j]["role"].(string)
		ni, _ := sorted[i]["name"].(string)
		nj, _ := sorted[j]["name"].(string)
		if ri != rj {
			return ri < rj
		}
		return ni < nj
	})

	var clusters []cluster

	for _, ent := range sorted {
		vec := entityEmbedding(ent, emb)

		if vec == nil {
			// Cannot compute similarity — isolated cluster.
			clusters = append(clusters, cluster{entities: []map[string]any{ent}})
			continue
		}

		bestIdx := -1
		bestSim := 0.0
		for i, cl := range clusters {
			if cl.centroid == nil || len(cl.entities) >= clusterMaxSize {
				continue
			}
			sim := cosineSimilarity(vec, cl.centroid)
			if sim >= clusterSimilarityThreshold && sim > bestSim {
				bestSim = sim
				bestIdx = i
			}
		}

		if bestIdx >= 0 {
			clusters[bestIdx].entities = append(clusters[bestIdx].entities, ent)
			clusters[bestIdx].centroid = meanEmbedding([][]float64{clusters[bestIdx].centroid, vec})
		} else {
			clusters = append(clusters, cluster{entities: []map[string]any{ent}, centroid: vec})
		}
	}

	result := make([][]map[string]any, len(clusters))
	for i, cl := range clusters {
		result[i] = cl.entities
	}
	return result
}

func entityEmbedding(ent map[string]any, emb *memory.Embedder) []float64 {
	// Prefer pre-computed embedding.
	if raw, ok := ent["_embedding"]; ok {
		if v := toFloat64Slice(raw); v != nil {
			return v
		}
	}
	if raw, ok := ent["embedding"]; ok {
		if v := toFloat64Slice(raw); v != nil {
			return v
		}
	}
	if emb == nil {
		return nil
	}
	name, _ := ent["name"].(string)
	desc, _ := ent["description"].(string)
	text := strings.TrimSpace(name + "\n" + desc)
	if text == "" {
		return nil
	}
	vec, err := emb.Embed(text)
	if err != nil {
		return nil
	}
	return vec
}

// ── Per-cluster processing ────────────────────────────────────────────────────

func processCluster(cl []map[string]any, db *surreal.Client, repoPath, groupID string, emb *memory.Embedder, llm *memory.LLMClient) (path string, retry bool, err error) {
	if len(cl) == 0 {
		return "", false, nil
	}
	primary := cl[0]
	name, _ := primary["name"].(string)
	role, _ := primary["role"].(string)
	desc, _ := primary["description"].(string)

	// Artifact-role clusters are excluded from wiki synthesis.
	if role == "artifact" {
		return "", false, nil
	}
	// Skip only DEGENERATE scaffold entities (scaffold-prefixed name AND trivial
	// description). Real entities also start with **What**: under the old ingester;
	// those MUST be curated so the LLM can clean their titles — gating on the name
	// alone would drop ~43% of a group's knowledge.
	if isDegenerateScaffold(name, desc) {
		return "", false, nil
	}

	slug := slugify(name, 80)
	okfPath := rolePath(role, slug)

	mdFile := filepath.Join(repoPath, okfPath+".md")
	if reservedNames[filepath.Base(mdFile)] {
		return "", false, nil
	}

	// Never overwrite human-authored pages.
	if fm := readFrontmatter(mdFile); fm["source"] == "human" {
		return "", false, nil
	}

	// Compute confidence.
	confidence := computeConfidence(role, len(cl))
	status := "auto_serve"
	if confidence < 0.7 {
		status = "needs_review"
	}

	// Build relations.
	edges := entityEdges(primary)
	relations := buildRelations(edges, role)

	// LLM synthesis. No-fallback: on failure / no-LLM / empty output, skip the
	// upsert entirely — never create or overwrite a doc with degraded content.
	// The entity stays behind the cursor and is retried next cycle.
	synthTitle, synthSummary, body, ok := synthesizeConcept(cl, name, role, desc, edges, llm)
	if !ok {
		// Chat-LLM unavailable or produced nothing usable: skip WITHOUT advancing
		// (retry=true) so this entity is re-synthesized once the LLM is back.
		return "", true, nil
	}
	// A synthesized title must not itself be a scaffold key.
	if isScaffoldName(synthTitle) {
		return "", false, nil
	}

	// Slug + path from the synthesized title.
	okfPath = rolePath(role, slugify(synthTitle, 80))
	mdFile = filepath.Join(repoPath, okfPath+".md")
	if fm := readFrontmatter(mdFile); fm["source"] == "human" {
		return "", false, nil
	}

	// Build embedding from the synthesized content.
	var vec []float64
	if emb != nil {
		embedText := synthTitle + "\n" + truncate(body, 500)
		if v, err := emb.Embed(embedText); err == nil {
			vec = v
		}
	}

	// Dedup AFTER synthesis: if a sufficiently-similar page already exists, write
	// to ITS path instead of creating a near-duplicate. Done here (not pre-
	// synthesis) so the final title's re-slug can't discard the match.
	//
	// Embedding-cosine (FindSimilarWikiDoc), role-scoped (#265), is the only
	// dedup signal: it is semantic (whole-body similarity) so a high score is
	// high-confidence and safe to auto-merge on. An inline lexical/LLM title-
	// match signal was tried and reverted (#265 review, owner decision): a
	// heuristic matcher compensating for an unstable identity key still makes
	// irreversible merges, which is the wrong tool for this problem — a future
	// consolidation tier (vigne/vignoble/global, supersede-not-delete) plus a
	// stable concept identity will handle near-duplicates properly instead.
	dedupPath := ""
	dedupReason := ""
	if vec != nil {
		if existingPath, score, err := db.FindSimilarWikiDoc(role, vec); err == nil &&
			existingPath != "" && existingPath != okfPath && score >= dedupThresholdFor(role) {
			dedupPath = existingPath
			dedupReason = fmt.Sprintf("embedding score=%.3f", score)
		}
	}
	if dedupPath != "" {
		log.Printf("INFO: near-dup %q matches existing %q (%s) — updating existing", okfPath, dedupPath, dedupReason)
		okfPath = dedupPath
		mdFile = filepath.Join(repoPath, okfPath+".md")
		if fm := readFrontmatter(mdFile); fm["source"] == "human" {
			return "", false, nil
		}
	}

	// Persist to SurrealDB.
	fm := map[string]any{
		"type":      role,
		"title":     synthTitle,
		"summary":   synthSummary,
		"group_id":  groupID,
		"confidence": confidence,
		"status":    status,
		"source":    "curator",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	if len(relations) > 0 {
		fm["relations"] = relations
	}
	if _, err := db.UpsertWikiDoc(synthTitle, body, okfPath, synthSummary, confidence, fm, vec); err != nil {
		log.Printf("WARN: upsert wiki_doc %s: %v", okfPath, err)
	}

	// Write to disk.
	if err := writeOKF(mdFile, fm, body); err != nil {
		return "", false, fmt.Errorf("write %s: %w", okfPath, err)
	}
	log.Printf("INFO: wrote OKF page: %s", okfPath)
	return okfPath, false, nil
}

// ── Synthesis ─────────────────────────────────────────────────────────────────

// synthesizeConcept returns ok=false when synthesis cannot produce a genuine
// LLM page (no LLM client, request error, or empty title/body). Callers MUST
// skip the upsert in that case — no fallback / degraded docs (#265).
func synthesizeConcept(cl []map[string]any, name, role, desc string, edges []map[string]any, llm *memory.LLMClient) (title, summary, body string, ok bool) {
	if llm == nil {
		return "", "", "", false
	}
	clusterSize := len(cl)

	entitiesDesc := make([]string, 0, len(cl))
	for _, e := range cl {
		r, _ := e["role"].(string)
		n, _ := e["name"].(string)
		d, _ := e["description"].(string)
		entitiesDesc = append(entitiesDesc, fmt.Sprintf("- [%s] %s: %s", r, n, d))
	}
	edgesDesc := "none"
	if len(edges) > 0 {
		parts := make([]string, 0, len(edges))
		for _, e := range edges {
			edge, _ := e["edge"].(string)
			tgt, _ := e["target_name"].(string)
			parts = append(parts, fmt.Sprintf("- %s → %s", edge, tgt))
		}
		edgesDesc = strings.Join(parts, "\n")
	}

	clusterNote := ""
	if clusterSize > 1 {
		clusterNote = fmt.Sprintf("This concept is supported by %d related observation(s).\n", clusterSize)
	}

	systemPrompt := `You are a technical knowledge curator. Synthesize a concise OKF wiki page from all provided cluster members into one coherent concept page. Respond with a JSON object with exactly three keys: "title" (one clean human-readable phrase, no trailing colon, no markdown marks), "summary" (one distilled sentence), and "body" (a markdown body using sections # Overview, # Details, # Citations). Output only the JSON object — no other text.`

	userMsg := fmt.Sprintf(
		"Synthesize a wiki page for the concept: **%s**\n\nType: %s\nPrimary description: %s\n\n%sAll cluster members:\n%s\n\nRelated entities:\n%s\n\nBe concise and factual. Respond with a JSON object: {\"title\": \"...\", \"summary\": \"...\", \"body\": \"...\"}",
		name, role, desc, clusterNote, strings.Join(entitiesDesc, "\n"), edgesDesc,
	)

	raw, err := llm.Complete([]memory.LLMMessage{{Role: "user", Content: userMsg}}, 1200, systemPrompt)
	if err != nil {
		log.Printf("WARN: LLM synthesis failed for %q: %v — skipping (no fallback)", name, err)
		return "", "", "", false
	}
	parsed := parseSynthesisResponse(raw)
	title = strings.TrimSpace(parsed["title"])
	summary = strings.TrimSpace(parsed["summary"])
	body = parsed["body"]
	if title == "" || strings.TrimSpace(body) == "" {
		log.Printf("WARN: LLM synthesis produced empty title/body for %q — skipping", name)
		return "", "", "", false
	}
	return title, summary, body, true
}

func parseSynthesisResponse(raw string) map[string]string {
	text := strings.TrimSpace(raw)
	// Strip markdown code fences.
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		var inner []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") && !inBlock {
				inBlock = true
				continue
			}
			if strings.HasPrefix(line, "```") && inBlock {
				break
			}
			if inBlock {
				inner = append(inner, line)
			}
		}
		text = strings.TrimSpace(strings.Join(inner, "\n"))
	}
	result := map[string]string{}
	tryParse := func(s string) bool {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			return false
		}
		for _, k := range []string{"title", "summary", "body"} {
			if v, ok := m[k]; ok {
				result[k] = fmt.Sprintf("%v", v)
			}
		}
		return len(result) > 0
	}
	if tryParse(text) {
		return result
	}
	// Last-ditch: find first {...} block.
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		tryParse(text[start : end+1])
	}
	return result
}

// ── Confidence scoring ────────────────────────────────────────────────────────

func computeConfidence(role string, clusterSize int) float64 {
	roleBonus := 0.0
	if highValueRoles[role] {
		roleBonus = 0.12
	}
	sizeBonus := 0.05 * math.Log2(float64(clusterSize)+1)
	c := 0.60 + sizeBonus + roleBonus
	if c > 0.9 {
		c = 0.9
	}
	return math.Round(c*1000) / 1000
}

// ── Relations ─────────────────────────────────────────────────────────────────

func entityEdges(e map[string]any) []map[string]any {
	raw, ok := e["_edges"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case []map[string]any:
		return v
	case []any:
		var out []map[string]any
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func buildRelations(edges []map[string]any, srcRole string) []map[string]string {
	var relations []map[string]string
	seen := map[[2]string]bool{}
	for _, edge := range edges {
		edgeName, _ := edge["edge"].(string)
		targetName, _ := edge["target_name"].(string)
		targetRole, _ := edge["target_role"].(string)
		if edgeName == "" || targetName == "" {
			continue
		}
		key := [2]string{edgeName, targetName}
		if seen[key] {
			continue
		}
		seen[key] = true
		tSlug := slugify(targetName, 0)
		var targetPath string
		if targetRole != "" {
			targetPath = rolePath(targetRole, tSlug)
		} else {
			targetPath = "concepts/" + tSlug
		}
		relations = append(relations, map[string]string{"edge": edgeName, "to": targetPath})
	}
	return relations
}

// ── Snapshot helpers ──────────────────────────────────────────────────────────

func emitAllWikiDocs(docs []map[string]any, repoPath string) []string {
	var written []string
	for _, doc := range docs {
		path, _ := doc["path"].(string)
		if path == "" {
			title, _ := doc["title"].(string)
			role, _ := doc["type"].(string)
			path = rolePath(role, slugify(title, 0))
		}
		mdFile := filepath.Join(repoPath, path+".md")
		if fm := readFrontmatter(mdFile); fm["source"] == "human" {
			continue
		}
		fm := map[string]any{}
		if stored, ok := doc["frontmatter"].(map[string]any); ok {
			for k, v := range stored {
				if k != "content_hash" {
					fm[k] = v
				}
			}
		}
		setDefault(fm, "title", doc["title"])
		setDefault(fm, "type", doc["type"])
		setDefault(fm, "summary", doc["summary"])
		setDefault(fm, "confidence", doc["confidence"])
		setDefault(fm, "status", "auto_serve")
		setDefault(fm, "source", "curator")
		body, _ := doc["body"].(string)
		if err := writeOKF(mdFile, fm, body); err != nil {
			log.Printf("WARN: emit wiki_doc %s: %v", path, err)
			continue
		}
		written = append(written, path)
	}
	return written
}

func pruneStaleWikiFiles(allWritten []string, repoPath, groupID string) {
	written := make(map[string]bool, len(allWritten))
	for _, p := range allWritten {
		written[p] = true
	}
	// Walk repo path for .md files in subdirectories.
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		// Only prune files inside subdirectories (role dirs), not root-level.
		if !strings.Contains(rel, string(filepath.Separator)) {
			return nil
		}
		if reservedNames[filepath.Base(path)] {
			return nil
		}
		pathKey := strings.TrimSuffix(rel, ".md")
		if written[pathKey] {
			return nil
		}
		fm := readFrontmatter(path)
		if fm["source"] == "human" {
			return nil
		}
		// Only prune files that belong to this group_id or have none set.
		if gid, _ := fm["group_id"].(string); gid != "" && gid != groupID {
			return nil
		}
		if err := os.Remove(path); err != nil {
			log.Printf("WARN: prune %s: %v", pathKey, err)
		} else {
			log.Printf("INFO: pruned stale page: %s", pathKey)
		}
		return nil
	})
}

func unionPaths(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range a {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range b {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// ── Git / MR operations ───────────────────────────────────────────────────────

func branchName(groupID string) string {
	safe := regexp.MustCompile(`[^a-zA-Z0-9._-]`).ReplaceAllString(groupID, "-")
	return "wiki-curator/" + safe
}

// commitAndPush stages wiki/<groupID>/... files and commits/pushes them to the
// vignoble repo. gitRoot is the vignoble clone root (actual git repository);
// wikiDir is the subdirectory where the markdown files were written.
func commitAndPush(gitRoot, groupID, wikiDir string, writtenPaths []string) error {
	// Verify gitRoot is a real git repo; bail with an error (not a silent skip)
	// so the caller can log and the regression test can catch this.
	gitDirCheck := exec.Command("git", "-C", gitRoot, "rev-parse", "--git-dir")
	if out, err := gitDirCheck.Output(); err != nil || strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("commitAndPush: %s is not a git repository", gitRoot)
	}

	// Ensure the wiki subdir exists.
	if err := os.MkdirAll(wikiDir, 0o755); err != nil {
		return fmt.Errorf("create wiki dir: %w", err)
	}

	branch := branchName(groupID)
	dryRun := os.Getenv("DRY_RUN") != ""

	runGit := func(args ...string) (string, string, error) {
		c := exec.Command("git", args...)
		c.Dir = gitRoot
		var outBuf, errBuf strings.Builder
		c.Stdout = &outBuf
		c.Stderr = &errBuf
		err := c.Run()
		return outBuf.String(), errBuf.String(), err
	}

	runGit("fetch", "origin") //nolint:errcheck — best-effort
	runGit("checkout", "-B", branch)

	// Stage files using paths relative to gitRoot so git works correctly.
	for _, p := range writtenPaths {
		// p is relative to wikiDir; convert to path relative to gitRoot.
		relWiki, err := filepath.Rel(gitRoot, filepath.Join(wikiDir, p+".md"))
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(gitRoot, relWiki)); err == nil {
			runGit("add", relWiki) //nolint:errcheck
		}
	}

	statusOut, _, _ := runGit("diff", "--cached", "--name-only")
	if strings.TrimSpace(statusOut) == "" {
		log.Printf("INFO: nothing to commit for %s", groupID)
		return nil
	}

	commitLines := []string{fmt.Sprintf("feat(wiki): full snapshot — %d page(s) [%s]", len(writtenPaths), groupID)}
	for _, p := range writtenPaths {
		commitLines = append(commitLines, "- "+p)
	}
	commitMsg := strings.Join(commitLines, "\n")
	if _, _, err := runGit("commit", "--no-verify", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	if dryRun {
		log.Printf("INFO: [DRY_RUN] would push branch %s for %s", branch, groupID)
		return nil
	}

	defaultBranch := detectDefaultBranch(gitRoot, runGit)
	title := fmt.Sprintf("wiki(curator): full wiki snapshot for %s", groupID)

	_, stderr, err := runGit(
		"push", "--force-with-lease", "-u", "origin", branch,
		"-o", "merge_request.create",
		"-o", "merge_request.target="+defaultBranch,
		"-o", "merge_request.title="+title,
		"-o", "merge_request.remove_source_branch=false",
	)
	if err != nil {
		// Try REST fallback.
		if mrURL := openMRviaREST(gitRoot, branch, defaultBranch, title, groupID); mrURL != "" {
			log.Printf("INFO: MR for %s: %s (via REST)", groupID, mrURL)
		} else {
			return fmt.Errorf("git push failed: %w", err)
		}
	} else {
		if mrURL := extractMRURL(stderr); mrURL != "" {
			log.Printf("INFO: MR for %s: %s (via push options)", groupID, mrURL)
		}
	}
	// Write sentinel timestamp for git-publish health signal.
	sentinel := filepath.Join(wikiDir, ".wiki_commit_ts")
	_ = os.WriteFile(sentinel, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	return nil
}

func detectDefaultBranch(repoPath string, runGit func(...string) (string, string, error)) string {
	out, _, err := runGit("symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		ref := strings.TrimSpace(out)
		if parts := strings.Split(ref, "/"); len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	out2, _, _ := runGit("remote", "show", "origin")
	for _, line := range strings.Split(out2, "\n") {
		if strings.Contains(line, "HEAD branch") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return "master"
}

func extractMRURL(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		stripped := strings.TrimPrefix(strings.TrimSpace(line), "remote:")
		stripped = strings.TrimSpace(stripped)
		if strings.HasPrefix(stripped, "https://") && strings.Contains(stripped, "/merge_requests/") {
			return stripped
		}
	}
	return ""
}

func openMRviaREST(repoPath, branch, defaultBranch, title, groupID string) string {
	botToken := os.Getenv("GITLAB_TOKEN")
	if botToken == "" {
		return ""
	}
	gitlabHost := os.Getenv("GITLAB_HOST")
	if gitlabHost == "" {
		gitlabHost = remoteProjectHost(repoPath)
	}
	projectPath := remoteProjectPath(repoPath)
	if projectPath == "" || gitlabHost == "" {
		return ""
	}
	encoded := strings.ReplaceAll(projectPath, "/", "%2F")
	apiURL := fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests", gitlabHost, encoded)
	description := fmt.Sprintf("## Wiki curator — %s\n\nAuto-generated OKF pages synthesized from the SurrealDB typed graph.\n\n- Branch: `%s`\n- Group: `%s`\n", groupID, branch, groupID)
	payload := map[string]any{
		"source_branch":        branch,
		"target_branch":        defaultBranch,
		"title":                title,
		"description":          description,
		"remove_source_branch": false,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, apiURL, strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	req.Header.Set("PRIVATE-TOKEN", botToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}
	if resp.StatusCode == 201 || resp.StatusCode == 200 {
		url, _ := data["web_url"].(string)
		return url
	}
	return ""
}

func remoteProjectPath(repoPath string) string {
	c := exec.Command("git", "remote", "get-url", "origin")
	c.Dir = repoPath
	out, err := c.Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	if strings.Contains(url, ":") && !strings.HasPrefix(url, "http") {
		// SSH: git@host:group/project.git
		parts := strings.SplitN(url, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	// HTTPS: strip scheme + host.
	re := regexp.MustCompile(`^https?://[^/]+/(.+?)(?:\.git)?$`)
	if m := re.FindStringSubmatch(url); len(m) == 2 {
		return m[1]
	}
	return ""
}

func remoteProjectHost(repoPath string) string {
	c := exec.Command("git", "remote", "get-url", "origin")
	c.Dir = repoPath
	out, err := c.Output()
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(out))
	if strings.Contains(url, ":") && !strings.HasPrefix(url, "http") {
		// SSH: git@HOST:...
		parts := strings.SplitN(url, "@", 2)
		if len(parts) == 2 {
			host := strings.SplitN(parts[1], ":", 2)[0]
			return host
		}
	}
	re := regexp.MustCompile(`^https?://([^/]+)/`)
	if m := re.FindStringSubmatch(url); len(m) == 2 {
		return m[1]
	}
	return ""
}

// ── OKF file I/O ──────────────────────────────────────────────────────────────

func writeOKF(mdFile string, fm map[string]any, body string) error {
	if err := os.MkdirAll(filepath.Dir(mdFile), 0o755); err != nil {
		return err
	}
	fmBytes, err := yaml.Marshal(fm)
	if err != nil {
		return err
	}
	content := "---\n" + string(fmBytes) + "---\n" + body
	return os.WriteFile(mdFile, []byte(content), 0o644)
}

func readFrontmatter(mdFile string) map[string]any {
	data, err := os.ReadFile(mdFile)
	if err != nil {
		return map[string]any{}
	}
	re := regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n`)
	m := re.FindSubmatch(data)
	if m == nil {
		return map[string]any{}
	}
	var fm map[string]any
	if err := yaml.Unmarshal(m[1], &fm); err != nil {
		return map[string]any{}
	}
	if fm == nil {
		return map[string]any{}
	}
	return fm
}

// ── Path helpers ──────────────────────────────────────────────────────────────

// wikiRepoPath returns the directory where wiki markdown files for groupID
// are written. It lives under the vignoble clone root so git tracks them.
// vignobleCloneDir is the root of the cloned vignoble repo.
func wikiRepoPath(groupID, vignobleCloneDir string) string {
	return filepath.Join(vignobleCloneDir, "wiki", groupID)
}

func rolePath(role, slug string) string {
	roleMap := map[string]string{
		"decision": "decisions", "diagnosis": "diagnoses",
		"verdict": "verdicts", "log_pattern": "log-patterns",
		"artifact": "artifacts", "concept": "concepts",
	}
	dir, ok := roleMap[role]
	if !ok || role == "" {
		dir = "concepts"
	}
	if slug == "" {
		slug = "unknown"
	}
	return dir + "/" + slug
}

func slugify(text string, maxLen int) string {
	s := strings.ToLower(text)
	var b strings.Builder
	prev := '-'
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prev = r
		} else if prev != '-' {
			b.WriteByte('-')
			prev = '-'
		}
	}
	result := strings.Trim(b.String(), "-")
	if maxLen > 0 && len(result) > maxLen {
		result = result[:maxLen]
		result = strings.TrimRight(result, "-")
	}
	if result == "" {
		result = "unknown"
	}
	return result
}

// ── Math helpers ──────────────────────────────────────────────────────────────

func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

func meanEmbedding(vecs [][]float64) []float64 {
	var valid [][]float64
	for _, v := range vecs {
		if v != nil {
			valid = append(valid, v)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	dim := len(valid[0])
	out := make([]float64, dim)
	for _, v := range valid {
		for i, x := range v {
			out[i] += x
		}
	}
	n := float64(len(valid))
	for i := range out {
		out[i] /= n
	}
	return out
}

func toFloat64Slice(v any) []float64 {
	switch t := v.(type) {
	case []float64:
		return t
	case []any:
		out := make([]float64, len(t))
		for i, f := range t {
			if fv, ok := f.(float64); ok {
				out[i] = fv
			}
		}
		return out
	}
	return nil
}

// ── Misc helpers ──────────────────────────────────────────────────────────────

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func setDefault(m map[string]any, key string, val any) {
	if _, ok := m[key]; !ok {
		m[key] = val
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
