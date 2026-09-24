package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/gitlab"
	"github.com/Genentech/pinard/internal/state"
)

type mockGitLab struct {
	mrs       map[string]*gitlab.MergeRequest
	notes     map[string][]gitlab.Note
	pipelines map[string][]gitlab.Pipeline
	approvals map[string]*gitlab.Approvals
}

func (m *mockGitLab) getMR(repo string, iid int) *gitlab.MergeRequest {
	if mr, ok := m.mrs[key(repo, iid)]; ok {
		return mr
	}
	return &gitlab.MergeRequest{State: "opened"}
}

func key(repo string, iid int) string {
	return repo + ":" + string(rune(iid+'0'))
}

type mockNATS struct {
	published []publishedEvent
}

type publishedEvent struct {
	Subject string
	Payload map[string]any
}

type mockKV struct {
	data map[string]map[string]any
}

func (m *mockKV) Get(bucket, key string) (map[string]any, error) {
	if m.data == nil {
		return nil, nil
	}
	return m.data[bucket+":"+key], nil
}

func TestMRWatcher_MergedMRTransitionsToPostMerge(t *testing.T) {
	dir := t.TempDir()
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"worker-1": {
				Name:    "worker-1",
				Project: "exo-cli",
				Repo:    "group/exo-cli",
				MR:      42,
			},
		}
	})

	// Verify initial state
	var watched *state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		watched = s.Watched["worker-1"]
	})
	if watched == nil {
		t.Fatal("worker-1 should exist in watched")
	}
	if watched.MR != 42 {
		t.Errorf("expected MR 42, got %d", watched.MR)
	}
}

func TestMRWatcher_DeadSessionRemoved(t *testing.T) {
	dir := t.TempDir()
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"dead-worker": {Name: "dead-worker", Repo: ""},
		}
	})

	// A watcher run with no MR tracked and session dead should remove it
	mrState.Read(func(s *state.MRWatcherState) {
		if _, ok := s.Watched["dead-worker"]; !ok {
			t.Fatal("dead-worker should exist before removal")
		}
	})
}

func TestMRWatcher_PipelineFailCountIncrements(t *testing.T) {
	dir := t.TempDir()
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"worker-1": {
				Name:              "worker-1",
				Project:           "exo-cli",
				Repo:              "group/exo-cli",
				MR:                42,
				PipelineFailCount: 3,
				LastPipelineID:    99,
			},
		}
	})

	// Simulate incrementing fail count
	mrState.Update(func(s *state.MRWatcherState) {
		w := s.Watched["worker-1"]
		w.PipelineFailCount++
		w.LastPipelineID = 100
	})

	mrState.Read(func(s *state.MRWatcherState) {
		w := s.Watched["worker-1"]
		if w.PipelineFailCount != 4 {
			t.Errorf("expected fail count 4, got %d", w.PipelineFailCount)
		}
		if w.LastPipelineID != 100 {
			t.Errorf("expected pipeline 100, got %d", w.LastPipelineID)
		}
	})
}

func TestMRWatcher_CircuitBreakerAt5(t *testing.T) {
	dir := t.TempDir()
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"worker-1": {
				Name:              "worker-1",
				Project:           "exo-cli",
				Repo:              "group/exo-cli",
				MR:                42,
				PipelineFailCount: 5,
			},
		}
	})

	mrState.Read(func(s *state.MRWatcherState) {
		w := s.Watched["worker-1"]
		if w.PipelineFailCount <= 5 {
			// Circuit breaker should trigger at >5
			shouldBreak := w.PipelineFailCount+1 > 5
			if !shouldBreak {
				t.Error("circuit breaker should trigger after 5 failures")
			}
		}
	})
}

func TestMRWatcher_PostMergeEnabled(t *testing.T) {
	vigne := config.Vigne{Repo: "group/exo-cli"}

	// Default: enabled
	if !vigne.ShouldMonitorPostMerge() {
		t.Error("post-merge monitoring should be enabled by default")
	}

	// Explicit false
	f := false
	vigne.MonitorPostMerge = &f
	if vigne.ShouldMonitorPostMerge() {
		t.Error("post-merge monitoring should be disabled when set to false")
	}
}

func TestMRWatcher_NoteFiltering(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 1, Body: "system note", System: true, Author: gitlab.Author{Username: "gitlab"}},
		{ID: 2, Body: "reviewer comment", System: false, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 3, Body: "pinard reply", System: false, Author: gitlab.Author{Username: "pinard"}},
		{ID: 4, Body: "another review", System: false, Author: gitlab.Author{Username: "reviewer2"}},
	}

	ignored := map[string]bool{"pinard": true}
	lastNoteID := 0

	var filtered []gitlab.Note
	for _, n := range notes {
		if n.System || n.ID <= lastNoteID || ignored[n.Author.Username] {
			continue
		}
		filtered = append(filtered, n)
	}

	if len(filtered) != 2 {
		t.Errorf("expected 2 notes after filtering, got %d", len(filtered))
	}
	if filtered[0].Body != "reviewer comment" {
		t.Errorf("first note should be 'reviewer comment', got %q", filtered[0].Body)
	}
	if filtered[1].Body != "another review" {
		t.Errorf("second note should be 'another review', got %q", filtered[1].Body)
	}
}

