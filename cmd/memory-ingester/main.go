// Command memory-ingester is the Go port of services/memory/ingester.py.
//
// It pulls curated observations from Engram, embeds them via Rosetta, maps
// them to the pinard ontology, and upserts records into SurrealDB. It also
// subscribes to NATS for query handler events.
//
// Subcommands:
//
//	clone-vignobles — port of services/memory/wiki/clone_vignobles.py;
//	                  reads the pinard-vignobles KV bucket and clones/pulls
//	                  every vignoble repo plus the global pinard-wiki repo.
//	                  Per-repo failures are warnings; always exits 0.
//
// Environment variables:
//
//	NATS_URL              — NATS server URL (default: nats://localhost:4222)
//	NATS_VIGNOBLE         — vignoble name (required for default ingester; used for logging in clone-vignobles)
//	VIGNOBLE_DIR          — vignoble root dir (for per-vignoble ontology discovery); if vignes.yaml
//	                        is present here, all vigne keys are used as group_ids to ingest
//	VIGNOBLES_BASE_DIR    — parent dir of per-vignoble clones; scanned for vignes.yaml files
//	                        to discover group_ids when VIGNOBLE_DIR is not set
//	PINARD_ONTOLOGY_DIRS  — colon-separated paths to domain ontology YAML dirs
//	VIGNOBLE_LOGS         — path to logs dir (default: ./logs)
//	SURREAL_URL           — SurrealDB endpoint
//	SURREAL_USER          — SurrealDB username
//	SURREAL_PASS          — SurrealDB password
//	ENGRAM_URL            — Engram HTTP API URL
//	ENGRAM_SINCE_HOURS    — observation look-back window (default: 168)
//	MEMORY_ENGRAM_SOURCE  — 'http' (default) or 'postgres'
//	ENGRAM_PG_DSN         — Postgres DSN; required when MEMORY_ENGRAM_SOURCE=postgres
//	EMBEDDING_URL         — embedding endpoint (ROSETTA_URL is a deprecated alias)
//	MEMORY_LLM_*          — LLM client configuration (see internal/memory/llm.go)
//	MEMORY_GROUP_IDS      — comma-separated group_ids to filter/ingest (optional; overrides auto-discovery)
//
// Environment variables for clone-vignobles:
//
//	NATS_URL         — NATS server URL (default: nats://localhost:4222)
//	CLONE_BASE_DIR   — parent directory for clones (default: /data/repos)
//	PINARD_WIKI_REPO — SSH URL for the global pinard-wiki repo (optional)
//	GITLAB_SSH_HOST  — SSH hostname used to build vignoble repo URLs (default: github.com)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/memory"
	"github.com/Genentech/pinard/internal/ontology"
	"github.com/Genentech/pinard/internal/surreal"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

var (
	flagReingest bool
	flagRecurate bool
	flagRechunk  bool
)

