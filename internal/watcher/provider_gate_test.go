package watcher

// provider_gate_test.go — unit tests for the fix that gates GitLab-specific
// auto-merge checks (approvals, discussions, memory events) by repo provider,
// not by client presence.
//
// Bug: tryAutoMerge guarded approval/discussion checks with `w.GitLab != nil`.
// The daemon ALWAYS constructs a GitLab client, so that guard was always true
// even for GitHub repos — causing a 404 on GetMRApprovals → early return →
// auto-merge never fired for GitHub PRs.
//
// Fix: replaced `if w.GitLab != nil` with `if w.isGitLabRepo(entry.Repo)`.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/gitlab"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/state"
)

// noopNATS returns a pnats.Client that will fail to connect (no server)
// but will not panic. Publish errors are logged and ignored in publishEvent.
func noopNATS() *pnats.Client {
	return pnats.NewClient(&config.Credentials{NATS: config.NATSConfig{URL: "nats://127.0.0.1:1"}})
}

// ── Pressoir stub ─────────────────────────────────────────────────────────────

// autoMergePressoir implements pressoir.Pressoir for unit tests.
// Only the methods used by tryAutoMerge are backed; all others are no-ops.
type autoMergePressoir struct {
	ciState          string
	pr               pressoir.PullRequest
	mergeErr         error
	merged           bool
	approved         bool
	approvalErr      error
	approvalCallCount int
	addedLabels      []string
	removedLabels    []string
	removeLabelErr   error
}

func (s *autoMergePressoir) CIStatusFor(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.CIStatus, error) {
	return pressoir.CIStatus{State: s.ciState}, nil
}
func (s *autoMergePressoir) GetPR(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.PullRequest, error) {
	return s.pr, nil
}
func (s *autoMergePressoir) Merge(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.MergeOpts) error {
	s.merged = true
	return s.mergeErr
}
func (s *autoMergePressoir) UpdatePR(_ context.Context, _ pressoir.RepoRef, _ int, _ map[string]string) error {
	return nil
}
func (s *autoMergePressoir) OpenPR(_ context.Context, _ pressoir.RepoRef, _, _, _, _ string, _ bool) (pressoir.PullRequest, error) {
	return pressoir.PullRequest{}, nil
}
func (s *autoMergePressoir) ListOpenPRs(_ context.Context, _ pressoir.RepoRef) ([]pressoir.PullRequest, error) {
	return nil, nil
}
func (s *autoMergePressoir) Approve(_ context.Context, _ pressoir.RepoRef, _ int) error { return nil }
func (s *autoMergePressoir) GetApprovalStatus(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.ApprovalStatus, error) {
	s.approvalCallCount++
	if s.approvalErr != nil {
		return pressoir.ApprovalStatus{}, s.approvalErr
	}
	return pressoir.ApprovalStatus{Approved: s.approved}, nil
}
func (s *autoMergePressoir) Comment(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *autoMergePressoir) PostPRNote(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *autoMergePressoir) InlineComment(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.InlineComment, _ string) error {
	return nil
}
func (s *autoMergePressoir) ListPRNotes(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Comment, error) {
	return nil, nil
}
func (s *autoMergePressoir) ListPRDiscussions(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Discussion, error) {
	return nil, nil
}
func (s *autoMergePressoir) GetPRChanges(_ context.Context, _ pressoir.RepoRef, _ int) ([]string, error) {
	return nil, nil
}
func (s *autoMergePressoir) GetPRClosingIssues(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Issue, error) {
	return nil, nil
}
func (s *autoMergePressoir) ListPipelinesByCommit(_ context.Context, _ pressoir.RepoRef, _ string) ([]pressoir.CIStatus, error) {
	return nil, nil
}
func (s *autoMergePressoir) ListIssues(_ context.Context, _ pressoir.RepoRef, _ pressoir.IssueFilter) ([]pressoir.Issue, error) {
	return nil, nil
}
func (s *autoMergePressoir) GetIssue(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.Issue, error) {
	return pressoir.Issue{}, nil
}
func (s *autoMergePressoir) CreateIssue(_ context.Context, _ pressoir.RepoRef, _ map[string]string) (pressoir.Issue, error) {
	return pressoir.Issue{}, nil
}
func (s *autoMergePressoir) UpdateIssue(_ context.Context, _ pressoir.RepoRef, _ int, _ map[string]string) error {
	return nil
}
func (s *autoMergePressoir) PostIssueNote(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *autoMergePressoir) SetLabels(_ context.Context, _ pressoir.RepoRef, _ int, _ []string) error {
	return nil
}
func (s *autoMergePressoir) AddLabel(_ context.Context, _ pressoir.RepoRef, _ int, label string) error {
	s.addedLabels = append(s.addedLabels, label)
	return nil
}
func (s *autoMergePressoir) RemoveLabel(_ context.Context, _ pressoir.RepoRef, _ int, label string) error {
	s.removedLabels = append(s.removedLabels, label)
	return s.removeLabelErr
}
func (s *autoMergePressoir) ListIssueNotes(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Comment, error) {
	return nil, nil
}
func (s *autoMergePressoir) LinkIssues(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *autoMergePressoir) AddSubIssue(_ context.Context, _ pressoir.RepoRef, _, _ int) error {
	return nil
}
func (s *autoMergePressoir) GetDefaultBranch(_ context.Context, _ pressoir.RepoRef) (string, error) {
	return "main", nil
}
func (s *autoMergePressoir) ResolveUser(_ context.Context, _ string) (pressoir.UserID, error) {
	return pressoir.UserID{}, nil
}
func (s *autoMergePressoir) Capabilities(_ context.Context) pressoir.Capabilities {
	return pressoir.Capabilities{}
}
func (s *autoMergePressoir) WorkerGuidance(_, _, _, _, _, _, _ string) string { return "" }

