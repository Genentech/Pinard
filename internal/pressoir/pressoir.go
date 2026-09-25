package pressoir

import "context"

// RepoRef identifies a repository on any pressoir host.
// For GitLab, Owner may be a subgroup path (e.g. "group/sub").
type RepoRef struct {
	Host  string
	Owner string
	Name  string
}

// PullRequest is the neutral change-request model (GitHub-shaped).
type PullRequest struct {
	Number       int
	State        string // open | merged | closed
	Title        string
	Body         string
	Labels       []string
	SourceBranch string
	WebURL       string
	Author       string
	MergeSHA     string
	HeadSHA      string // current HEAD commit SHA (used for SHA-keyed review idempotency)
	Draft        bool   // true when the PR is a draft / work-in-progress
}

// Issue is the neutral issue model.
type Issue struct {
	Number int
	State  string
	Title  string
	Body   string
	Labels []string
	WebURL string
	Author string
}

// InlineComment locates a comment on a specific file line.
// Side is "left" (old) or "right" (new).
type InlineComment struct {
	Path string
	Line int
	Side string
}

// Comment is a PR or issue comment, optionally inline.
type Comment struct {
	ID            int
	Body          string
	Author        string
	InlineComment *InlineComment // nil for top-level comments
	DiscussionID  string
}

// Review is a PR review (approve / request-changes / comment).
type Review struct {
	ID       int
	Author   string
	State    string // approved | changes_requested | commented
	Body     string
	Comments []InlineComment
}

// ApprovalStatus is the provider-neutral approval gate result for a pull request.
// Required is the minimum approvals needed (0 when unknown or unreadable — degrade gracefully).
type ApprovalStatus struct {
	Approved         bool
	ApprovedBy       []string
	ChangesRequested bool
	Required         int
}

// CIStatus is the neutral CI gate result.
// State: success | failed | running | pending | none
type CIStatus struct {
	State  string
	WebURL string
}

// Capabilities reports which optional pressoir primitives are available.
type Capabilities struct {
	HasEpics bool
}

// IssueFilter constrains ListIssues.
type IssueFilter struct {
	Assignee string
	Label    string
	State    string // open | closed | all
}

// MergeOpts controls how a PR is merged.
type MergeOpts struct {
	MergeCommitMessage string
}

// UserID is a resolved pressoir user identity.
type UserID struct {
	Username string
	ID       int // numeric provider-specific user ID (GitLab user ID, GitHub user ID, …)
}

// Discussion is a thread of comments on a PR.
type Discussion struct {
	ID       string
	Comments []Comment
}

// Pressoir is the provider-neutral interface for all git host operations.
// Callers never branch on the provider — they call Pressoir methods only.
type Pressoir interface {
	// Pull request operations
	OpenPR(ctx context.Context, repo RepoRef, src, dst, title, body string, draft bool) (PullRequest, error)
	ListOpenPRs(ctx context.Context, repo RepoRef) ([]PullRequest, error)
	GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error)
	UpdatePR(ctx context.Context, repo RepoRef, number int, params map[string]string) error
	Merge(ctx context.Context, repo RepoRef, number int, opts MergeOpts) error
	Approve(ctx context.Context, repo RepoRef, number int) error
	GetApprovalStatus(ctx context.Context, repo RepoRef, number int) (ApprovalStatus, error)

	// PR comments and reviews
	Comment(ctx context.Context, repo RepoRef, number int, body string) error
	PostPRNote(ctx context.Context, repo RepoRef, number int, body string) error
	InlineComment(ctx context.Context, repo RepoRef, number int, c InlineComment, body string) error
	ListPRNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error)
	ListPRDiscussions(ctx context.Context, repo RepoRef, number int) ([]Discussion, error)
	GetPRChanges(ctx context.Context, repo RepoRef, number int) ([]string, error)
	GetPRClosingIssues(ctx context.Context, repo RepoRef, number int) ([]Issue, error)

	// CI
	CIStatusFor(ctx context.Context, repo RepoRef, number int) (CIStatus, error)
	ListPipelinesByCommit(ctx context.Context, repo RepoRef, sha string) ([]CIStatus, error)

	// Issue operations
	ListIssues(ctx context.Context, repo RepoRef, filter IssueFilter) ([]Issue, error)
	GetIssue(ctx context.Context, repo RepoRef, number int) (Issue, error)
	CreateIssue(ctx context.Context, repo RepoRef, params map[string]string) (Issue, error)
	UpdateIssue(ctx context.Context, repo RepoRef, number int, params map[string]string) error
	PostIssueNote(ctx context.Context, repo RepoRef, number int, body string) error
	SetLabels(ctx context.Context, repo RepoRef, number int, labels []string) error
	// AddLabel and RemoveLabel are additive/subtractive label operations —
	// unlike SetLabels (replace-all), they never disturb other labels already
	// on the issue/PR (e.g. auto-merge, parcelle:*).
	AddLabel(ctx context.Context, repo RepoRef, number int, label string) error
	RemoveLabel(ctx context.Context, repo RepoRef, number int, label string) error
	ListIssueNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error)
	LinkIssues(ctx context.Context, repo RepoRef, number int, targetRepo RepoRef, targetNumber int, linkType string) error
	// AddSubIssue attaches childNumber as a sub-issue of parentNumber.
	// On providers that support native sub-issues (GitHub), the native API is attempted first;
	// on failure or when unsupported, the parent body is updated with a task-list entry.
	// Never returns an error for the "sub-issues unavailable" case.
	AddSubIssue(ctx context.Context, repo RepoRef, parentNumber, childNumber int) error

	// Repository
	GetDefaultBranch(ctx context.Context, repo RepoRef) (string, error)

	// User
	ResolveUser(ctx context.Context, username string) (UserID, error)

	// Provider capabilities
	Capabilities(ctx context.Context) Capabilities

	// WorkerGuidance returns provider-specific instructions for a worker prompt:
	// how to open a pull/merge request, post comments, and notify the conductor.
	// repo is the "owner/name" path; host, targetBranch, encodedRepo, user, project,
	// and name are injected by the caller from spawn-time context.
	WorkerGuidance(repo, host, encodedRepo, targetBranch, user, project, name string) string
}