func main() {
	root := &cobra.Command{
		Use:   "memory-ingester",
		Short: "Pinard memory ingester — Engram → SurrealDB",
		RunE:  run,
	}
	root.Flags().BoolVar(&flagReingest, "reingest", false, "Reset all ingest cursors to seq=0 and exit")
	root.Flags().BoolVar(&flagRecurate, "recurate", false, "Delete non-human wiki_doc rows and drop curator cursors, then exit")
	root.Flags().BoolVar(&flagRechunk, "rechunk", false, "Backfill wiki_chunk rows for all wiki_doc pages, then exit")

	root.AddCommand(&cobra.Command{
		Use:   "clone-vignobles",
		Short: "Clone/pull all vignoble repos from NATS KV + the global wiki repo",
		Long: `Port of services/memory/wiki/clone_vignobles.py.

Reads the pinard-vignobles KV bucket, derives the SSH URL for each vignoble
(git@GITLAB_SSH_HOST:OWNER/vignoble-NAME.git), and clones or pulls it under
CLONE_BASE_DIR/vignobles/vignoble-NAME/. Also clones or pulls PINARD_WIKI_REPO
under CLONE_BASE_DIR/pinard-wiki/.

Per-repo failures are logged as warnings. The command always exits 0 so the
k8s init-container never crashes on a temporarily unreachable repo.`,
		RunE: runCloneVignobles,
	})

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

// ── clone-vignobles ───────────────────────────────────────────────────────────

const kvBucket = "pinard-vignobles"

func runCloneVignobles(_ *cobra.Command, _ []string) error {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	cloneBase := envOr("CLONE_BASE_DIR", "/data/repos")
	sshHost := envOr("GITLAB_SSH_HOST", "github.com")
	pinardWikiRepo := os.Getenv("PINARD_WIKI_REPO")

	// Connect to NATS — failure means we skip vignoble discovery but still clone the wiki.
	var vignobles []vignobleEntry
	nc, err := nats.Connect(natsURL,
		nats.Name("memory-ingester/clone-vignobles"),
		nats.Timeout(10*time.Second),
		nats.MaxReconnects(3),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		log.Printf("WARN: cannot connect to NATS (%s): %v — skipping vignoble KV discovery", natsURL, err)
	} else {
		vignobles = listVignobles(nc)
		nc.Close()
	}

	vignobleDir := filepath.Join(cloneBase, "vignobles")
	if err := os.MkdirAll(vignobleDir, 0755); err != nil {
		log.Printf("WARN: mkdir %s: %v", vignobleDir, err)
	}

	for _, v := range vignobles {
		repoURL := fmt.Sprintf("git@%s:%s/vignoble-%s.git", sshHost, v.Owner, v.Name)
		dest := filepath.Join(vignobleDir, "vignoble-"+v.Name)
		cloneOrPull(repoURL, dest)
	}

	if pinardWikiRepo != "" {
		wikiDest := filepath.Join(cloneBase, "pinard-wiki")
		cloneOrPull(pinardWikiRepo, wikiDest)
	} else {
		log.Printf("INFO: PINARD_WIKI_REPO not set — skipping pinard-wiki clone")
	}

	log.Printf("INFO: clone-vignobles done")
	return nil // always exit 0
}

type vignobleEntry struct {
	Name  string
	Owner string
}

func listVignobles(nc *nats.Conn) []vignobleEntry {
	js, err := nc.JetStream()
	if err != nil {
		log.Printf("WARN: JetStream context: %v", err)
		return nil
	}
	kv, err := js.KeyValue(kvBucket)
	if err != nil {
		log.Printf("WARN: open KV bucket %s: %v", kvBucket, err)
		return nil
	}

	// Enumerate keys via StreamInfo (same approach as internal/pnats KV.Keys —
	// avoids the WatchAll consumer reliability issues documented there).
	prefix := "$KV." + kvBucket + "."
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	si, err := js.StreamInfo("KV_"+kvBucket, &nats.StreamInfoRequest{SubjectsFilter: prefix + ">"}, nats.Context(ctx))
	if err != nil {
		log.Printf("WARN: list keys in %s: %v", kvBucket, err)
		return nil
	}

	var entries []vignobleEntry
	for subject := range si.State.Subjects {
		name := strings.TrimPrefix(subject, prefix)
		if name == subject || name == "" {
			continue
		}
		entry, err := kv.Get(name)
		if err != nil {
			log.Printf("WARN: read KV entry %s: %v", name, err)
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(entry.Value(), &data); err != nil {
			log.Printf("WARN: parse KV entry %s: %v", name, err)
			continue
		}
		owner, _ := data["owner"].(string)
		if owner == "" {
			log.Printf("WARN: no owner for vignoble %s — skipping", name)
			continue
		}
		entries = append(entries, vignobleEntry{Name: name, Owner: owner})
	}
	return entries
}

func cloneOrPull(repoURL, dest string) {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		log.Printf("WARN: mkdir parent of %s: %v", dest, err)
		return
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		log.Printf("INFO: pulling %s", dest)
		out, err := exec.Command("git", "-C", dest, "pull", "--ff-only").CombinedOutput()
		if err != nil {
			log.Printf("WARN: git pull --ff-only %s: %v: %s", dest, err, strings.TrimSpace(string(out)))
		}
	} else {
		log.Printf("INFO: cloning %s → %s", repoURL, dest)
		out, err := exec.Command("git", "clone", repoURL, dest).CombinedOutput()
		if err != nil {
			log.Printf("WARN: git clone %s: %v: %s", repoURL, err, strings.TrimSpace(string(out)))
		}
	}
}

func run(cmd *cobra.Command, args []string) error {
	setupLogging()

	vignoble := os.Getenv("NATS_VIGNOBLE")
	if vignoble == "" {
		return fmt.Errorf("NATS_VIGNOBLE is required")
	}

	if flagReingest {
		return doReingest(vignoble)
	}
	if flagRecurate {
		return doRecurate(vignoble)
	}
	if flagRechunk {
		return doRechunk(vignoble)
	}

	return runIngester(vignoble)
}

// ── Engram ingestion ──────────────────────────────────────────────────────────

// obsTypeToRole maps Engram observation types to SurrealDB entity roles.
var obsTypeToRole = map[string]string{
	// Canonical ingester types
	"rule":                  "decision",
	"fact":                  "artifact",
	"teaching-episode":      "task",
	"summary":               "task",
	"diagnosis":             "diagnosis",
	"action":                "action",
	"log_pattern":           "log_pattern",
	"environment_condition": "environment_condition",
	"gate":                  "gate",
	"step":                  "step",
	"verdict":               "verdict",
	// mem_save observation types from the Pinard agent
	"bugfix":          "diagnosis",
	"decision":        "decision",
	"architecture":    "artifact",
	"discovery":       "artifact",
	"pattern":         "decision",
	"config":          "artifact",
	"preference":      "artifact",
	"session_summary": "task",
	"plan":            "task",
	"manual":          "decision",
}

// globalRegistry is the ontology registry loaded once at startup.
var globalRegistry *ontology.Registry

func getRegistry() *ontology.Registry {
	if globalRegistry != nil {
		return globalRegistry
	}
	reg, err := ontology.NewRegistry()
	if err != nil {
		log.Fatalf("FATAL: load core ontology: %v", err)
	}
	vignoblePath := os.Getenv("VIGNOBLE_DIR")
	if errs := reg.LoadFromEnv(vignoblePath); len(errs) > 0 {
		for _, e := range errs {
			log.Printf("WARN: ontology load: %v", e)
		}
	}
	globalRegistry = reg
	return reg
}

func obsToRoleName(content, obsType, groupID string, reg *ontology.Registry) (role, name string) {
	composed := reg.Compose(groupID)
	role = obsTypeToRole[obsType]
	if role == "" || !composed.HasRole(role) {
		role = "artifact"
		if !composed.HasRole("artifact") {
			roles := composed.EntityRoles()
			if len(roles) > 0 {
				role = roles[0]
			}
		}
	}
	return role, deriveEntityName(content)
}

// scaffoldMarkerRE matches a leading mem_save structured-body key
// (**What**: / Why: / Where: / Learned: / …). Such a marker must never become
// the entity name — otherwise entities like "What:env" leak into the graph and
// produce degenerate wiki titles. We strip the marker and use the actual text.
var scaffoldMarkerRE = regexp.MustCompile(`(?i)^\**\s*(what|why|where|learned|goal|instructions|discoveries|accomplished|next steps|relevant files)\**\s*:\s*`)

// deriveEntityName picks the first meaningful line as the entity name, stripping
// markdown headings and any leading scaffold marker. It skips lines that are
// scaffold-only (marker with no text after it) and falls back to the collapsed
// content when nothing better is found.
func deriveEntityName(content string) string {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "#"))
		if line == "" {
			continue
		}
		if m := scaffoldMarkerRE.FindStringIndex(line); m != nil {
			line = strings.TrimSpace(line[m[1]:])
		}
		if line == "" {
			continue
		}
		if len(line) > 120 {
			line = line[:120]
		}
		return line
	}
	s := strings.TrimSpace(strings.ReplaceAll(content, "\n", " "))
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func ingestObservation(obs memory.EngramObservation, emb *memory.Embedder, db *surreal.Client, reg *ontology.Registry) error {
	role, name := obsToRoleName(obs.Content, obs.ObsType, obs.GroupID, reg)

	var vec []float64
	if v, err := emb.Embed(obs.Content); err != nil {
		log.Printf("WARN: embedding failed for obs %s: %v", obs.ObsID, err)
	} else {
		vec = v
	}

	composed := reg.Compose(obs.GroupID)
	if !composed.HasRole(role) {
		_, err := db.UpsertEntityStaging(name, role, obs.Content,
			fmt.Sprintf("obs_type=%q not in composed ontology for group_id=%q", obs.ObsType, obs.GroupID),
			"engram_observation", nil, vec)
		return err
	}

	// Validate the record's data against the JSON Schema for this role.
	// We surface Content as the top-level data map with a single "description" field
	// so the base-field schema (description: string, role: string) is exercised.
	recordData := map[string]any{
		"role":        role,
		"name":        name,
		"description": obs.Content,
	}
	if validationErr := ontology.ValidateRecord(composed, role, recordData); validationErr != "" {
		log.Printf("WARN: record validation failed obs=%s role=%s: %s", obs.ObsID, role, validationErr)
		_, err := db.UpsertEntityStaging(name, role, obs.Content,
			fmt.Sprintf("schema validation failed: %s", validationErr),
			"engram_observation", nil, vec)
		return err
	}

	_, err := db.UpsertEntity(role, name, obs.Content, nil, vec, "1.0.0", "engram_pg")
	return err
}

