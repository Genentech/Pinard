package watcher

// review_ack_test.go — regression tests for issue #322: the mr-watcher must
// treat a maître-applied `pinard:reviewed` label as a record-without-notify
// ALL-CLEAR ack (stops re-dispatch, no note), and must clear a stale label
// once a new commit invalidates it. Tests call tryAutoReview directly
// (production logic), per the #598-review directive not to re-implement its
// predicates locally.

import (
	"strings"
	"testing"
	"time"

	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/state"
	"github.com/nats-io/nats.go"
)

// TestTryAutoReview_ReviewedLabelStopsRedispatch verifies that a MR already
// carrying the ReviewedLabel at the current head SHA is treated as
// review-complete: no needs_review is published, and no MR note is posted
// (tryAutoReview never posts notes itself — the ack path is silent by
// construction).
func TestTryAutoReview_ReviewedLabelStopsRedispatch(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()

	const (
		sessionName = "worker-700"
		repo        = "group/pinard"
	)
	p := &autoMergePressoir{
		ciState: "success",
		pr:      pressoir.PullRequest{State: "opened", Title: "Fix something", HeadSHA: "sha1", Labels: []string{ReviewedLabel}},
	}
	w, mrState := makeAutoReviewWatcher(t, repo, p)
	w.NATS = testClient(t, url)
	defer w.NATS.Close()

	entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 700, Parcelle: "pinard"}
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

	w.tryAutoReview(sessionName, entry)

	if msg, err := syncSub.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("expected no needs_review notification for an already-labeled MR, got: %s", msg.Data)
	}

	var got *state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		got = s.Watched[sessionName]
	})
	if got.ReviewedSHA != "sha1" || !got.ReviewNotified {
		t.Errorf("expected local state synced to the labeled SHA (ReviewedSHA=sha1, ReviewNotified=true), got %+v", got)
	}
	if len(p.removedLabels) != 0 {
		t.Errorf("label at the current head SHA should not be removed, got removals: %v", p.removedLabels)
	}
}

// TestTryAutoReview_StaleReviewedLabelIsCleared verifies that a ReviewedLabel
// left over from a prior commit (entry.ReviewedSHA no longer matches the
// fetched head SHA) is removed — labels do not auto-invalidate on push like
// GitLab approvals do — and that a fresh needs_review IS dispatched for the
// new SHA.
func TestTryAutoReview_StaleReviewedLabelIsCleared(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()

	const (
		sessionName = "worker-701"
		repo        = "group/pinard"
	)
	p := &autoMergePressoir{
		ciState: "success",
		pr:      pressoir.PullRequest{State: "opened", Title: "Fix something", HeadSHA: "sha2", WebURL: "https://example.com/mr/701", Labels: []string{ReviewedLabel}},
	}
	w, mrState := makeAutoReviewWatcher(t, repo, p)
	w.NATS = testClient(t, url)
	defer w.NATS.Close()

	// Locally we last reviewed sha1; the forge label is stale (still applied
	// from that review) while the MR has since moved to sha2.
	entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 701, Parcelle: "pinard", ReviewedSHA: "sha1", ReviewNotified: true}
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

	w.tryAutoReview(sessionName, entry)

	if len(p.removedLabels) != 1 || p.removedLabels[0] != ReviewedLabel {
		t.Fatalf("expected the stale %q label to be removed, got removals: %v", ReviewedLabel, p.removedLabels)
	}

	if _, err := syncSub.NextMsg(2 * time.Second); err != nil {
		t.Fatalf("expected a fresh needs_review notification for the new SHA: %v", err)
	}

	var got *state.WatchedMR
	mrState.Read(func(s *state.MRWatcherState) {
		got = s.Watched[sessionName]
	})
	if got.ReviewedSHA != "sha2" || !got.ReviewNotified {
		t.Errorf("expected ReviewedSHA=sha2/ReviewNotified=true after redispatch, got %+v", got)
	}
}

// TestTryAutoReview_DispatchWordingStatesFactNotVerdict is the regression
// test for Defect 5: the dispatch message must state the fact (CI passed),
// not a verdict ("green"/"ready") that could nudge a reviewer toward
// rubber-stamping.
func TestTryAutoReview_DispatchWordingStatesFactNotVerdict(t *testing.T) {
	ns, url := startEmbeddedNATS(t)
	defer ns.Shutdown()

	const (
		sessionName = "worker-702"
		repo        = "group/pinard"
	)
	p := &autoMergePressoir{
		ciState: "success",
		pr:      pressoir.PullRequest{State: "opened", Title: "Fix something", HeadSHA: "sha1", WebURL: "https://example.com/mr/702"},
	}
	w, mrState := makeAutoReviewWatcher(t, repo, p)
	w.NATS = testClient(t, url)
	defer w.NATS.Close()

	entry := &state.WatchedMR{Name: sessionName, Project: "pinard", Repo: repo, MR: 702, Parcelle: "pinard"}
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

	w.tryAutoReview(sessionName, entry)

	msg, err := syncSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a needs_review notification: %v", err)
	}
	body := string(msg.Data)
	if strings.Contains(body, "is green") {
		t.Errorf("dispatch message still asserts readiness (\"is green\"): %s", body)
	}
	if !strings.Contains(body, "CI passed") {
		t.Errorf("dispatch message should state the fact (\"CI passed\"): %s", body)
	}
}
