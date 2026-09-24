package main

import (
	"fmt"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/state"
)

// mockKV is a simple in-memory KVReader used for testing resolveAgentRecord.
type mockKV struct {
	records map[string]map[string]any
}

func (m *mockKV) Get(bucket, key string) (map[string]any, error) {
	if rec, ok := m.records[key]; ok {
		return rec, nil
	}
	return nil, fmt.Errorf("key not found")
}

func (m *mockKV) Keys(bucket string) ([]string, error) {
	keys := make([]string, 0, len(m.records))
	for k := range m.records {
		keys = append(keys, k)
	}
	return keys, nil
}

// BUG REGRESSION (#63): parcelle-style session names ("<parcelle>--<project>-<id>")
// must yield the correct project even when --project is not passed to track-mr.
func TestDeriveProjectFromSessionRest(t *testing.T) {
	vignes := map[string]config.Vigne{
		"pinard": {Repo: "your-group/pinard"},
		"exo-cli": {Repo: "exohub/exo-cli"},
		"my-service": {Repo: "team/my-service"},
	}

	cases := []struct {
		name string
		rest string // the portion after "--" in the session name
		want string
	}{
		{
			name: "known vigne with hex suffix",
			rest: "pinard-60a2f57b",
			want: "pinard",
		},
		{
			name: "known vigne with longer hex suffix",
			rest: "exo-cli-9188a934",
			want: "exo-cli",
		},
		{
			name: "known vigne with longer id (hyphenated)",
			rest: "my-service-abcd1234",
			want: "my-service",
		},
		{
			name: "unknown vigne — fallback strips hex suffix",
			rest: "some-project-deadbeef",
			want: "some-project",
		},
		{
			name: "no hex suffix — cannot determine",
			rest: "pinard-notahex",
			want: "pinard", // matched by vigne prefix
		},
		{
			name: "completely unknown, no hex suffix",
			rest: "unknown-word",
			want: "",
		},
		{
			name: "exact vigne name (no id)",
			rest: "pinard",
			want: "pinard",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveProjectFromSessionRest(c.rest, vignes)
			if got != c.want {
				t.Errorf("deriveProjectFromSessionRest(%q) = %q, want %q", c.rest, got, c.want)
			}
		})
	}
}

func TestIsHexString(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"60a2f57b", true},
		{"deadbeef", true},
		{"DEADBEEF", true},
		{"9188a934", true},
		{"", false},
		{"notahex", false},
		{"word", false},
		{"abc123", true},
		{"abc-123", false},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			if got := isHexString(c.s); got != c.want {
				t.Errorf("isHexString(%q) = %v, want %v", c.s, got, c.want)
			}
		})
	}
}

func TestShouldPostWebtermLink(t *testing.T) {
	cases := []struct {
		name          string
		hasKVRecord   bool
		isNewTracking bool
		repo          string
		want          bool
	}{
		{
			name:          "real vendangeur, new tracking, repo known → post link",
			hasKVRecord:   true,
			isNewTracking: true,
			repo:          "exohub/exo-cli",
			want:          true,
		},
		{
			name:          "no KV record (tracking-only / cuvée) → no link",
			hasKVRecord:   false,
			isNewTracking: true,
			repo:          "exohub/exo-cli",
			want:          false,
		},
		{
			name:          "real vendangeur but not new tracking → no link",
			hasKVRecord:   true,
			isNewTracking: false,
			repo:          "exohub/exo-cli",
			want:          false,
		},
		{
			name:          "real vendangeur, new tracking, empty repo → no link",
			hasKVRecord:   true,
			isNewTracking: true,
			repo:          "",
			want:          false,
		},
		{
			name:          "no KV record, not new tracking, no repo → no link",
			hasKVRecord:   false,
			isNewTracking: false,
			repo:          "",
			want:          false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldPostWebtermLink(c.hasKVRecord, c.isNewTracking, c.repo)
			if got != c.want {
				t.Errorf("shouldPostWebtermLink(%v, %v, %q) = %v, want %v",
					c.hasKVRecord, c.isNewTracking, c.repo, got, c.want)
			}
		})
	}
}