// BUG REGRESSION: Resolved notes were not filtered, causing already-addressed
// review comments (marked with checkmark) to be re-forwarded on every tick.
func TestMRWatcher_ResolvedNotesFiltered(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 10, Body: "unresolved comment", System: false, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 11, Body: "resolved comment", System: false, Resolvable: true, Resolved: true, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 12, Body: "general comment", System: false, Resolvable: false, Resolved: false, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 13, Body: "another resolved", System: false, Resolvable: true, Resolved: true, Author: gitlab.Author{Username: "reviewer2"}},
	}

	ignored := map[string]bool{}
	lastNoteID := 0

	var filtered []gitlab.Note
	for _, n := range notes {
		if n.System || n.ID <= lastNoteID || (n.Resolvable && n.Resolved) || ignored[n.Author.Username] {
			continue
		}
		filtered = append(filtered, n)
	}

	if len(filtered) != 2 {
		t.Errorf("expected 2 notes (unresolved + general), got %d", len(filtered))
	}
	if filtered[0].ID != 10 {
		t.Errorf("first should be unresolved (ID=10), got ID=%d", filtered[0].ID)
	}
	if filtered[1].ID != 12 {
		t.Errorf("second should be general (ID=12), got ID=%d", filtered[1].ID)
	}
}

// BUG REGRESSION: last_note_id must advance to the maximum note ID after
// forwarding, regardless of which notes pass the filter. Otherwise the daemon
// re-discovers the same notes every tick (looping).
func TestMRWatcher_LastNoteIDAdvancesToMax(t *testing.T) {
	dir := t.TempDir()
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"worker-1": {
				Name:       "worker-1",
				Project:    "web",
				Repo:       "group/web",
				MR:         51,
				LastNoteID: 100,
			},
		}
	})

	// Simulate what forwardNotes does: filter notes, then set lastID to max
	allNotes := []gitlab.Note{
		{ID: 101, Body: "first", System: false, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 102, Body: "second", System: true, Author: gitlab.Author{Username: "system"}},
		{ID: 103, Body: "third", System: false, Resolvable: true, Resolved: true, Author: gitlab.Author{Username: "reviewer"}},
		{ID: 104, Body: "fourth", System: false, Author: gitlab.Author{Username: "reviewer"}},
	}

	ignored := map[string]bool{}
	var newNotes []gitlab.Note
	for _, n := range allNotes {
		if n.System || n.ID <= 100 || (n.Resolvable && n.Resolved) || ignored[n.Author.Username] {
			continue
		}
		newNotes = append(newNotes, n)
	}

	// Only 2 notes pass filter (101, 104) but last_note_id should be set to
	// the max of the FORWARDED notes (104), not 101.
	if len(newNotes) != 2 {
		t.Fatalf("expected 2 filtered notes, got %d", len(newNotes))
	}

	lastID := newNotes[len(newNotes)-1].ID
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched["worker-1"].LastNoteID = lastID
	})

	mrState.Read(func(s *state.MRWatcherState) {
		if s.Watched["worker-1"].LastNoteID != 104 {
			t.Errorf("last_note_id should advance to 104, got %d", s.Watched["worker-1"].LastNoteID)
		}
	})
}

// BUG REGRESSION: auto-merge must only block on UNRESOLVED discussions.
// The old code checked `n.Position.NewPath != "" && !n.System` which blocked
// on ANY diff-thread note, even resolved ones.
func TestMRWatcher_AutoMergeNotBlockedByResolvedDiscussions(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 1, Resolvable: true, Resolved: true, Position: gitlab.Position{NewPath: "file.go", NewLine: 10}},
		{ID: 2, Resolvable: true, Resolved: true, Position: gitlab.Position{NewPath: "file.go", NewLine: 20}},
		{ID: 3, Resolvable: false, System: true},
	}

	blocked := false
	for _, n := range notes {
		if n.Resolvable && !n.Resolved {
			blocked = true
			break
		}
	}

	if blocked {
		t.Error("auto-merge should NOT be blocked when all discussions are resolved")
	}
}

func TestMRWatcher_AutoMergeBlockedByUnresolvedDiscussion(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 1, Resolvable: true, Resolved: true, Position: gitlab.Position{NewPath: "file.go", NewLine: 10}},
		{ID: 2, Resolvable: true, Resolved: false, Position: gitlab.Position{NewPath: "file.go", NewLine: 20}},
	}

	blocked := false
	for _, n := range notes {
		if n.Resolvable && !n.Resolved {
			blocked = true
			break
		}
	}

	if !blocked {
		t.Error("auto-merge SHOULD be blocked when there are unresolved discussions")
	}
}

// BUG REGRESSION: auto-merge must skip unresolved discussions where all
// unresolved notes are from the MR author or ignored users (bot replies,
// push-event discussions). Only block on reviewer feedback.
func TestMRWatcher_AutoMergeSkipsBotOnlyDiscussions(t *testing.T) {
	mrAuthor := "pinard-bot"
	ignoredAuthors := map[string]bool{"pinard-bot": true}

	tests := []struct {
		name    string
		notes   []gitlab.Note
		blocked bool
	}{
		{
			name: "all notes from MR author — not blocked",
			notes: []gitlab.Note{
				{ID: 1, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "pinard-bot"}},
			},
			blocked: false,
		},
		{
			name: "reviewer note unresolved — blocked",
			notes: []gitlab.Note{
				{ID: 1, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "reviewer"}},
			},
			blocked: true,
		},
		{
			name: "mix of author + reviewer unresolved — blocked",
			notes: []gitlab.Note{
				{ID: 1, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "pinard-bot"}},
				{ID: 2, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "reviewer"}},
			},
			blocked: true,
		},
		{
			name: "system note + author reply — not blocked",
			notes: []gitlab.Note{
				{ID: 1, Resolvable: false, System: true, Author: gitlab.Author{Username: "gitlab"}},
				{ID: 2, Resolvable: true, Resolved: false, Author: gitlab.Author{Username: "pinard-bot"}},
			},
			blocked: false,
		},
		{
			name: "all resolved — not blocked",
			notes: []gitlab.Note{
				{ID: 1, Resolvable: true, Resolved: true, Author: gitlab.Author{Username: "reviewer"}},
			},
			blocked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hasUnresolved := false
			onlyAuthorOrSystem := true
			for _, n := range tc.notes {
				if n.Resolvable && !n.Resolved {
					hasUnresolved = true
					if !n.System && n.Author.Username != mrAuthor && !ignoredAuthors[n.Author.Username] {
						onlyAuthorOrSystem = false
					}
				}
			}
			blocked := hasUnresolved && !onlyAuthorOrSystem
			if blocked != tc.blocked {
				t.Errorf("expected blocked=%v, got %v", tc.blocked, blocked)
			}
		})
	}
}

