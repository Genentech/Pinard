package watcher

import (
	"context"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
)

// stubPressoir is a minimal Pressoir implementation used in tests.
type stubPressoir struct{ name string }

func (s *stubPressoir) OpenPR(_ context.Context, _ pressoir.RepoRef, _, _, _, _ string, _ bool) (pressoir.PullRequest, error) {
	return pressoir.PullRequest{}, nil
}
func (s *stubPressoir) ListOpenPRs(_ context.Context, _ pressoir.RepoRef) ([]pressoir.PullRequest, error) {
	return nil, nil
}
func (s *stubPressoir) GetPR(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.PullRequest, error) {
	return pressoir.PullRequest{}, nil
}
func (s *stubPressoir) UpdatePR(_ context.Context, _ pressoir.RepoRef, _ int, _ map[string]string) error {
	return nil
}
func (s *stubPressoir) Merge(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.MergeOpts) error {
	return nil
}
func (s *stubPressoir) Approve(_ context.Context, _ pressoir.RepoRef, _ int) error { return nil }
func (s *stubPressoir) GetApprovalStatus(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.ApprovalStatus, error) {
	return pressoir.ApprovalStatus{}, nil
}
func (s *stubPressoir) Comment(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) PostPRNote(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) InlineComment(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.InlineComment, _ string) error {
	return nil
}
func (s *stubPressoir) ListPRNotes(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Comment, error) {
	return nil, nil
}
func (s *stubPressoir) ListPRDiscussions(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Discussion, error) {
	return nil, nil
}
func (s *stubPressoir) GetPRChanges(_ context.Context, _ pressoir.RepoRef, _ int) ([]string, error) {
	return nil, nil
}
func (s *stubPressoir) GetPRClosingIssues(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Issue, error) {
	return nil, nil
}
func (s *stubPressoir) CIStatusFor(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.CIStatus, error) {
	return pressoir.CIStatus{}, nil
}
func (s *stubPressoir) ListPipelinesByCommit(_ context.Context, _ pressoir.RepoRef, _ string) ([]pressoir.CIStatus, error) {
	return nil, nil
}
func (s *stubPressoir) ListIssues(_ context.Context, _ pressoir.RepoRef, _ pressoir.IssueFilter) ([]pressoir.Issue, error) {
	return nil, nil
}
func (s *stubPressoir) GetIssue(_ context.Context, _ pressoir.RepoRef, _ int) (pressoir.Issue, error) {
	return pressoir.Issue{}, nil
}
func (s *stubPressoir) CreateIssue(_ context.Context, _ pressoir.RepoRef, _ map[string]string) (pressoir.Issue, error) {
	return pressoir.Issue{}, nil
}
func (s *stubPressoir) UpdateIssue(_ context.Context, _ pressoir.RepoRef, _ int, _ map[string]string) error {
	return nil
}
func (s *stubPressoir) PostIssueNote(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) SetLabels(_ context.Context, _ pressoir.RepoRef, _ int, _ []string) error {
	return nil
}
func (s *stubPressoir) AddLabel(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) RemoveLabel(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) ListIssueNotes(_ context.Context, _ pressoir.RepoRef, _ int) ([]pressoir.Comment, error) {
	return nil, nil
}
func (s *stubPressoir) LinkIssues(_ context.Context, _ pressoir.RepoRef, _ int, _ pressoir.RepoRef, _ int, _ string) error {
	return nil
}
func (s *stubPressoir) AddSubIssue(_ context.Context, _ pressoir.RepoRef, _, _ int) error {
	return nil
}
func (s *stubPressoir) GetDefaultBranch(_ context.Context, _ pressoir.RepoRef) (string, error) {
	return "main", nil
}
func (s *stubPressoir) ResolveUser(_ context.Context, _ string) (pressoir.UserID, error) {
	return pressoir.UserID{}, nil
}
func (s *stubPressoir) Capabilities(_ context.Context) pressoir.Capabilities {
	return pressoir.Capabilities{}
}
func (s *stubPressoir) WorkerGuidance(_, _, _, _, _, _, _ string) string { return "" }

// TestPressoirResolver_FallbackOnError verifies that when per-repo resolution
// fails (unknown provider), the fallback adapter is returned.
// ResolvePressoirConfig looks up vignes by vigne name (the map key), not by
// repo path, so we pass the vigne name as the key and as the repo arg here.
func TestPressoirResolver_FallbackOnError(t *testing.T) {
	fallback := &stubPressoir{name: "fallback"}
	// Set a vignoble-level unknown provider so any repo lookup triggers the error.
	r := &PressoirResolver{
		Vignoble: &config.Vignoble{
			Config: &config.VignobleConfig{
				GitLabHost: "gitlab.example.com",
				Pressoir: config.PressoirConfig{
					ProviderName: "unknown-provider", // will fail NewPressoir
				},
				Vignes: map[string]config.Vigne{},
			},
		},
		Creds:    &config.Credentials{},
		Fallback: fallback,
	}

	got := r.For("any-repo")
	if got != fallback {
		t.Errorf("For() should return fallback on resolution error, got %T", got)
	}
}

// TestPressoirResolver_Memoized verifies that the same adapter is returned on
// repeated calls for the same repo (no extra NewPressoir calls).
func TestPressoirResolver_Memoized(t *testing.T) {
	fallback := &stubPressoir{name: "fallback"}
	r := &PressoirResolver{
		Vignoble: &config.Vignoble{
			Config: &config.VignobleConfig{
				GitLabHost: "gitlab.example.com",
				Vignes:     map[string]config.Vigne{},
			},
		},
		Creds:    &config.Credentials{},
		Fallback: fallback,
	}

	a1 := r.For("mygroup/some-repo")
	a2 := r.For("mygroup/some-repo")
	if a1 != a2 {
		t.Errorf("For() returned different adapters on repeated calls (not memoized)")
	}
}

// TestPressoirResolver_GitLabDefault verifies that a repo with no pressoir override
// resolves to a non-nil GitLab adapter (default behaviour unchanged).
func TestPressoirResolver_GitLabDefault(t *testing.T) {
	fallback := &stubPressoir{name: "fallback"}
	r := &PressoirResolver{
		Vignoble: &config.Vignoble{
			Config: &config.VignobleConfig{
				GitLabHost: "gitlab.example.com",
				Vignes:     map[string]config.Vigne{},
			},
		},
		Creds:    &config.Credentials{},
		Fallback: fallback,
	}

	got := r.For("mygroup/legacy-repo")
	if got == nil {
		t.Fatal("For() returned nil for a default-gitlab repo")
	}
	// Should be a GitLabAdapter (not the fallback stub), since provider=gitlab resolves fine.
	if _, isStub := got.(*stubPressoir); isStub {
		t.Errorf("For() returned the fallback stub for a default-gitlab repo; expected GitLabAdapter")
	}
}