// discoverSourceProjects returns the set of source project names to ingest,
// grouped by canonical group_id.
//
// For the postgres source, it calls DiscoverProjects on the PG reader to get
// the live set from cloud_mutations. For the HTTP source (local dev), it falls
// back to resolveGroupIDs so each canonical id is its own single source.
//
// If MEMORY_GROUP_IDS is set it acts as an explicit filter: only canonical
// group_ids that appear in the list (after normalization) are included.
func discoverSourceProjects(vignoble, source string) map[string][]string {
	explicitFilter := resolveExplicitFilter()

	if source == "postgres" {
		// Auto-discover from cloud_mutations.project — no hard-coded list needed.
		pgReader := memory.NewEngramPostgresReader(vignoble)
		projects, err := pgReader.DiscoverProjects()
		if err != nil {
			log.Printf("WARN: DiscoverProjects failed (%v) — falling back to resolveGroupIDs", err)
		} else if len(projects) > 0 {
			groups := memory.GroupByCanonical(projects)
			if len(explicitFilter) > 0 {
				filtered := make(map[string][]string)
				for _, id := range explicitFilter {
					canon := memory.NormalizeGroupID(id)
					if sources, ok := groups[canon]; ok {
						filtered[canon] = sources
					}
				}
				return filtered
			}
			return groups
		}
	}

	// HTTP source or PG discovery fallback: each resolved group_id is its own source.
	ids := resolveGroupIDs(vignoble)
	groups := make(map[string][]string, len(ids))
	for _, id := range ids {
		canon := memory.NormalizeGroupID(id)
		groups[canon] = append(groups[canon], id)
	}
	return groups
}