func TestResolveAgentRecord(t *testing.T) {
	// Scheduled-task worker: KV key == tmux session name == agentId.
	scheduledRec := map[string]any{
		"name":     "exohub-user-guide-sync-20260713",
		"agentId":  "exohub-user-guide-sync-20260713",
		"vignoble": "exohub",
	}
	// Issue-driven worker: KV key (agentId) ≠ tmux session name.
	issueRec := map[string]any{
		"name":     "pinard--pinard-38b4bbff",
		"agentId":  "pinard-swe-38",
		"runId":    "pinard-swe-38",
		"vignoble": "misc",
	}

	kv := &mockKV{
		records: map[string]map[string]any{
			"exohub-user-guide-sync-20260713": scheduledRec,
			"pinard-swe-38":                  issueRec,
		},
	}

	cases := []struct {
		name      string
		token     string
		wantNil   bool
		wantName  string
	}{
		{
			name:     "scheduled: direct hit by KV key",
			token:    "exohub-user-guide-sync-20260713",
			wantName: "exohub-user-guide-sync-20260713",
		},
		{
			name:     "issue-driven: hit by tmux session name field",
			token:    "pinard--pinard-38b4bbff",
			wantName: "pinard--pinard-38b4bbff",
		},
		{
			name:     "issue-driven: hit by agentId field (KV key)",
			token:    "pinard-swe-38",
			wantName: "pinard--pinard-38b4bbff",
		},
		{
			name:     "issue-driven: hit by runId field",
			token:    "pinard-swe-38",
			wantName: "pinard--pinard-38b4bbff",
		},
		{
			name:    "no match → nil",
			token:   "track-some-cuvee-mr",
			wantNil: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveAgentRecord(kv, c.token)
			if c.wantNil {
				if got != nil {
					t.Errorf("resolveAgentRecord(%q) = %v, want nil", c.token, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("resolveAgentRecord(%q) = nil, want record with name=%q", c.token, c.wantName)
			}
			if name, _ := got["name"].(string); name != c.wantName {
				t.Errorf("resolveAgentRecord(%q)[\"name\"] = %q, want %q", c.token, name, c.wantName)
			}
		})
	}
}

func TestWebtermLinkAlreadyPosted(t *testing.T) {
	cases := []struct {
		name  string
		notes []pressoir.Comment
		want  bool
	}{
		{
			name:  "no notes → not posted",
			notes: nil,
			want:  false,
		},
		{
			name:  "notes without marker → not posted",
			notes: []pressoir.Comment{{ID: 1, Body: "LGTM"}, {ID: 2, Body: "CI passed"}},
			want:  false,
		},
		{
			name: "one note contains marker → already posted",
			notes: []pressoir.Comment{
				{ID: 1, Body: "LGTM"},
				{ID: 2, Body: webtermNoteMarker + "\n🖥️ **Live terminal** (vendangeur `session`, read-only):\n\nhttps://example.com"},
			},
			want: true,
		},
		{
			name: "second call with duplicate notes → already posted",
			notes: []pressoir.Comment{
				{ID: 10, Body: webtermNoteMarker + "\n🖥️ **Live terminal** (vendangeur `a`, read-only):\n\nhttps://a"},
				{ID: 11, Body: webtermNoteMarker + "\n🖥️ **Live terminal** (vendangeur `b`, read-only):\n\nhttps://b"},
			},
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := webtermLinkAlreadyPosted(c.notes)
			if got != c.want {
				t.Errorf("webtermLinkAlreadyPosted(%v) = %v, want %v", c.notes, got, c.want)
			}
		})
	}
}

// TestRegisterWatchedMR_SameMRDifferentSession_NoDuplicate is the regression
// test for issue #308 defect 1: the same logical agent calling `aoc
// track-mr` under two different session identities for the same MR (e.g.
// the issue-driven runID "pinard-swe-305" and the actual session name
// "webterm--pinard-3058b7433") must not create a second WatchedMR entry.
func TestRegisterWatchedMR_SameMRDifferentSession_NoDuplicate(t *testing.T) {
	s := &state.MRWatcherState{
		Watched: map[string]*state.WatchedMR{
			"pinard-swe-305": {
				Name:       "pinard-swe-305",
				Project:    "pinard",
				Repo:       "group/pinard",
				MR:         598,
				LastNoteID: 4,
			},
		},
	}

	isNew := registerWatchedMR(s, "webterm--pinard-3058b7433", "pinard", "group/pinard", 598, "pinard", "swe")

	if isNew {
		t.Error("registerWatchedMR should report isNewTracking=false when the MR is already tracked under another key")
	}
	if len(s.Watched) != 1 {
		t.Fatalf("expected exactly 1 watched entry, got %d: %v", len(s.Watched), s.Watched)
	}
	existing, ok := s.Watched["pinard-swe-305"]
	if !ok {
		t.Fatal("expected the original entry to survive, keyed by its original session")
	}
	if existing.LastNoteID != 4 {
		t.Errorf("LastNoteID should be preserved (not reset), got %d", existing.LastNoteID)
	}
	if existing.Parcelle != "pinard" || existing.ProcessName != "swe" {
		t.Errorf("expected routing fields to be refreshed from the second call, got Parcelle=%q ProcessName=%q", existing.Parcelle, existing.ProcessName)
	}
}

// TestRegisterWatchedMR_NewMR_CreatesEntry verifies the ordinary first-time
// tracking path still creates a new entry.
func TestRegisterWatchedMR_NewMR_CreatesEntry(t *testing.T) {
	s := &state.MRWatcherState{}

	isNew := registerWatchedMR(s, "worker-1", "pinard", "group/pinard", 600, "pinard", "swe")

	if !isNew {
		t.Error("registerWatchedMR should report isNewTracking=true for a brand-new (repo, mr)")
	}
	if len(s.Watched) != 1 {
		t.Fatalf("expected exactly 1 watched entry, got %d", len(s.Watched))
	}
	if _, ok := s.Watched["worker-1"]; !ok {
		t.Fatal("expected new entry to be keyed by the given session name")
	}
}

// TestRegisterWatchedMR_SameSessionSameMR_RefreshesInPlace verifies
// re-tracking under the SAME session does not reset progress fields.
func TestRegisterWatchedMR_SameSessionSameMR_RefreshesInPlace(t *testing.T) {
	s := &state.MRWatcherState{
		Watched: map[string]*state.WatchedMR{
			"worker-1": {Name: "worker-1", Project: "pinard", Repo: "group/pinard", MR: 600, LastNoteID: 9, ReviewedSHA: "abc"},
		},
	}

	isNew := registerWatchedMR(s, "worker-1", "pinard", "group/pinard", 600, "", "")

	if isNew {
		t.Error("re-tracking the same session+MR should not report isNewTracking=true")
	}
	entry := s.Watched["worker-1"]
	if entry.LastNoteID != 9 || entry.ReviewedSHA != "abc" {
		t.Errorf("re-tracking must not reset progress fields, got LastNoteID=%d ReviewedSHA=%q", entry.LastNoteID, entry.ReviewedSHA)
	}
}