// ── HTTP spy for gitlab.Client ─────────────────────────────────────────────────
//
// mrs.go calls w.GitLab.GetMRApprovals / ListMRDiscussions / GetMR directly on
// *gitlab.Client. We intercept at the HTTP layer (gitlab.Client.HTTP) so we can
// count calls and return controlled responses without a real server.

type gitLabSpy struct {
	approvals      *gitlab.Approvals
	approvalsErr   bool // return 404 instead of body
	discussions    []gitlab.Discussion
	discussionsErr bool

	getApprovalsCount    int
	listDiscussionsCount int
	getMRCount           int
}

type spyTransport struct {
	spy *gitLabSpy
}

func (t *spyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	hdr := http.Header{"Content-Type": []string{"application/json"}}

	respond := func(code int, body string) *http.Response {
		return &http.Response{
			StatusCode: code,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     hdr,
			Request:    req,
		}
	}

	switch {
	case strings.Contains(path, "/approvals"):
		t.spy.getApprovalsCount++
		if t.spy.approvalsErr {
			return respond(http.StatusNotFound, `{"message":"404 Not Found"}`), nil
		}
		b, _ := json.Marshal(t.spy.approvals)
		return respond(http.StatusOK, string(b)), nil

	case strings.Contains(path, "/discussions"):
		t.spy.listDiscussionsCount++
		if t.spy.discussionsErr {
			return respond(http.StatusNotFound, `{"message":"404 Not Found"}`), nil
		}
		b, _ := json.Marshal(t.spy.discussions)
		return respond(http.StatusOK, string(b)), nil

	case strings.Contains(path, "/merge_requests/") &&
		!strings.Contains(path, "/notes") &&
		!strings.Contains(path, "/pipelines") &&
		!strings.Contains(path, "/changes") &&
		!strings.Contains(path, "/closing_issues"):
		t.spy.getMRCount++
		return respond(http.StatusOK, `{}`), nil

	default:
		return respond(http.StatusOK, `{}`), nil
	}
}

// newSpyGitLabClient returns a *gitlab.Client backed by the spy transport.
func newSpyGitLabClient(spy *gitLabSpy) *gitlab.Client {
	c := gitlab.NewClient("gitlab.example.com", "test-token")
	c.HTTP = &http.Client{Transport: &spyTransport{spy: spy}}
	return c
}

// ── Helpers ────────────────────────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

// makeProviderVignoble builds a minimal Vignoble with one vigne configured for
// the given provider ("github" or "gitlab").
func makeProviderVignoble(vigneName, repo, provider string) *config.Vignoble {
	return &config.Vignoble{
		Name: "test",
		Config: &config.VignobleConfig{
			AutoMerge:   true,
			GitLabHost:  "gitlab.example.com",
			Vignes: map[string]config.Vigne{
				vigneName: {
					Repo:      repo,
					AutoMerge: boolPtr(true),
					Pressoir:  config.PressoirConfig{ProviderName: provider},
				},
			},
		},
	}
}