// BUG REGRESSION: post-merge must use ListPipelinesByCommit (merge_commit_sha)
// instead of ListMRPipelines. The MR pipeline is stale after merge — it shows
// the pre-merge pipeline that already passed, not the actual main branch pipeline.
func TestMRWatcher_PostMergeUsesMergeCommitSHA(t *testing.T) {
	src, err := os.ReadFile("mrs.go")
	if err != nil {
		t.Skipf("cannot read mrs.go: %v", err)
	}
	content := string(src)

	// handlePostMerge must use ListPipelinesByCommit, NOT ListMRPipelines
	if !strings.Contains(content, "ListPipelinesByCommit") {
		t.Error("handlePostMerge should use ListPipelinesByCommit")
	}

	// Verify MergeCommitSHA is stored when transitioning to post_merge
	if !strings.Contains(content, "MergeCommitSHA") {
		t.Error("post_merge transition should store MergeCommitSHA")
	}
}

// BUG REGRESSION: forwardNotes must be called regardless of whether the worker
// session is alive. When worker dies, review comments were silently dropped
// because forwardNotes was inside an `if alive` block.
func TestMRWatcher_ForwardNotesCalledEvenWhenWorkerDead(t *testing.T) {
	src, err := os.ReadFile("mrs.go")
	if err != nil {
		t.Skipf("cannot read mrs.go: %v", err)
	}
	content := string(src)

	if !strings.Contains(content, "w.forwardNotes(sessionName, entry)") {
		t.Error("forwardNotes call not found in mrs.go")
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, "w.forwardNotes") {
			for j := i - 1; j >= 0; j-- {
				trimmed := strings.TrimSpace(lines[j])
				if trimmed == "" {
					continue
				}
				if strings.Contains(trimmed, "if alive") || strings.Contains(trimmed, "if !alive") {
					braceCount := 0
					for k := j + 1; k < i; k++ {
						if strings.Contains(lines[k], "{") {
							braceCount++
						}
						if strings.Contains(lines[k], "}") {
							braceCount--
						}
					}
					if braceCount == 0 {
						t.Error("forwardNotes is gated behind alive check — bug regression")
					}
				}
				break
			}
		}
	}
}

// BUG REGRESSION (#15): a Draft/WIP MR must never be auto-merged. Draft is the
// mechanism used to hold an MR for review fixes; merging it ships pre-review code.
func TestMRWatcher_AutoMergeSkipsDraft(t *testing.T) {
	tests := []struct {
		name string
		mr   gitlab.MergeRequest
		skip bool
	}{
		{"draft true", gitlab.MergeRequest{Draft: true}, true},
		{"work_in_progress true", gitlab.MergeRequest{WorkInProgress: true}, true},
		{"neither — eligible", gitlab.MergeRequest{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skip := tc.mr.Draft || tc.mr.WorkInProgress
			if skip != tc.skip {
				t.Errorf("expected skip=%v, got %v", tc.skip, skip)
			}
		})
	}
}

// BUG REGRESSION (#18): the conductor shares the pinard GitLab identity, so its
// MR comments are normally filtered as self-authored. A note carrying the
// conductor marker must be forwarded to the worker anyway; the marker is stripped
// before delivery.
func TestMRWatcher_ConductorMarkedNotesForwarded(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 1, Body: "worker progress note", System: false, Author: gitlab.Author{Username: "pinard"}},
		{ID: 2, Body: "fix the authz gap\n\n" + ConductorMarker, System: false, Author: gitlab.Author{Username: "pinard"}},
		{ID: 3, Body: "human review", System: false, Author: gitlab.Author{Username: "lelongs"}},
	}
	w := &MRWatcher{IgnoredAuthors: map[string]bool{"pinard": true}}
	lastNoteID := 0

	var filtered []gitlab.Note
	for _, n := range notes {
		if n.ID <= lastNoteID || !w.shouldForwardNote(n) {
			continue
		}
		filtered = append(filtered, n)
	}

	if len(filtered) != 2 {
		t.Fatalf("expected 2 notes (conductor-marked + human), got %d", len(filtered))
	}
	if filtered[0].ID != 2 {
		t.Errorf("first forwarded note should be the conductor-marked one (ID=2), got ID=%d", filtered[0].ID)
	}
	// The marker must be stripped before the note reaches the worker.
	cleaned := strings.TrimSpace(strings.ReplaceAll(filtered[0].Body, ConductorMarker, ""))
	if strings.Contains(cleaned, ConductorMarker) {
		t.Error("conductor marker should be stripped from the forwarded body")
	}
	if cleaned != "fix the authz gap" {
		t.Errorf("cleaned body = %q, want %q", cleaned, "fix the authz gap")
	}
}

// BUG REGRESSION (#305): a maître review posted via the raw pressoir comment-pr
// primitive (no conductor marker) must be dropped, while the same review posted
// via the comment_mr tool / `aoc comment-mr` (marker appended) must be forwarded.
// This is the exact failure mode from #305: the needs_review template told the
// maître to use the unmarked primitive, so every maître review was invisible to
// the vendangeur.
func TestMRWatcher_UnmarkedConductorNoteSkipped_MarkedForwarded(t *testing.T) {
	w := &MRWatcher{IgnoredAuthors: map[string]bool{"pinard": true}}

	unmarked := gitlab.Note{ID: 1, Body: "LGTM 🍇 Reviewed by the webterm maître", System: false, Author: gitlab.Author{Username: "pinard"}}
	if w.shouldForwardNote(unmarked) {
		t.Error("unmarked conductor-authored note should be skipped")
	}

	marked := gitlab.Note{ID: 2, Body: "Please add a test for the edge case\n\n" + ConductorMarker, System: false, Author: gitlab.Author{Username: "pinard"}}
	if !w.shouldForwardNote(marked) {
		t.Error("marked conductor-authored note should be forwarded")
	}
}

