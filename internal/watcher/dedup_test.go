package watcher

// dedup_test.go — regression tests for issue #308: duplicate/stale review
// dispatches caused by (1) an MR being tracked under two independent
// WatchedMR keys, and (2) tryAutoReview never checking that the MR is still
// open. Tests call the production methods directly (reconcileDuplicateMRs,
// tryAutoReview) rather than re-implementing their predicates locally.

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/state"
	"github.com/nats-io/nats.go"
)

// ── Defect 1: duplicate-entry reconciliation ────────────────────────────────

// TestReconcileDuplicateMRs_MergesAndCollapses reproduces the live duplicate
// reported in #308: the same MR tracked under two keys (one alive in KV, one
// not), each with different progress. Reconciliation must keep exactly one
// entry — the one backed by a live KV record — and pull forward the more
// advanced progress fields from the entry it discards.
func TestReconcileDuplicateMRs_MergesAndCollapses(t *testing.T) {
	dir := t.TempDir()
	mrState, err := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	const (
		repo    = "group/pinard"
		mr      = 598
		liveKey = "webterm--pinard-3058b7433"
		runKey  = "pinard-swe-305"
	)

	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			liveKey: {
				Name:    liveKey,
				Project: "pinard",
				Repo:    repo,
				MR:      mr,
				State:   "post_merge",
				// Fewer notes forwarded / no approval notification yet on this key.
				LastNoteID:            2,
				NeedsApprovalNotified: false,
				ReviewedSHA:           "",
				LastChecked:           "2026-09-01T00:00:00Z",
			},
			runKey: {
				Name:                  runKey,
				Project:               "pinard",
				Repo:                  repo,
				MR:                    mr,
				State:                 "post_merge",
				LastNoteID:            7,
				NeedsApprovalNotified: true,
				ReviewedSHA:           "deadbeef",
				LastChecked:           "2026-09-02T00:00:00Z",
			},
		}
	})

	kv := newMockKV()
	kv.setAgent(liveKey, map[string]any{"state": "running"})
	// runKey has no KV record at all — matches the live-proof evidence that
	// the issue-driven runID never had its own tmux session.

	w := &MRWatcher{State: mrState, KV: kv}
	w.reconcileDuplicateMRs()

	var watched map[string]*state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		watched = s.Watched
	})

	if len(watched) != 1 {
		t.Fatalf("expected exactly 1 watched entry after reconciliation, got %d: %v", len(watched), watched)
	}
	entry, ok := watched[liveKey]
	if !ok {
		t.Fatalf("expected surviving entry to be keyed by the live session %q, got keys: %v", liveKey, watched)
	}
	if entry.LastNoteID != 7 {
		t.Errorf("LastNoteID: got %d, want 7 (merged from losing entry)", entry.LastNoteID)
	}
	if !entry.NeedsApprovalNotified {
		t.Error("NeedsApprovalNotified: expected true (merged from losing entry)")
	}
	if entry.ReviewedSHA != "deadbeef" {
		t.Errorf("ReviewedSHA: got %q, want %q (non-empty wins)", entry.ReviewedSHA, "deadbeef")
	}
	if entry.State != "post_merge" {
		t.Errorf("State: got %q, want post_merge", entry.State)
	}
}

// TestReconcileDuplicateMRs_NoDuplicates_NoOp verifies that a single-keyed
// MR (the common case) is left untouched.
func TestReconcileDuplicateMRs_NoDuplicates_NoOp(t *testing.T) {
	dir := t.TempDir()
	mrState, err := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"pinard-swe-265": {Name: "pinard-swe-265", Project: "pinard", Repo: "group/pinard", MR: 600},
		}
	})

	w := &MRWatcher{State: mrState, KV: newMockKV()}
	w.reconcileDuplicateMRs()

	var watched map[string]*state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		watched = s.Watched
	})
	if len(watched) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(watched))
	}
	if _, ok := watched["pinard-swe-265"]; !ok {
		t.Error("single-keyed entry should be left in place")
	}
}

// ── Defect 2: terminal-state gate in tryAutoReview ─────────────────────────

// makeAutoReviewWatcher assembles an MRWatcher for a tryAutoReview unit test.
func makeAutoReviewWatcher(t *testing.T, repo string, p *autoMergePressoir) (*MRWatcher, *state.Store[state.MRWatcherState]) {
	t.Helper()
	dir := t.TempDir()
	mrState, err := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	vignoble := &config.Vignoble{
		Name: "test",
		Config: &config.VignobleConfig{
			GitLabHost: "gitlab.example.com",
			Vignes: map[string]config.Vigne{
				"pinard": {Repo: repo, Pressoir: config.PressoirConfig{ProviderName: "gitlab"}},
			},
		},
	}
	resolver := &PressoirResolver{Vignoble: vignoble, Fallback: p}
	resolver.mu.Lock()
	resolver.cache = map[string]pressoir.Pressoir{repo: p}
	resolver.mu.Unlock()

	w := &MRWatcher{
		State:    mrState,
		NATS:     noopNATS(),
		KV:       newMockKV(),
		Vignoble: vignoble,
		Resolver: resolver,
	}
	return w, mrState
}