// makeAutoMergeWatcher assembles an MRWatcher for a tryAutoMerge unit test.
func makeAutoMergeWatcher(
	t *testing.T,
	vigneName, repo, provider string,
	p *autoMergePressoir,
	spy *gitLabSpy,
) (*MRWatcher, *state.Store[state.MRWatcherState]) {
	t.Helper()
	dir := t.TempDir()
	mrState, err := state.Load[state.MRWatcherState](filepath.Join(dir, "mr-watcher.yaml"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}

	vignoble := makeProviderVignoble(vigneName, repo, provider)
	resolver := &PressoirResolver{Vignoble: vignoble, Fallback: p}
	resolver.mu.Lock()
	resolver.cache = map[string]pressoir.Pressoir{repo: p}
	resolver.mu.Unlock()

	// A zero-value pnats.Client: Publish will fail to connect (no NATS server)
	// but will not panic — acceptable for unit tests that don't assert NATS delivery.
	w := &MRWatcher{
		State:    mrState,
		NATS:     noopNATS(),
		KV:       newMockKV(),
		Vignoble: vignoble,
		Resolver: resolver,
		GitLab:   newSpyGitLabClient(spy),
	}
	return w, mrState
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestIsGitLabRepo verifies the helper classifies repos by provider.
func TestIsGitLabRepo(t *testing.T) {
	vignoble := &config.Vignoble{
		Name: "test",
		Config: &config.VignobleConfig{
			GitLabHost: "gitlab.example.com",
			Vignes: map[string]config.Vigne{
				"gh-proj": {Repo: "owner/gh-repo", Pressoir: config.PressoirConfig{ProviderName: "github"}},
				"gl-proj": {Repo: "owner/gl-repo", Pressoir: config.PressoirConfig{ProviderName: "gitlab"}},
			},
		},
	}
	w := &MRWatcher{Vignoble: vignoble}

	if w.isGitLabRepo("owner/gh-repo") {
		t.Error("isGitLabRepo(github repo) should return false")
	}
	if !w.isGitLabRepo("owner/gl-repo") {
		t.Error("isGitLabRepo(gitlab repo) should return true")
	}
}

// TestIsGitLabRepo_DefaultsToGitLab verifies that a repo with no explicit provider
// defaults to "gitlab" (the historical default).
func TestIsGitLabRepo_DefaultsToGitLab(t *testing.T) {
	w := &MRWatcher{
		Vignoble: &config.Vignoble{
			Name: "test",
			Config: &config.VignobleConfig{
				GitLabHost: "gitlab.example.com",
				Vignes:     map[string]config.Vigne{"my-proj": {Repo: "group/my-proj"}},
			},
		},
	}
	if !w.isGitLabRepo("group/my-proj") {
		t.Error("isGitLabRepo with no provider config should default to true (gitlab)")
	}
}

// TestTryAutoMerge_GitHub_SkipsDiscussion is the core regression test.
//
// For a GitHub-provider repo with CI passing, approved, and a non-draft PR,
// tryAutoMerge must call Merge() WITHOUT calling ListMRDiscussions on the
// GitLab client. Approval now flows through the pressoir seam.
func TestTryAutoMerge_GitHub_SkipsDiscussion(t *testing.T) {
	const (
		vigneName   = "gh-proj"
		repo        = "owner/gh-repo"
		sessionName = "test-worker-gh"
	)

	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "dev", Draft: false},
		approved: true, // pressoir seam reports approved
	}
	// Spy: discussions would return 404 if the GitLab-only call fires incorrectly.
	spy := &gitLabSpy{discussionsErr: true}

	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "github", p, spy)
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 1},
		}
	})
	entry := &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 1}

	w.tryAutoMerge(sessionName, entry)

	// Merge must have been called.
	if !p.merged {
		t.Fatal("tryAutoMerge: Merge() was NOT called for a GitHub repo with CI passing and approved — auto-merge broken")
	}
	// Approval must have been checked via the pressoir seam.
	if p.approvalCallCount == 0 {
		t.Error("tryAutoMerge: GetApprovalStatus was NOT called via pressoir seam for GitHub repo")
	}
	// GitLab-specific discussion check must NOT have been invoked.
	if spy.listDiscussionsCount != 0 {
		t.Errorf("tryAutoMerge: ListMRDiscussions called %d time(s) for GitHub repo — must be 0", spy.listDiscussionsCount)
	}
}