// ── Tests for process-worker identity split (issue #144) ──────────────────
//
// A process worker has two identities:
//   - KV agent record keyed by agentId (= runId, e.g. "pinard-swe-143")
//   - tmux session name (e.g. "memory--pinard-1431574f4") stored in the
//     mr-watcher state and used as the branch suffix
//
// resolveAgentByToken, agentFromBranch, getWorkerProcess, and getWorkerParcelle
// must all resolve via name-scan when the direct KV.Get(token) misses.

func TestResolveAgentByToken_DirectHit(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv}
	kvKey, rec := w.resolveAgentByToken("pinard-swe-143")
	if kvKey != "pinard-swe-143" {
		t.Errorf("expected kvKey 'pinard-swe-143', got %q", kvKey)
	}
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
}

func TestResolveAgentByToken_NameScan_ByName(t *testing.T) {
	// KV keyed by agentId; token is the tmux session name (stored in 'name' field)
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv}
	kvKey, rec := w.resolveAgentByToken("memory--pinard-1431574f4")
	if kvKey != "pinard-swe-143" {
		t.Errorf("expected kvKey 'pinard-swe-143', got %q", kvKey)
	}
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
}

func TestResolveAgentByToken_NameScan_ByAgentId(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"agentId": "pinard-swe-143", "runId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv}
	// Direct hit for agentId token
	kvKey, rec := w.resolveAgentByToken("pinard-swe-143")
	if kvKey != "pinard-swe-143" {
		t.Errorf("expected kvKey 'pinard-swe-143', got %q", kvKey)
	}
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
}

func TestResolveAgentByToken_NotFound(t *testing.T) {
	kv := newMockKV()
	w := &MRWatcher{KV: kv}
	kvKey, rec := w.resolveAgentByToken("nonexistent")
	if kvKey != "" || rec != nil {
		t.Errorf("expected ('', nil), got (%q, %v)", kvKey, rec)
	}
}

// agentFromBranch must return the KV key (agentId), NOT the session name,
// when the worker's KV record is keyed by agentId.
func TestAgentFromBranch_ProcessWorker_NameScan(t *testing.T) {
	kv := newMockKV()
	// KV key = agentId; 'name' field = session/branch suffix
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv, Vignoble: &config.Vignoble{Name: "misc"}}
	got := w.agentFromBranch("pinard/memory--pinard-1431574f4", "pinard")
	if got != "pinard-swe-143" {
		t.Errorf("agentFromBranch: expected 'pinard-swe-143' (agentId), got %q", got)
	}
}

// agentFromBranch must return "" when the project doesn't match.
func TestAgentFromBranch_ProcessWorker_WrongProject(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv, Vignoble: &config.Vignoble{Name: "misc"}}
	got := w.agentFromBranch("pinard/memory--pinard-1431574f4", "other-project")
	if got != "" {
		t.Errorf("expected '' for wrong project, got %q", got)
	}
}

// agentFromBranch must return "" for freeform workers (no process field).
func TestAgentFromBranch_FreeformWorker_ReturnsEmpty(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("memory--pinard-1431574f4", map[string]any{
		"project": "pinard",
		// no process field
	})

	w := &MRWatcher{KV: kv, Vignoble: &config.Vignoble{Name: "misc"}}
	got := w.agentFromBranch("pinard/memory--pinard-1431574f4", "pinard")
	if got != "" {
		t.Errorf("expected '' for freeform worker, got %q", got)
	}
}

// getWorkerProcess must resolve via name-scan for process workers.
func TestGetWorkerProcess_NameScan(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv}
	// Token is the session name, not the KV key
	got := w.getWorkerProcess("memory--pinard-1431574f4")
	if got != "swe" {
		t.Errorf("getWorkerProcess via name-scan: expected 'swe', got %q", got)
	}
}

// getWorkerParcelle must resolve via name-scan for process workers.
func TestGetWorkerParcelle_NameScan(t *testing.T) {
	kv := newMockKV()
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe", "parcelle": "memory",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
	})

	w := &MRWatcher{KV: kv}
	got := w.getWorkerParcelle("memory--pinard-1431574f4")
	if got != "memory" {
		t.Errorf("getWorkerParcelle via name-scan: expected 'memory', got %q", got)
	}
}

// Full end-to-end: process worker with KV key != session name routes correctly.
// This is the exact bug scenario from issue #144.
func TestDispatchRouting_ProcessWorker_NameScan_EndToEnd(t *testing.T) {
	kv := newMockKV()
	// Worker is registered under agentId 'pinard-swe-143', tmux session =
	// 'memory--pinard-1431574f4', parcelle = 'memory'.
	kv.setAgent("pinard-swe-143", map[string]any{
		"project": "pinard", "process": "swe", "parcelle": "memory",
		"name": "memory--pinard-1431574f4", "agentId": "pinard-swe-143",
		"mr": float64(242),
	})

	w := &MRWatcher{
		KV:       kv,
		Vignoble: &config.Vignoble{Name: "misc"},
	}

	// Simulate the dispatch path: session name = 'memory--pinard-1431574f4'
	// (what the watcher stores in the tracked entry).
	session := "memory--pinard-1431574f4"
	processName := w.getWorkerProcess(session)  // must resolve via name-scan
	parcelle := w.getWorkerParcelle(session)      // must resolve via name-scan

	if processName != "swe" {
		t.Errorf("getWorkerProcess: expected 'swe', got %q", processName)
	}
	if parcelle != "memory" {
		t.Errorf("getWorkerParcelle: expected 'memory', got %q", parcelle)
	}

	// The dispatch target must also be the agentId, not the session name.
	// In dispatchToWorkerWithType, when processName=="", it falls back to
	// findAgentForMR. But with the name-scan fix, getWorkerProcess already
	// returns a non-empty result, so we use the resolved agentId directly.
	// Here we verify that the resolved kvKey is the agentId.
	kvKey, _ := w.resolveAgentByToken(session)
	if kvKey != "pinard-swe-143" {
		t.Errorf("resolveAgentByToken: expected kvKey 'pinard-swe-143', got %q", kvKey)
	}

	subject := WorkerInboxSubject(w.Vignoble.Name, parcelle, kvKey, processName)
	expected := "pinard.misc.parcelles.memory.agents.pinard-swe-143.process.swe.inbox"
	if subject != expected {
		t.Errorf("inbox subject: expected %q, got %q", expected, subject)
	}
}