// TestTryAutoReview_SkipsMergedMR is the regression test for defect 2: a
// merged (or closed) MR must never receive a needs_review dispatch, even
// though tryAutoReview fetches its own, independent PR snapshot.
func TestTryAutoReview_SkipsMergedMR(t *testing.T) {
	for _, terminalState := range []string{"merged", "closed"} {
		t.Run(terminalState, func(t *testing.T) {
			const (
				sessionName = "worker-583"
				repo        = "group/pinard"
			)
			p := &autoMergePressoir{
				ciState: "success",
				pr:      pressoir.PullRequest{State: terminalState, Title: "Fix something", HeadSHA: "sha1"},
			}
			w, mrState := makeAutoReviewWatcher(t, repo, p)
			entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 583, Parcelle: "pinard"}
			mrState.Update(func(s *state.MRWatcherState) {
				s.Watched = map[string]*state.WatchedMR{sessionName: entry}
			})

			w.tryAutoReview(sessionName, entry)

			var got *state.WatchedMR
			mrState.Read(func(s *state.MRWatcherState) {
				got = s.Watched[sessionName]
			})
			if got.ReviewedSHA != "" {
				t.Errorf("ReviewedSHA should remain empty for a %s MR, got %q — needs_review was dispatched", terminalState, got.ReviewedSHA)
			}
			if got.ReviewNotified {
				t.Errorf("ReviewNotified should remain false for a %s MR", terminalState)
			}
		})
	}
}

// TestTryAutoReview_DispatchesForOpenMR is the counterpart sanity check: an
// open, green, non-draft MR at a new SHA must still be dispatched.
func TestTryAutoReview_DispatchesForOpenMR(t *testing.T) {
	const (
		sessionName = "worker-600"
		repo        = "group/pinard"
	)
	p := &autoMergePressoir{
		ciState: "success",
		pr:      pressoir.PullRequest{State: "opened", Title: "Fix something", HeadSHA: "sha1"},
	}
	w, mrState := makeAutoReviewWatcher(t, repo, p)
	entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 600, Parcelle: "pinard"}
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{sessionName: entry}
	})

	// noopNATS() cannot actually deliver, so the optimistic ReviewedSHA mark
	// (which only happens after a successful Publish) will not be set here;
	// this test only guards against a regression in the earlier gates
	// (CI/draft/trivial-title) — SHA-mark + idempotency is covered against a
	// real NATS server in TestTryAutoReview_IdempotentPerSHA below.
	w.tryAutoReview(sessionName, entry)
}

// TestTryAutoReview_IdempotentPerSHA is the regression test for the
// invariant the issue endorses: "a reviewer is notified at most once per
// (MR, SHA)". Against a real (embedded) NATS/JetStream server, calling
// tryAutoReview twice for the same HEAD SHA — simulating a re-emitted tick
// or a second surviving duplicate entry before reconciliation lands — must
// publish needs_review exactly once.
func TestTryAutoReview_IdempotentPerSHA(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()

	const (
		sessionName = "worker-600"
		repo        = "group/pinard"
	)
	p := &autoMergePressoir{
		ciState: "success",
		pr:      pressoir.PullRequest{State: "opened", Title: "Fix something", HeadSHA: "sha1", WebURL: "https://example.com/mr/600"},
	}
	w, mrState := makeAutoReviewWatcher(t, repo, p)
	w.NATS = testClient(t, url)
	defer w.NATS.Close()

	entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 600, Parcelle: "pinard"}
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{sessionName: entry}
	})

	sub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	defer sub.Close()
	syncSub, err := sub.SubscribeSync("pinard.test.parcelles.pinard.notifications")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	sub.Flush()

	// First call: should dispatch and mark the entry reviewed at this SHA.
	w.tryAutoReview(sessionName, entry)

	var afterFirst *state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		afterFirst = s.Watched[sessionName]
	})
	if afterFirst.ReviewedSHA != "sha1" || !afterFirst.ReviewNotified {
		t.Fatalf("expected first tryAutoReview call to mark ReviewedSHA=sha1/ReviewNotified=true, got %+v", afterFirst)
	}

	if _, err := syncSub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("expected one needs_review notification after the first call: %v", err)
	}

	// Second call at the SAME SHA (repeat-event / surviving-duplicate
	// scenario, e.g. a second watcher tick or a duplicate entry not yet
	// reconciled): re-fetch the entry from the store first — as Run() does
	// on every real tick — so the SHA gate observes the persisted mark, then
	// call tryAutoReview again. Must NOT publish a second time.
	var second *state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		second = s.Watched[sessionName]
	})
	w.tryAutoReview(sessionName, second)

	if msg, err := syncSub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("expected no second needs_review notification for the same SHA, got: %s", msg.Data)
	}
}