// TestTryAutoMerge_GitLab_ApprovalCheckBlocksMerge verifies that for a
// GitLab-provider repo, the approval check IS executed via the pressoir seam.
// When the MR is not yet approved, the merge must NOT fire.
func TestTryAutoMerge_GitLab_ApprovalCheckBlocksMerge(t *testing.T) {
	const (
		vigneName   = "gl-proj"
		repo        = "group/gl-repo"
		sessionName = "test-worker-gl-unapproved"
	)

	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "dev", Draft: false},
		approved: false, // not yet approved
	}
	spy := &gitLabSpy{} // no longer consulted for approvals

	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "gitlab", p, spy)
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 2},
		}
	})
	entry := &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 2}

	w.tryAutoMerge(sessionName, entry)

	// Approval check must have run via the pressoir seam.
	if p.approvalCallCount == 0 {
		t.Error("tryAutoMerge: GetApprovalStatus was NOT called via pressoir seam for GitLab repo — approval gate broken")
	}
	// Merge must NOT have fired (not yet approved).
	if p.merged {
		t.Error("tryAutoMerge: Merge() was called on an unapproved GitLab MR — must be blocked")
	}
}

// TestTryAutoMerge_GitLab_DiscussionCheckBlocksMerge verifies that for a GitLab
// repo with an approved MR but an unresolved reviewer thread, the discussion check
// blocks the merge.
func TestTryAutoMerge_GitLab_DiscussionCheckBlocksMerge(t *testing.T) {
	const (
		vigneName   = "gl-proj"
		repo        = "group/gl-repo"
		sessionName = "test-worker-gl-unresolved"
	)

	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "bot", Draft: false},
		approved: true, // approved, so the discussion check is reached
	}
	// Approved via pressoir seam; discussions still checked via GitLab client.
	spy := &gitLabSpy{
		approvals: &gitlab.Approvals{Approved: true},
		discussions: []gitlab.Discussion{
			{
				Notes: []gitlab.Note{
					{
						Resolvable: true,
						Resolved:   false,
						Author:     gitlab.Author{Username: "reviewer"},
					},
				},
			},
		},
	}

	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "gitlab", p, spy)
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 3},
		}
	})
	entry := &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 3}

	w.tryAutoMerge(sessionName, entry)

	// Approval check must have run via pressoir seam.
	if p.approvalCallCount == 0 {
		t.Error("tryAutoMerge: GetApprovalStatus not called for GitLab repo")
	}
	// Discussion check must have run via GitLab client.
	if spy.listDiscussionsCount == 0 {
		t.Error("tryAutoMerge: ListMRDiscussions not called for GitLab repo with unresolved thread")
	}
	// Merge must NOT have fired.
	if p.merged {
		t.Error("tryAutoMerge: Merge() was called despite unresolved reviewer thread — must be blocked")
	}
}

// TestTryAutoMerge_GitLab_MergesWhenApprovedAndClean verifies the happy path for
// GitLab: CI passing, approved, no unresolved threads → merge fires.
func TestTryAutoMerge_GitLab_MergesWhenApprovedAndClean(t *testing.T) {
	const (
		vigneName   = "gl-proj"
		repo        = "group/gl-repo"
		sessionName = "test-worker-gl-clean"
	)

	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "bot", Draft: false},
		approved: true,
	}
	spy := &gitLabSpy{
		approvals:   &gitlab.Approvals{Approved: true},
		discussions: nil, // no threads
	}

	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "gitlab", p, spy)

	// Disable post-merge monitoring so tryAutoMerge calls reapWorker (simpler test).
	f := false
	w.Vignoble.Config.Vignes[vigneName] = config.Vigne{
		Repo:             repo,
		AutoMerge:        boolPtr(true),
		MonitorPostMerge: &f,
		Pressoir:         config.PressoirConfig{ProviderName: "gitlab"},
	}

	kv := newMockKV()
	kv.setAgent(sessionName, map[string]any{"state": "running", "process": "swe"})
	w.KV = kv
	w.Session = &mockSession{}

	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 4},
		}
	})
	entry := &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 4}

	w.tryAutoMerge(sessionName, entry)

	// Merge must have fired.
	if !p.merged {
		t.Error("tryAutoMerge: Merge() was NOT called for an approved+clean GitLab MR")
	}
	// Approval check must have run via pressoir seam.
	if p.approvalCallCount == 0 {
		t.Error("tryAutoMerge: GetApprovalStatus was not called for GitLab repo")
	}
	// Discussion check must have run via GitLab client.
	if spy.listDiscussionsCount == 0 {
		t.Error("tryAutoMerge: ListMRDiscussions was not called for GitLab repo")
	}
}