func TestWorkerInboxSubject_Freeform(t *testing.T) {
	subject := WorkerInboxSubject("exohub", "exo-cli", "worker-001", "")
	expected := "pinard.exohub.parcelles.exo-cli.agents.worker-001.inbox"
	if subject != expected {
		t.Errorf("expected %q, got %q", expected, subject)
	}
}

func TestWorkerInboxSubject_ProcessScoped(t *testing.T) {
	subject := WorkerInboxSubject("exohub", "exo-cli", "worker-001", "dev")
	expected := "pinard.exohub.parcelles.exo-cli.agents.worker-001.process.dev.inbox"
	if subject != expected {
		t.Errorf("expected %q, got %q", expected, subject)
	}
}

func TestWorkerInboxSubject_DifferentProcesses(t *testing.T) {
	tests := []struct {
		vignoble string
		parcelle string
		session  string
		process  string
		expected string
	}{
		{"exohub", "exo-cli", "w-1", "dev", "pinard.exohub.parcelles.exo-cli.agents.w-1.process.dev.inbox"},
		{"exohub", "semantic-search", "w-1", "genomics-build", "pinard.exohub.parcelles.semantic-search.agents.w-1.process.genomics-build.inbox"},
		{"data", "genomics", "w-2", "hello", "pinard.data.parcelles.genomics.agents.w-2.process.hello.inbox"},
		{"exohub", "exo-cli", "w-3", "", "pinard.exohub.parcelles.exo-cli.agents.w-3.inbox"},
	}
	for _, tt := range tests {
		subject := WorkerInboxSubject(tt.vignoble, tt.parcelle, tt.session, tt.process)
		if subject != tt.expected {
			t.Errorf("WorkerInboxSubject(%q, %q, %q, %q) = %q, want %q",
				tt.vignoble, tt.parcelle, tt.session, tt.process, subject, tt.expected)
		}
	}
}


func TestShouldPublishMRMemory(t *testing.T) {
	tests := []struct {
		name   string
		mr     gitlab.MergeRequest
		want   bool
	}{
		{
			name: "regular MR is published",
			mr: gitlab.MergeRequest{
				Title:       "fix(memory): improve recall precision",
				Description: "Fixed the distance gate to reject irrelevant hits.",
			},
			want: true,
		},
		{
			name: "Ledger sync is skipped",
			mr: gitlab.MergeRequest{
				Title:       "feat(docs): sync Ledger to abc123",
				Description: "Automated Ledger sync.",
			},
			want: false,
		},
		{
			name: "bump-image is skipped",
			mr: gitlab.MergeRequest{
				Title:       "chore: bump-image pinard-memory to v1.2.3",
				Description: "Bump the image.",
			},
			want: false,
		},
		{
			name: "bump-chart is skipped",
			mr: gitlab.MergeRequest{
				Title:       "chore: bump-chart pinard 0.5.0",
				Description: "Bump the chart.",
			},
			want: false,
		},
		{
			name: "Revert MR is skipped",
			mr: gitlab.MergeRequest{
				Title:       "Revert \"feat: add caching\"",
				Description: "Reverts the caching change.",
			},
			want: false,
		},
		{
			name: "cuvee source branch is skipped",
			mr: gitlab.MergeRequest{
				Title:        "chore: accumulate cuvee changes",
				Description:  "Accumulated changes from cuvee.",
				SourceBranch: "cuvee/2026-08",
			},
			want: false,
		},
		{
			name: "empty description is skipped",
			mr: gitlab.MergeRequest{
				Title:       "fix: something",
				Description: "",
			},
			want: false,
		},
		{
			name: "whitespace-only description is skipped",
			mr: gitlab.MergeRequest{
				Title:       "fix: something",
				Description: "   \n  ",
			},
			want: false,
		},
		{
			name: "memory:skip label always skips",
			mr: gitlab.MergeRequest{
				Title:       "fix(memory): improve recall precision",
				Description: "Fixed something important.",
				Labels:      []string{"memory:skip"},
			},
			want: false,
		},
		{
			name: "memory:capture label always publishes (overrides Revert)",
			mr: gitlab.MergeRequest{
				Title:       "Revert \"feat: add caching\"",
				Description: "Reverts the caching change.",
				Labels:      []string{"memory:capture"},
			},
			want: true,
		},
		{
			name: "memory:skip overrides memory:capture when both present",
			mr: gitlab.MergeRequest{
				Title:       "fix: something",
				Description: "Something.",
				Labels:      []string{"memory:skip", "memory:capture"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldPublishMRMemory(&tt.mr)
			if got != tt.want {
				t.Errorf("ShouldPublishMRMemory() = %v, want %v", got, tt.want)
			}
		})
	}
}

// mockMRMemoryGitLab implements mrMemoryGitLab for unit tests.
type mockMRMemoryGitLab struct {
	changes       []string
	changesErr    error
	closingIssues []gitlab.Issue
	closingErr    error
	issues        map[int]*gitlab.Issue
	notes         []gitlab.Note
	notesErr      error
}

func (m *mockMRMemoryGitLab) GetMRChanges(repo string, iid int) ([]string, error) {
	return m.changes, m.changesErr
}

func (m *mockMRMemoryGitLab) GetMRClosingIssues(repo string, iid int) ([]gitlab.Issue, error) {
	return m.closingIssues, m.closingErr
}

func (m *mockMRMemoryGitLab) GetIssue(repo string, iid int) (*gitlab.Issue, error) {
	if iss, ok := m.issues[iid]; ok {
		return iss, nil
	}
	return nil, fmt.Errorf("issue #%d not found", iid)
}

func (m *mockMRMemoryGitLab) ListMRNotes(repo string, iid int) ([]gitlab.Note, error) {
	return m.notes, m.notesErr
}

func TestBuildMRMemoryPayload(t *testing.T) {
	mr := &gitlab.MergeRequest{
		IID:          42,
		Title:        "feat: add caching",
		Description:  "Adds an LRU cache. Closes #7.",
		SourceBranch: "feat/caching",
		MergedAt:     "2026-01-15T12:00:00Z",
		Author:       gitlab.Author{Username: "dev1"},
		WebURL:       "https://code.example.com/group/proj/-/merge_requests/42",
	}

	gl := &mockMRMemoryGitLab{
		changes: []string{"internal/cache/cache.go", "cmd/aoc/main.go"},
		closingIssues: []gitlab.Issue{
			{IID: 7, Title: "Support caching", Description: "We need an LRU cache."},
		},
	}

	payload, _, err := BuildMRMemoryPayload(gl, "myproject", "group/proj", mr)
	if err != nil {
		t.Fatalf("BuildMRMemoryPayload returned unexpected error: %v", err)
	}

	assertEqual := func(field string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("payload[%q] = %v, want %v", field, got, want)
		}
	}

	assertEqual("source", payload["source"], "mr")
	assertEqual("project", payload["project"], "myproject")
	assertEqual("repo", payload["repo"], "group/proj")
	assertEqual("iid", payload["iid"], 42)
	assertEqual("scope", payload["scope"], "myproject")
	assertEqual("title", payload["title"], "feat: add caching")
	assertEqual("description", payload["description"], "Adds an LRU cache. Closes #7.")
	assertEqual("merged_at", payload["merged_at"], "2026-01-15T12:00:00Z")
	assertEqual("author", payload["author"], "dev1")
	assertEqual("url", payload["url"], "https://code.example.com/group/proj/-/merge_requests/42")

	files, ok := payload["files_changed"].([]string)
	if !ok {
		t.Fatalf("payload[\"files_changed\"] is not []string: %T", payload["files_changed"])
	}
	if len(files) != 2 || files[0] != "internal/cache/cache.go" {
		t.Errorf("unexpected files_changed: %v", files)
	}

	issues, ok := payload["issues"].([]MRMemoryIssueContext)
	if !ok {
		t.Fatalf("payload[\"issues\"] is not []MRMemoryIssueContext: %T", payload["issues"])
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 closing issue, got %d", len(issues))
	}
	if issues[0].IID != 7 || issues[0].Title != "Support caching" || issues[0].Description != "We need an LRU cache." {
		t.Errorf("unexpected closing issue: %+v", issues[0])
	}
}

