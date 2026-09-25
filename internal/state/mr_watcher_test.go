package state

import (
	"path/filepath"
	"testing"
)

func TestOpenMR(t *testing.T) {
	dir := t.TempDir()
	st, err := Load[MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *MRWatcherState) {
		s.Watched = map[string]*WatchedMR{
			// This is the realistic "open" shape: MRWatcher never writes anything
			// but "post_merge" (see mrs.go) — a freshly-tracked, still-open MR has
			// State == "". A regression test using a non-empty placeholder like
			// "review" here would NOT have caught the State != "" bug that made
			// OpenMR return false for every real open MR (MR !626 review).
			"agent-open":         {Name: "sess-open", MR: 42, State: ""},
			"agent-open-labeled": {Name: "sess-open-labeled", MR: 45, State: "in-review"},
			"agent-merged":       {Name: "sess-merged", MR: 43, State: "post_merge"},
			"agent-no-mr":        {Name: "sess-no-mr", MR: 0, State: ""},
		}
	})

	cases := []struct {
		name      string
		key       string
		sessName  string
		wantOpen  bool
		wantMRNum int
	}{
		// Regression for the inert-predicate bug: State == "" + MR > 0 must be
		// reported open — this is the shape every real open MR has.
		{"open with empty state (the realistic case)", "agent-open", "no-match", true, 42},
		{"open by name", "no-match", "sess-open", true, 42},
		{"open with a non-empty, non-post_merge state", "agent-open-labeled", "", true, 45},
		{"post_merge is not open", "agent-merged", "", false, 0},
		{"no MR number is not open", "agent-no-mr", "", false, 0},
		{"no match at all", "nope", "nope", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mr, open := OpenMR(st, c.key, c.sessName)
			if open != c.wantOpen {
				t.Errorf("OpenMR(%q,%q) open = %v, want %v", c.key, c.sessName, open, c.wantOpen)
			}
			if mr != c.wantMRNum {
				t.Errorf("OpenMR(%q,%q) mr = %d, want %d", c.key, c.sessName, mr, c.wantMRNum)
			}
		})
	}
}

func TestOpenMR_NilStore(t *testing.T) {
	mr, open := OpenMR(nil, "any", "any")
	if open || mr != 0 {
		t.Errorf("expected (0, false) for a nil store, got (%d, %v)", mr, open)
	}
}

func TestDropWatchedMR(t *testing.T) {
	dir := t.TempDir()
	st, err := Load[MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *MRWatcherState) {
		s.Watched = map[string]*WatchedMR{
			"agent-a": {Name: "sess-a", MR: 1},
			"agent-b": {Name: "sess-b", MR: 2},
		}
	})

	if dropped := DropWatchedMR(st, "agent-a", "no-match"); !dropped {
		t.Error("expected DropWatchedMR to report a drop for a key match")
	}
	st.Read(func(s *MRWatcherState) {
		if _, ok := s.Watched["agent-a"]; ok {
			t.Error("expected agent-a entry to be removed")
		}
		if _, ok := s.Watched["agent-b"]; !ok {
			t.Error("expected agent-b entry to survive")
		}
	})

	if dropped := DropWatchedMR(st, "no-match", "sess-b"); !dropped {
		t.Error("expected DropWatchedMR to report a drop for a name match")
	}
	st.Read(func(s *MRWatcherState) {
		if _, ok := s.Watched["agent-b"]; ok {
			t.Error("expected agent-b entry to be removed by name match")
		}
	})

	if dropped := DropWatchedMR(st, "nope", "nope"); dropped {
		t.Error("expected no drop when nothing matches")
	}
}

func TestDropWatchedMR_NilStore(t *testing.T) {
	if dropped := DropWatchedMR(nil, "any", "any"); dropped {
		t.Error("expected false for a nil store")
	}
}