// TestTryAutoMerge_ApprovalGate_GitLab verifies that for a GitLab repo the
// pressoir seam ApprovalStatus is consulted and blocks the merge when not approved.
func TestTryAutoMerge_ApprovalGate_GitLab(t *testing.T) {
	const (
		vigneName   = "gl-proj"
		repo        = "group/gl-repo"
		sessionName = "gl-gate-blocked"
	)
	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "bot", Draft: false},
		approved: false, // not approved
	}
	spy := &gitLabSpy{}
	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "gitlab", p, spy)
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 10},
		}
	})
	w.tryAutoMerge(sessionName, &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 10})

	if p.approvalCallCount == 0 {
		t.Error("GetApprovalStatus not called via pressoir seam for GitLab repo")
	}
	if p.merged {
		t.Error("Merge() called on unapproved GitLab MR — must be blocked")
	}
}

// TestTryAutoMerge_ApprovalGate_GitHub verifies that for a GitHub repo the
// pressoir seam ApprovalStatus is consulted; when not approved, merge is blocked;
// when approved, merge proceeds.
func TestTryAutoMerge_ApprovalGate_GitHub(t *testing.T) {
	t.Run("blocks_when_not_approved", func(t *testing.T) {
		const (
			vigneName   = "gh-proj"
			repo        = "owner/gh-repo"
			sessionName = "gh-gate-blocked"
		)
		p := &autoMergePressoir{
			ciState:  "success",
			pr:       pressoir.PullRequest{State: "opened", Author: "dev", Draft: false},
			approved: false,
		}
		spy := &gitLabSpy{}
		w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "github", p, spy)
		mrState.Update(func(s *state.MRWatcherState) {
			s.Watched = map[string]*state.WatchedMR{
				sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 20},
			}
		})
		w.tryAutoMerge(sessionName, &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 20})

		if p.approvalCallCount == 0 {
			t.Error("GetApprovalStatus not called via pressoir seam for GitHub repo")
		}
		if p.merged {
			t.Error("Merge() called on unapproved GitHub PR — must be blocked")
		}
	})

	t.Run("falls_through_on_approval_error", func(t *testing.T) {
		const (
			vigneName   = "gh-proj"
			repo        = "owner/gh-repo"
			sessionName = "gh-gate-error"
		)
		p := &autoMergePressoir{
			ciState:     "success",
			pr:          pressoir.PullRequest{State: "opened", Author: "dev", Draft: false},
			approvalErr: fmt.Errorf("PAT lacks admin scope"),
		}
		spy := &gitLabSpy{}
		w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "github", p, spy)
		mrState.Update(func(s *state.MRWatcherState) {
			s.Watched = map[string]*state.WatchedMR{
				sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 21},
			}
		})
		w.tryAutoMerge(sessionName, &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 21})

		// GitHub: on approval error, fall through and attempt Merge (GitHub enforces
		// branch protection at merge time).
		if !p.merged {
			t.Error("Merge() not called for GitHub PR when approval read errored — should fall through to Merge")
		}
	})
}

// TestTryAutoMerge_GitHub_CIOnly_NoReviews_Merges guards the #279 baseline:
// a GitHub repo with no branch-protection review requirement (Required=0) and
// zero reviews must still auto-merge when CI passes.
// GetApprovalStatus returns Approved=true (0 >= 0 && !ChangesRequested).
func TestTryAutoMerge_GitHub_CIOnly_NoReviews_Merges(t *testing.T) {
	const (
		vigneName   = "gh-proj"
		repo        = "owner/gh-repo"
		sessionName = "gh-ci-only"
	)
	p := &autoMergePressoir{
		ciState:  "success",
		pr:       pressoir.PullRequest{State: "opened", Author: "dev", Draft: false},
		approved: true, // Required=0 + no reviews → Approved=true from the seam
	}
	spy := &gitLabSpy{}
	w, mrState := makeAutoMergeWatcher(t, vigneName, repo, "github", p, spy)
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			sessionName: {Name: sessionName, Project: vigneName, Repo: repo, MR: 30},
		}
	})
	w.tryAutoMerge(sessionName, &state.WatchedMR{Name: sessionName, Project: vigneName, Repo: repo, MR: 30})

	// Must have checked approval via seam.
	if p.approvalCallCount == 0 {
		t.Error("GetApprovalStatus not called for GitHub CI-only repo")
	}
	// Must have merged — this is the #279 regression guard.
	if !p.merged {
		t.Error("Merge() NOT called for GitHub CI-only repo with CI passing and Required=0 — auto-merge broken")
	}
}