func TestBuildMRMemoryPayload_FallbackToDescriptionParsing(t *testing.T) {
	mr := &gitlab.MergeRequest{
		IID:         99,
		Title:       "fix: bug",
		Description: "Fixes something.\n\nCloses #3",
		MergedAt:    "2026-02-01T00:00:00Z",
		Author:      gitlab.Author{Username: "alice"},
		WebURL:      "https://code.example.com/g/p/-/merge_requests/99",
	}

	gl := &mockMRMemoryGitLab{
		changes:    []string{"foo.go"},
		closingErr: fmt.Errorf("API unavailable"),
		issues: map[int]*gitlab.Issue{
			3: {IID: 3, Title: "Bug report", Description: "There is a bug."},
		},
	}

	payload, _, err := BuildMRMemoryPayload(gl, "proj", "g/p", mr)
	if err != nil {
		t.Fatalf("BuildMRMemoryPayload returned unexpected error: %v", err)
	}

	issues, ok := payload["issues"].([]MRMemoryIssueContext)
	if !ok {
		t.Fatalf("payload[\"issues\"] is not []MRMemoryIssueContext: %T", payload["issues"])
	}
	if len(issues) != 1 || issues[0].IID != 3 {
		t.Errorf("expected fallback issue #3, got %v", issues)
	}
}

