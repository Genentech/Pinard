package state

type WatchedMR struct {
	Name                  string `yaml:"name"`
	Project               string `yaml:"project"`
	Repo                  string `yaml:"repo"`
	Parcelle              string `yaml:"parcelle,omitempty"`
	ProcessName           string `yaml:"process_name,omitempty"`
	MR                    int    `yaml:"mr,omitempty"`
	LastNoteID            int    `yaml:"last_note_id"`
	LastPipelineID        int    `yaml:"last_pipeline_id,omitempty"`
	PipelineFailCount     int    `yaml:"pipeline_fail_count,omitempty"`
	ReviewPending         bool   `yaml:"review_pending,omitempty"`
	NeedsApprovalNotified bool   `yaml:"needs_approval_notified,omitempty"`
	ReviewedSHA           string `yaml:"reviewed_sha,omitempty"`
	ReviewNotified        bool   `yaml:"review_notified,omitempty"`
	AutoMergeLabeled      bool   `yaml:"auto_merge_labeled,omitempty"`
	State                 string `yaml:"state,omitempty"`
	MergedAt              string `yaml:"merged_at,omitempty"`
	PostMergeChecks       int    `yaml:"post_merge_checks,omitempty"`
	MergeCommitSHA        string `yaml:"merge_commit_sha,omitempty"`
	MainPipelineDone      bool   `yaml:"main_pipeline_done,omitempty"`
	TagPipelineDone       bool   `yaml:"tag_pipeline_done,omitempty"`
	LastChecked           string `yaml:"last_checked,omitempty"`
	NotFoundCount         int    `yaml:"not_found_count,omitempty"`
}

type MRWatcherState struct {
	Watched map[string]*WatchedMR `yaml:"watched"`
}

// OpenMR reports whether a worker (matched by KV key or by tmux/session name)
// has an open, unmerged MR tracked in the watcher state. "Open" means it has
// an MR number and its state hasn't reached the terminal post_merge stage.
// Freshly-tracked entries have an *empty* State ("opened"/"in-review"/etc. are
// never written — the only value MRWatcher ever assigns is "post_merge" on
// completion, see mrs.go) — so State == "" with MR > 0 is the common "still
// open" case, not an unknown/excluded one. Returns (0, false) when mrState is
// nil or no matching entry qualifies — callers must not treat that as a
// guarantee there's no MR, only that none is known to be open.
func OpenMR(mrState *Store[MRWatcherState], key, name string) (int, bool) {
	if mrState == nil {
		return 0, false
	}
	var mr int
	var open bool
	mrState.Read(func(s *MRWatcherState) {
		for k, entry := range s.Watched {
			if entry == nil {
				continue
			}
			if k != key && entry.Name != name {
				continue
			}
			if entry.MR > 0 && entry.State != "post_merge" {
				mr = entry.MR
				open = true
			}
		}
	})
	return mr, open
}

// DropWatchedMR removes the watcher entry/entries for a worker (matched by KV
// key or by tmux/session name) after it has been torn down externally (e.g. a
// forced respawn reaping a worker with an open MR), so the MR watcher doesn't
// keep polling a session that no longer exists. Returns true if at least one
// entry was found and removed. No-op when mrState is nil.
func DropWatchedMR(mrState *Store[MRWatcherState], key, name string) bool {
	if mrState == nil {
		return false
	}
	var dropped bool
	mrState.Update(func(s *MRWatcherState) {
		if s.Watched == nil {
			return
		}
		for k, entry := range s.Watched {
			if k == key || (entry != nil && entry.Name == name) {
				delete(s.Watched, k)
				dropped = true
			}
		}
	})
	return dropped
}