// resolveExplicitFilter returns the canonical group_ids from MEMORY_GROUP_IDS,
// or nil when the env var is unset/empty (meaning: ingest all).
func resolveExplicitFilter() []string {
	s := os.Getenv("MEMORY_GROUP_IDS")
	if s == "" {
		return nil
	}
	var ids []string
	for _, id := range strings.Split(s, ",") {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func runEngramIngestion(vignoble string, emb *memory.Embedder, reg *ontology.Registry) {
	source := envOr("MEMORY_ENGRAM_SOURCE", "http")

	// Discover source projects grouped by canonical group_id.
	// For postgres: auto-discovered from cloud_mutations (no hard-coded list).
	// MEMORY_GROUP_IDS, if set, acts as an explicit filter/override.
	groups := discoverSourceProjects(vignoble, source)
	log.Printf("INFO: ingesting %d canonical group(s)", len(groups))


	for canonical, sourceProjects := range groups {
		db, err := surreal.New(canonical)
		if err != nil {
			log.Printf("ERROR: SurrealDB connect for %s: %v", canonical, err)
			continue
		}
		if err := db.EnsureSchema(); err != nil {
			log.Printf("ERROR: EnsureSchema for %s: %v", canonical, err)
			db.Close()
			continue
		}

		// Best-effort dedup: clean up duplicate/legacy cursor rows from
		// pre-hash-ID deployments. Non-fatal.
		if err := db.DeduplicateCursors(); err != nil {
			log.Printf("WARN: DeduplicateCursors for %s: %v", canonical, err)
		}

		totalIngested := int64(0)
		var cycleFailed int64
		if source == "postgres" {
			// Ingest from each source project into the canonical DB.
			// Each source project gets its own cursor so seq spaces don't collide.
			for _, srcProject := range sourceProjects {
				cursorID := "engram_pg:" + srcProject
				lastSeq, err := db.GetIngestCursor(cursorID)
				if err != nil {
					log.Printf("WARN: get ingest cursor %s: %v", cursorID, err)
				}
				pgReader := memory.NewEngramPostgresReader(srcProject)
				obs, newSeq, err := pgReader.Fetch(lastSeq)
				if err != nil {
					log.Printf("ERROR: Engram PG fetch project=%s: %v", srcProject, err)
					continue

				}
				for _, o := range obs {
					if err := ingestObservation(o, emb, db, reg); err != nil {
						log.Printf("WARN: ingest obs %s: %v", o.ObsID, err)
					} else {
						totalIngested++
					}
				}
				if newSeq > lastSeq {
					if err := db.SetIngestCursor(cursorID, newSeq); err != nil {
						log.Printf("WARN: set ingest cursor %s: %v", cursorID, err)
					}
				}
				log.Printf("INFO: PG fetch canonical=%s source=%s fetched=%d cursor=%d→%d",
					canonical, srcProject, len(obs), lastSeq, newSeq)
			}
		} else {
			// HTTP source: each canonical maps to itself.
			httpReader := memory.NewEngramReader(canonical)
			obs, err := httpReader.Fetch()
			if err != nil {
				log.Printf("ERROR: Engram HTTP fetch for %s: %v", canonical, err)
				db.Close()
				continue
			}
			for _, o := range obs {
				if err := ingestObservation(o, emb, db, reg); err != nil {
					log.Printf("WARN: ingest obs %s: %v", o.ObsID, err)
					cycleFailed++
				} else {
					totalIngested++
				}
			}
		}
		prevStats, _ := db.GetIngestStats(canonical)
		totalFailed := prevStats.Failed + cycleFailed
		if err := db.UpdateIngestStats(canonical, prevStats.Ingested+totalIngested, totalFailed); err != nil {
			log.Printf("WARN: update ingest_stats for %s: %v", canonical, err)
		}
		log.Printf("INFO: ingest cycle canonical=%s sources=%v ingested=%d failed=%d",
			canonical, sourceProjects, totalIngested, cycleFailed)

		db.Close()
	}
}

// ── NATS consumers ────────────────────────────────────────────────────────────

// memoryStatusResponse is the JSON shape served by the memory.status handler.
type memoryStatusResponse struct {
	Engram    engramStatusSection    `json:"engram"`
	SurrealDB surrealStatusSection   `json:"surrealdb"`
	Wiki      wikiStatusSection      `json:"wiki"`
}

type engramStatusSection struct {
	Reachable bool  `json:"reachable"`
	Pending   int64 `json:"pending"`
}

type surrealStatusSection struct {
	TotalLag    int64               `json:"total_lag"`
	TotalFailed int64               `json:"total_failed"`
	Groups      []surrealGroupStats `json:"groups"`
}

type surrealGroupStats struct {
	Group      string `json:"group"`
	EngramMax  int64  `json:"engram_max"`
	Cursor     int64  `json:"cursor"`
	Lag        int64  `json:"lag"`
	Entities   int64  `json:"entities"`
	Failed     int64  `json:"failed"`
	LastIngest string `json:"last_ingest"`
}

type wikiStatusSection struct {
	TotalDocs   int64            `json:"total_docs"`
	NeedsReview int64            `json:"needs_review"`
	LastRollup  string           `json:"last_rollup"`
	Groups      []wikiGroupStats `json:"groups"`
}

type wikiGroupStats struct {
	Group          string `json:"group"`
	Docs           int64  `json:"docs"`
	AutoServe      int64  `json:"auto_serve"`
	NeedsReview    int64  `json:"needs_review"`
	GitPublishOk   bool   `json:"git_publish_ok"`
	LastWikiCommit string `json:"last_wiki_commit,omitempty"`
}

// engramMaxSeq queries the Postgres cloud_mutations table for the max seq for project.
// Returns 0 on error or when MEMORY_ENGRAM_SOURCE != "postgres".
func engramMaxSeq(project string) int64 {
	if envOr("MEMORY_ENGRAM_SOURCE", "http") != "postgres" {
		return 0
	}
	r := memory.NewEngramPostgresReader(project)
	max, err := r.MaxSeq()
	if err != nil {
		return 0
	}
	return max
}

// engramReachable checks whether the Engram HTTP API is reachable.
func checkEngramReachable() bool {
	engURL := os.Getenv("ENGRAM_URL")
	if engURL == "" {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(engURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

func subscribeMemoryStatusHandler(nc *nats.Conn, vignoble string) error {
	// Wildcard: one ingester serves every vignoble's memory-status. The requested
	// vignoble is the 2nd subject token (pinard.<vignoble>.memory-status).
	_, err := nc.Subscribe("pinard.*.memory-status", func(msg *nats.Msg) {
		reqVignoble := vignoble
		if parts := strings.Split(msg.Subject, "."); len(parts) >= 3 && parts[1] != "" {
			reqVignoble = parts[1]
		}
		groupIDs := resolveGroupIDsForVignoble(reqVignoble)

		resp := memoryStatusResponse{
			Engram: engramStatusSection{
				Reachable: checkEngramReachable(),
			},
		}

		for _, gid := range groupIDs {
			db, err := surreal.New(gid)
			if err != nil {
				log.Printf("WARN: memory-status SurrealDB connect %s: %v", gid, err)
				continue
			}

			cursor, _ := db.GetIngestCursor("engram_pg:" + gid)
			entities, _ := db.GetEntityCount()
			stats, _ := db.GetIngestStats(gid)
			wiki, _ := db.GetWikiStats()
			db.Close()

			engramMax := engramMaxSeq(gid)
			lag := engramMax - cursor
			if lag < 0 {
				lag = 0
			}

			lastIngest := ""
			if !stats.LastIngest.IsZero() {
				lastIngest = stats.LastIngest.UTC().Format(time.RFC3339)
			}

			resp.SurrealDB.TotalLag += lag
			resp.SurrealDB.TotalFailed += stats.Failed
			resp.SurrealDB.Groups = append(resp.SurrealDB.Groups, surrealGroupStats{
				Group:      gid,
				EngramMax:  engramMax,
				Cursor:     cursor,
				Lag:        lag,
				Entities:   entities,
				Failed:     stats.Failed,
				LastIngest: lastIngest,
			})

			lastRollup := ""
			if !wiki.LastRollup.IsZero() {
				lastRollup = wiki.LastRollup.UTC().Format(time.RFC3339)
			}
			resp.Wiki.TotalDocs += wiki.TotalDocs
			resp.Wiki.NeedsReview += wiki.NeedsReview
			if lastRollup != "" && (resp.Wiki.LastRollup == "" || lastRollup > resp.Wiki.LastRollup) {
				resp.Wiki.LastRollup = lastRollup
			}
			gitOk, lastCommit := readWikiCommitSentinel(reqVignoble, gid)
			resp.Wiki.Groups = append(resp.Wiki.Groups, wikiGroupStats{
				Group:          gid,
				Docs:           wiki.TotalDocs,
				AutoServe:      wiki.AutoServe,
				NeedsReview:    wiki.NeedsReview,
				GitPublishOk:   gitOk,
				LastWikiCommit: lastCommit,
			})
		}

		out, _ := json.Marshal(resp)
		_ = msg.Respond(out)
	})
	return err
}

func subscribeQueryHandler(nc *nats.Conn, vignoble string, emb *memory.Embedder) error {
	// Wildcard: serve queries for every vignoble (handler scopes by req.group_id).
	_, err := nc.Subscribe("pinard.*.memory.query", func(msg *nats.Msg) {
		var req map[string]any
		if err := json.Unmarshal(msg.Data, &req); err != nil {
			_ = msg.Respond([]byte(`{"error":"invalid request"}`))
			return
		}
		groupID, _ := req["group_id"].(string)
		queryText, _ := req["query"].(string)
		if groupID == "" || queryText == "" {
			_ = msg.Respond([]byte(`{"error":"group_id and query required"}`))
			return
		}
		db, err := surreal.New(groupID)
		if err != nil {
			_ = msg.Respond([]byte(`{"error":"surreal connect failed"}`))
			return
		}
		defer db.Close()

		// Embed the query text for vector recall.
		vec, embedErr := emb.Embed(queryText)
		var rows []map[string]any
		if embedErr == nil {
			rows, _ = db.Recall(vec, 5, "")
		} else {
			rows, _ = db.Lookup(queryText, 5)
		}

		resp, _ := json.Marshal(map[string]any{"results": rows})
		_ = msg.Respond(resp)
	})
	return err
}

// ── Main ingester loop ────────────────────────────────────────────────────────

func runIngester(vignoble string) error {
	reg := getRegistry()

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	nc, err := nats.Connect(natsURL,
		nats.Name("memory-ingester"),
		nats.ReconnectWait(5*time.Second),
		nats.MaxReconnects(-1),
	)
	if err != nil {
		return fmt.Errorf("NATS connect: %w", err)
	}
	defer nc.Close()
	log.Printf("INFO: memory-ingester connected to NATS vignoble=%s", vignoble)

	emb := memory.NewEmbedder()

	// Subscribe to query handler.
	if err := subscribeQueryHandler(nc, vignoble, emb); err != nil {
		log.Printf("WARN: subscribe query handler: %v", err)
	}

	// Subscribe to memory status handler.
	if err := subscribeMemoryStatusHandler(nc, vignoble); err != nil {
		log.Printf("WARN: subscribe memory status handler: %v", err)
	}

	// Run Engram ingestion loop (every 30 minutes).
	go func() {
		for {
			runEngramIngestion(vignoble, emb, reg)
			time.Sleep(30 * time.Minute)
		}
	}()

	// Wait for shutdown signal.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	log.Printf("INFO: memory-ingester shutting down")
	return nil
}

// ── Admin commands ────────────────────────────────────────────────────────────

func doReingest(vignoble string) error {
	source := envOr("MEMORY_ENGRAM_SOURCE", "http")
	groups := discoverSourceProjects(vignoble, source)
	for canonical, sourceProjects := range groups {
		db, err := surreal.New(canonical)
		if err != nil {
			log.Printf("ERROR: SurrealDB for %s: %v", canonical, err)
			continue
		}
		for _, src := range sourceProjects {
			cursorID := "engram_pg:" + src
			if err := db.SetIngestCursor(cursorID, 0); err != nil {
				log.Printf("ERROR: reset cursor %s: %v", cursorID, err)
			} else {
				log.Printf("INFO: reset ingest cursor %s", cursorID)
			}
		}
		db.Close()
	}
	return nil
}

func doRecurate(vignoble string) error {
	for _, gid := range resolveGroupIDs(vignoble) {
		db, err := surreal.New(gid)
		if err != nil {
			log.Printf("ERROR: SurrealDB for %s: %v", gid, err)
			continue
		}
		// Delete non-human wiki_doc rows and drop wiki_curator_cursor table.
		sqls := []string{
			"DELETE wiki_doc WHERE frontmatter.source != 'human'",
			"REMOVE TABLE IF EXISTS wiki_curator_cursor",
		}
		for _, sql := range sqls {
			if _, err := db.Query(sql, nil); err != nil {
				log.Printf("WARN: recurate %s %q: %v", gid, sql, err)
			}
		}
		log.Printf("INFO: recurate complete for %s", gid)
		db.Close()
	}
	return nil
}

func doRechunk(vignoble string) error {
	for _, gid := range resolveGroupIDs(vignoble) {
		db, err := surreal.New(gid)
		if err != nil {
			log.Printf("ERROR: SurrealDB for %s: %v", gid, err)
			continue
		}
		// Fetch all wiki_doc rows and rechunk.
		rows, err := db.Query("SELECT path, title, body FROM wiki_doc", nil)
		if err != nil {
			log.Printf("ERROR: fetch wiki_docs for %s: %v", gid, err)
			db.Close()
			continue
		}
		emb := memory.NewEmbedder()
		chunked := 0
		for _, rowSet := range rows {
			pages, ok := rowSet.([]any)
			if !ok {
				continue
			}
			for _, p := range pages {
				page, ok := p.(map[string]any)
				if !ok {
					continue
				}
				path, _ := page["path"].(string)
				title, _ := page["title"].(string)
				body, _ := page["body"].(string)
				if path == "" || body == "" {
					continue
				}
				chunks := chunkWikiBody(title, body)
				if _, err := db.DeleteWikiChunksByPath(path); err != nil {
					log.Printf("WARN: delete chunks for %s: %v", path, err)
				}
				var wikiChunks []surreal.WikiChunk
				for _, ch := range chunks {
					var vec []float64
					if v, err := emb.Embed(ch.EmbedText); err == nil {
						vec = v
					}
					wikiChunks = append(wikiChunks, surreal.WikiChunk{
						ParentPath: path,
						Heading:    ch.Heading,
						ChunkIndex: ch.Index,
						Text:       ch.Text,
						Embedding:  vec,
					})
				}
				if err := db.UpsertWikiChunks(wikiChunks); err != nil {
					log.Printf("WARN: upsert chunks for %s: %v", path, err)
				} else {
					chunked++
				}
			}
		}
		log.Printf("INFO: rechunk complete for %s chunked=%d", gid, chunked)
		db.Close()
	}
	return nil
}

// ── Wiki chunking ─────────────────────────────────────────────────────────────

type wikiChunk struct {
	Heading   string
	Index     int
	Text      string
	EmbedText string
}

func chunkWikiBody(title, body string) []wikiChunk {
	lines := strings.Split(body, "\n")
	var chunks []wikiChunk
	var currentHeading string
	var currentLines []string
	chunkIndex := 0

	flush := func() {
		text := strings.TrimSpace(strings.Join(currentLines, "\n"))
		if text == "" {
			return
		}
		embedText := title + "\n" + currentHeading + "\n" + text
		chunks = append(chunks, wikiChunk{
			Heading:   currentHeading,
			Index:     chunkIndex,
			Text:      text,
			EmbedText: embedText,
		})
		chunkIndex++
		currentLines = nil
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "# ") {
			flush()
			currentHeading = strings.TrimLeft(line, "#")
			currentHeading = strings.TrimSpace(currentHeading)
		} else {
			currentLines = append(currentLines, line)
		}
	}
	flush()
	return chunks
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// readWikiCommitSentinel reads the .wiki_commit_ts sentinel written by the
// curator after a successful push. Returns (true, timestamp) if the sentinel
// exists and is readable, (false, "") otherwise.
func readWikiCommitSentinel(vignoble, groupID string) (bool, string) {
	base := os.Getenv("VIGNOBLES_BASE_DIR")
	if base == "" {
		base = os.Getenv("WIKI_CLONE_DIR")
	}
	if base == "" {
		return false, ""
	}
	// Try vignoble-<name> then bare name.
	var sentinelPath string
	for _, cand := range []string{
		filepath.Join(base, "vignoble-"+vignoble, "wiki", groupID, ".wiki_commit_ts"),
		filepath.Join(base, vignoble, "wiki", groupID, ".wiki_commit_ts"),
	} {
		if _, err := os.Stat(cand); err == nil {
			sentinelPath = cand
			break
		}
	}
	if sentinelPath == "" {
		// Also try VIGNOBLE_DIR (single-vignoble mode).
		if dir := os.Getenv("VIGNOBLE_DIR"); dir != "" {
			cand := filepath.Join(dir, "wiki", groupID, ".wiki_commit_ts")
			if _, err := os.Stat(cand); err == nil {
				sentinelPath = cand
			}
		}
	}
	if sentinelPath == "" {
		return false, ""
	}
	raw, err := os.ReadFile(sentinelPath)
	if err != nil {
		return false, ""
	}
	timestamp := strings.TrimSpace(string(raw))
	return true, timestamp
}

// resolveGroupIDsForVignoble returns group_ids scoped to a single vignoble
// (for the status view). When VIGNOBLES_BASE_DIR is set it reads only
// $VIGNOBLES_BASE_DIR/<vignoble>/vignes.yaml rather than scanning all subdirs.
func resolveGroupIDsForVignoble(vignoble string) []string {
	// Scope to a single vignoble's vignes for the status view. Clone subdirs under
	// VIGNOBLES_BASE_DIR are named "vignoble-<name>" (also accept the bare name),
	// and a directly mounted VIGNOBLE_DIR is only used when it holds a vignes.yaml.
	var vignes []string
	if base := os.Getenv("VIGNOBLES_BASE_DIR"); base != "" {
		for _, cand := range []string{
			filepath.Join(base, "vignoble-"+vignoble, "vignes.yaml"),
			filepath.Join(base, vignoble, "vignes.yaml"),
		} {
			if ids := groupIDsFromVignesYAML(cand); len(ids) > 0 {
				vignes = ids
				break
			}
		}
	}
	if len(vignes) == 0 {
		if dir := os.Getenv("VIGNOBLE_DIR"); dir != "" {
			vignes = groupIDsFromVignesYAML(filepath.Join(dir, "vignes.yaml"))
		}
	}
	// Include the régisseur group (<vignoble>) + its vignes. Exclude the
	// vignoble-<vignoble> ROLLUP scope: it isn't ingested from engram (populated by
	// the rollup engine), so it has no ingest cursor and would show a bogus lag.
	out := []string{vignoble}
	seen := map[string]bool{vignoble: true}
	for _, g := range vignes {
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// resolveGroupIDs returns the list of group_ids for the ingestion path
// (non-postgres HTTP source). For the postgres source, auto-discovery via
// DiscoverProjects is used instead (see discoverSourceProjects).
//
// Resolution order:
//  1. $VIGNOBLE_DIR/vignes.yaml — if the file exists, all vigne keys become group_ids.
//  2. $VIGNOBLES_BASE_DIR — parent dir of per-vignoble clones; each subdir with a
//     vignes.yaml contributes its vigne keys.
//  3. Fall back to the vignoble name (single group, original behaviour).
func resolveGroupIDs(vignoble string) []string {
	// 1. Single vignes.yaml mounted at VIGNOBLE_DIR.
	if dir := os.Getenv("VIGNOBLE_DIR"); dir != "" {
		if ids := groupIDsFromVignesYAML(filepath.Join(dir, "vignes.yaml")); len(ids) > 0 {
			return ids
		}
	}

	// 2. Scan VIGNOBLES_BASE_DIR for per-vignoble vignes.yaml files.
	if base := os.Getenv("VIGNOBLES_BASE_DIR"); base != "" {
		var ids []string
		entries, err := os.ReadDir(base)
		if err != nil {
			log.Printf("WARN: resolveGroupIDs: read VIGNOBLES_BASE_DIR %s: %v", base, err)
		} else {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				candidatePath := filepath.Join(base, e.Name(), "vignes.yaml")
				ids = append(ids, groupIDsFromVignesYAML(candidatePath)...)
			}
		}
		if len(ids) > 0 {
			return ids
		}
	}

	// 3. Single-vignoble fallback.
	return []string{vignoble}
}

// groupIDsFromVignesYAML parses a vignes.yaml file and returns the vigne keys as group_ids.
// Returns nil if the file does not exist or cannot be parsed.
func groupIDsFromVignesYAML(path string) []string {
	cfg, err := config.LoadVignoble(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: groupIDsFromVignesYAML: parse %s: %v", path, err)
		}
		return nil
	}
	var ids []string
	for name := range cfg.Vignes {
		ids = append(ids, name)
	}
	return ids
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func setupLogging() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)
}