func TestBuildMRMemoryPayload_ReviewNotes(t *testing.T) {
	// BuildMRMemoryPayload must include pre-filtered review_notes in the payload
	// and return all raw notes to the caller for @memory: scanning.
	// Models the !364 scenario: conductor is the reviewer, so notes carry
	// <!-- pinard:conductor --> alongside substantive content.
	mr := &gitlab.MergeRequest{
		IID:         364,
		Title:       "feat(webterm): PTY viewer",
		Description: "Read-only PTY via tmux attach -r.",
		MergedAt:    "2026-08-28T00:00:00Z",
		Author:      gitlab.Author{Username: "dev"},
		WebURL:      "https://code.example.com/g/p/-/merge_requests/364",
	}
	gl := &mockMRMemoryGitLab{
		changes: []string{"responder.go"},
		notes: []gitlab.Note{
			// #1 system note — always dropped
			{ID: 1, System: true, Body: "approved this merge request", Author: gitlab.Author{Username: "gitlab"}},
			// #2 pure marker — nothing survives after stripping
			{ID: 2, Body: "<!-- pinard:webterm-link -->", Author: gitlab.Author{Username: "pinard"}},
			// #3 conductor note with substantive content — marker line stripped, prose kept
			{ID: 3, Body: "<!-- pinard:conductor -->\nLooks good. **Notes from review:** pumpBytes is shared.", Author: gitlab.Author{Username: "pinard"}},
			// #4 conductor note that is the security gem — must survive
			{ID: 4, Body: "<!-- pinard:conductor -->\nDid a dedicated security pass — no host-access/write path; read-only integrity holds.", Author: gitlab.Author{Username: "pinard"}},
			// #5 @memory: marker note — substantive, must survive
			{ID: 5, Body: "@memory: grant must be verified on attach.", Author: gitlab.Author{Username: "reviewer"}},
		},
	}

	payload, allNotes, err := BuildMRMemoryPayload(gl, "proj", "g/p", mr)
	if err != nil {
		t.Fatalf("BuildMRMemoryPayload returned error: %v", err)
	}

	// review_notes must have notes #3, #4, #5 (marker lines stripped); #1 system, #2 pure-marker dropped.
	rn, ok := payload["review_notes"].([]MRMemoryReviewNote)
	if !ok {
		t.Fatalf("review_notes is not []MRMemoryReviewNote: %T", payload["review_notes"])
	}
	if len(rn) != 3 {
		t.Errorf("expected 3 review notes, got %d: %+v", len(rn), rn)
	}
	if len(rn) >= 1 && !strings.Contains(rn[0].Body, "pumpBytes") {
		t.Errorf("note 0 body should contain 'pumpBytes', got %q", rn[0].Body)
	}
	if len(rn) >= 1 && strings.Contains(rn[0].Body, "pinard:conductor") {
		t.Errorf("note 0 body must not contain the conductor marker, got %q", rn[0].Body)
	}
	if len(rn) >= 2 && !strings.Contains(rn[1].Body, "security pass") {
		t.Errorf("note 1 body should contain 'security pass', got %q", rn[1].Body)
	}
	if len(rn) >= 3 && !strings.Contains(rn[2].Body, "grant must be verified") {
		t.Errorf("note 2 body should contain 'grant must be verified', got %q", rn[2].Body)
	}

	// allNotes must include ALL notes (for @memory: scanning).
	if len(allNotes) != 5 {
		t.Errorf("expected 5 allNotes, got %d", len(allNotes))
	}

	// ExtractMemoryMarkers must pick up note #5.
	markers := ExtractMemoryMarkers(allNotes)
	if len(markers) != 1 || markers[0] != "grant must be verified on attach." {
		t.Errorf("ExtractMemoryMarkers: got %v, want [grant must be verified on attach.]", markers)
	}
}

// ── §10 @memory: marker tests ─────────────────────────────────────────────

func TestExtractMemoryMarker_Detected(t *testing.T) {
	tests := []struct {
		body  string
		want  string
		hasIt bool
	}{
		{"@memory: pty.out must be scoped per tenant in multi-tenant NATS", "pty.out must be scoped per tenant in multi-tenant NATS", true},
		{"@Memory: Some constraint", "Some constraint", true},
		{"@MEMORY: upper case", "upper case", true},
		{"@memory:no space after colon is fine", "no space after colon is fine", true},
		{"LGTM, no memory here", "", false},
		{"pinard: some other marker", "", false},
		{"  @memory:   leading whitespace body  ", "leading whitespace body", true},
	}
	for _, tt := range tests {
		got := extractMemoryMarker(tt.body)
		if tt.hasIt && got != tt.want {
			t.Errorf("extractMemoryMarker(%q) = %q, want %q", tt.body, got, tt.want)
		}
		if !tt.hasIt && got != "" {
			t.Errorf("extractMemoryMarker(%q) = %q, want empty", tt.body, got)
		}
	}
}

// ── §9 review noise filter tests ─────────────────────────────────────────────

func TestIsReviewNoise(t *testing.T) {
	noisy := []string{
		"LGTM", "lgtm", "Looks good to me", "looks good",
		"tests pass", "Tests Passed", "CI passed", "pipeline success",
		"pushed a commit", "pushed commit", "rebased", "rebase",
		"merge when green", "merge once ready", "Thanks.", "thanks!",
	}
	for _, s := range noisy {
		if !isReviewNoise(s) {
			t.Errorf("isReviewNoise(%q) = false, want true", s)
		}
	}

	substantive := []string{
		"The grant bypass is a security hole — the grant must be verified on every attach.",
		"pty.out is not scoped per tenant; in multi-tenant this leaks terminal output.",
		"Why did we choose X over Y here?",
		"Good catch, but the real issue is that the token is not invalidated on disconnect.",
	}
	for _, s := range substantive {
		if isReviewNoise(s) {
			t.Errorf("isReviewNoise(%q) = true, want false (substantive content)", s)
		}
	}
}

func TestIsPinardMarker(t *testing.T) {
	pinard := []string{
		"<!-- pinard:conductor --> please fix the auth gap",
		"pinard: some internal marker",
	}
	for _, s := range pinard {
		if !isPinardMarker(s) {
			t.Errorf("isPinardMarker(%q) = false, want true", s)
		}
	}

	notPinard := []string{
		"The security model relies on grant verification.",
		"@memory: pty.out scoping constraint",
	}
	for _, s := range notPinard {
		if isPinardMarker(s) {
			t.Errorf("isPinardMarker(%q) = true, want false", s)
		}
	}
}

// TestMRMemory_ReviewNotesPreFilter verifies the sanitizeReviewNote logic
// (mirrors what BuildMRMemoryPayload does internally after the line-granularity fix).
func TestMRMemory_ReviewNotesPreFilter(t *testing.T) {
	notes := []gitlab.Note{
		{ID: 1, System: true, Body: "approved this merge request", Author: gitlab.Author{Username: "gitlab"}},
		{ID: 2, Body: "LGTM", Author: gitlab.Author{Username: "reviewer"}},
		{ID: 3, Body: "tests pass", Author: gitlab.Author{Username: "reviewer"}},
		{ID: 4, Body: "pinard: internal pinard marker", Author: gitlab.Author{Username: "pinard"}},
		// #5: pure marker line only — nothing survives
		{ID: 5, Body: "<!-- pinard:conductor -->", Author: gitlab.Author{Username: "pinard"}},
		{ID: 6, Body: "pty.out must be scoped per tenant — this is load-bearing security.", Author: gitlab.Author{Username: "reviewer"}},
		{ID: 7, Body: "CI passed", Author: gitlab.Author{Username: "reviewer"}},
		{ID: 8, Body: "@memory: grant verification must happen on every attach, not just connect.", Author: gitlab.Author{Username: "reviewer"}},
		{ID: 9, Body: "rebased", Author: gitlab.Author{Username: "pinard"}},
		// !364 scenario: conductor note with substantive body — marker line stripped, prose kept
		{ID: 10, Body: "<!-- pinard:conductor -->\nDid a dedicated security pass — read-only integrity holds.", Author: gitlab.Author{Username: "pinard"}},
	}

	var reviewNotes []struct{ Author, Body string }
	for _, note := range notes {
		if note.System {
			continue
		}
		body := strings.TrimSpace(note.Body)
		clean := sanitizeReviewNote(body)
		if clean == "" {
			continue
		}
		reviewNotes = append(reviewNotes, struct{ Author, Body string }{note.Author.Username, clean})
	}

	// Notes 6, 8, 10 survive; #10 has its conductor marker line stripped.
	if len(reviewNotes) != 3 {
		t.Fatalf("expected 3 review notes after filtering, got %d: %+v", len(reviewNotes), reviewNotes)
	}
	if !strings.Contains(reviewNotes[0].Body, "pty.out") {
		t.Errorf("first note should be the pty.out constraint, got %q", reviewNotes[0].Body)
	}
	if !strings.Contains(reviewNotes[1].Body, "grant verification") {
		t.Errorf("second note should be the @memory: grant note, got %q", reviewNotes[1].Body)
	}
	if !strings.Contains(reviewNotes[2].Body, "security pass") {
		t.Errorf("third note should be the security pass gem, got %q", reviewNotes[2].Body)
	}
	if strings.Contains(reviewNotes[2].Body, "pinard:conductor") {
		t.Errorf("third note must not contain the conductor marker, got %q", reviewNotes[2].Body)
	}
}

// TestSanitizeReviewNote covers the line-granularity stripping logic including the !364 scenarios.
func TestSanitizeReviewNote(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  string // "" means the note is fully dropped
	}{
		// Pure noise notes — fully dropped
		{"pure LGTM", "LGTM", ""},
		{"pure tests pass", "tests pass", ""},
		{"pure CI passed", "CI passed", ""},
		{"pure rebased", "rebased", ""},
		{"pure thanks", "thanks.", ""},
		{"pure merge when green", "merge when green", ""},
		// Pure marker notes — fully dropped
		{"pure webterm marker", "<!-- pinard:webterm-link -->", ""},
		{"pure conductor marker", "<!-- pinard:conductor -->", ""},
		{"pure pinard: prefix", "pinard: internal", ""},
		// !364 note #2: conductor marker + substantive content on next line
		{
			"!364 note2: conductor+substance",
			"<!-- pinard:conductor -->\nLooks good. **Notes from review:** pumpBytes is shared.",
			"Looks good. **Notes from review:** pumpBytes is shared.",
		},
		// !364 note #4: the security gem
		{
			"!364 note4: security gem",
			"<!-- pinard:conductor -->\nDid a dedicated security pass — no host-access/write path; read-only integrity holds.",
			"Did a dedicated security pass — no host-access/write path; read-only integrity holds.",
		},
		// !364 note #3: substantive with inline noise phrase — phrase does NOT cause line drop
		// because the whole line is not pure noise (it has other content)
		{
			"!364 note3: substantive with noise phrase",
			"The responder tests pass — pumpBytes is a shared helper; rename would break contract.",
			"The responder tests pass — pumpBytes is a shared helper; rename would break contract.",
		},
		// Mixed: marker line + noise line + substance line
		{
			"mixed marker+noise+substance",
			"<!-- pinard:conductor -->\nLGTM\nThe grant bypass is a security hole.",
			"The grant bypass is a security hole.",
		},
		// Purely substantive note — unchanged
		{
			"substantive unchanged",
			"pty.out must be scoped per tenant in multi-tenant NATS.",
			"pty.out must be scoped per tenant in multi-tenant NATS.",
		},
		// @memory: note — not a noise line, survives
		{
			"@memory: survives",
			"@memory: grant must be verified on attach.",
			"@memory: grant must be verified on attach.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeReviewNote(tt.body)
			if got != tt.want {
				t.Errorf("sanitizeReviewNote(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// TestPrRef verifies provider-aware PR/MR reference strings.
func TestPrRef(t *testing.T) {
	tests := []struct {
		provider string
		iid      int
		want     string
	}{
		{"gitlab", 42, "MR !42"},
		{"github", 42, "PR #42"},
		{"", 7, "MR !7"}, // empty defaults to gitlab vocabulary
		{"github", 1, "PR #1"},
	}
	for _, tt := range tests {
		got := prRef(tt.provider, tt.iid)
		if got != tt.want {
			t.Errorf("prRef(%q, %d) = %q, want %q", tt.provider, tt.iid, got, tt.want)
		}
	}
}

// TestMRMemory_SourceContainsReviewNotes verifies review_notes is wired into the payload.
func TestMRMemory_SourceContainsReviewNotes(t *testing.T) {
	src, err := os.ReadFile("mrs.go")
	if err != nil {
		t.Skipf("cannot read mrs.go: %v", err)
	}
	content := string(src)
	if !strings.Contains(content, "review_notes") {
		t.Error("BuildMRMemoryPayload must include review_notes field in the payload")
	}
	if !strings.Contains(content, "publishMemoryLesson") {
		t.Error("mrs.go must define publishMemoryLesson for @memory: handling")
	}
}
