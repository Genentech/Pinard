package pressoir

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Genentech/pinard/internal/gitlab"
)

// GitLabAdapter implements Pressoir by wrapping internal/gitlab.Client.
// For GitLab, Number == iid (both PR and Issue iids are per-project monotonic integers).
type GitLabAdapter struct {
	client *gitlab.Client
}

// compile-time assertion: GitLabAdapter must satisfy Pressoir.
var _ Pressoir = (*GitLabAdapter)(nil)

// NewGitLabAdapter creates a Pressoir backed by GitLab.
func NewGitLabAdapter(host, token string) *GitLabAdapter {
	return &GitLabAdapter{client: gitlab.NewClient(host, token)}
}

// repoPath returns "owner/name" for use in gitlab API calls.
func repoPath(repo RepoRef) string {
	if repo.Owner == "" {
		return repo.Name
	}
	return repo.Owner + "/" + repo.Name
}

// mrToPR maps a gitlab.MergeRequest to the neutral PullRequest model.
func mrToPR(mr gitlab.MergeRequest) PullRequest {
	return PullRequest{
		Number:       mr.IID,
		State:        mr.State,
		Title:        mr.Title,
		Body:         mr.Description,
		Labels:       mr.Labels,
		SourceBranch: mr.SourceBranch,
		WebURL:       mr.WebURL,
		Author:       mr.Author.Username,
		MergeSHA:     mr.MergeCommitSHA,
		HeadSHA:      mr.DiffRefs.HeadSHA,
		Draft:        mr.Draft || mr.WorkInProgress,
	}
}

// issueToDomain maps a gitlab.Issue to the neutral Issue model.
func issueToDomain(iss gitlab.Issue) Issue {
	return Issue{
		Number: iss.IID,
		State:  iss.State,
		Title:  iss.Title,
		Body:   iss.Description,
		Labels: iss.Labels,
		WebURL: iss.WebURL,
		Author: iss.Author.Username,
	}
}

// noteToComment maps a gitlab.Note to the neutral Comment model.
// A note with a non-empty Position.NewPath is an inline comment.
func noteToComment(n gitlab.Note) Comment {
	c := Comment{
		ID:           n.ID,
		Body:         n.Body,
		Author:       n.Author.Username,
		DiscussionID: n.DiscussionID,
	}
	if n.Position.NewPath != "" {
		c.InlineComment = &InlineComment{
			Path: n.Position.NewPath,
			Line: n.Position.NewLine,
			Side: "right",
		}
	}
	return c
}

// pipelineToCIStatus maps a gitlab.Pipeline to the neutral CIStatus.
func pipelineToCIStatus(p gitlab.Pipeline) CIStatus {
	state := mapPipelineState(p.Status)
	return CIStatus{State: state, WebURL: p.WebURL}
}

func mapPipelineState(status string) string {
	switch status {
	case "success":
		return "success"
	case "failed", "canceled":
		return "failed"
	case "running":
		return "running"
	case "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		return "pending"
	default:
		return "none"
	}
}

// — Pull request operations —

func (a *GitLabAdapter) OpenPR(ctx context.Context, repo RepoRef, src, dst, title, body string, draft bool) (PullRequest, error) {
	params := map[string]string{
		"source_branch": src,
		"target_branch": dst,
		"title":         title,
		"description":   body,
	}
	if draft {
		params["title"] = "Draft: " + title
	}
	raw, err := a.client.CreateMR(repoPath(repo), params)
	if err != nil {
		return PullRequest{}, err
	}
	var mr gitlab.MergeRequest
	if err := json.Unmarshal(raw, &mr); err != nil {
		return PullRequest{}, fmt.Errorf("pressoir/gitlab: parse CreateMR response: %w", err)
	}
	return mrToPR(mr), nil
}

func (a *GitLabAdapter) ListOpenPRs(ctx context.Context, repo RepoRef) ([]PullRequest, error) {
	mrs, err := a.client.ListAllOpenMRs(repoPath(repo))
	if err != nil {
		return nil, err
	}
	prs := make([]PullRequest, len(mrs))
	for i, mr := range mrs {
		prs[i] = mrToPR(mr)
	}
	return prs, nil
}

func (a *GitLabAdapter) GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error) {
	mr, err := a.client.GetMR(repoPath(repo), number)
	if err != nil {
		return PullRequest{}, err
	}
	return mrToPR(*mr), nil
}

func (a *GitLabAdapter) UpdatePR(ctx context.Context, repo RepoRef, number int, params map[string]string) error {
	return a.client.UpdateMR(repoPath(repo), number, params)
}

func (a *GitLabAdapter) Merge(ctx context.Context, repo RepoRef, number int, opts MergeOpts) error {
	if opts.MergeCommitMessage != "" {
		return a.client.MergeMRWithMessage(repoPath(repo), number, opts.MergeCommitMessage)
	}
	return a.client.MergeMR(repoPath(repo), number)
}

func (a *GitLabAdapter) Approve(ctx context.Context, repo RepoRef, number int) error {
	return a.client.ApproveMR(repoPath(repo), number)
}

func (a *GitLabAdapter) GetApprovalStatus(_ context.Context, repo RepoRef, number int) (ApprovalStatus, error) {
	approvals, err := a.client.GetMRApprovals(repoPath(repo), number)
	if err != nil {
		return ApprovalStatus{}, fmt.Errorf("pressoir/gitlab: get approvals: %w", err)
	}
	by := make([]string, 0, len(approvals.ApprovedBy))
	for _, u := range approvals.ApprovedBy {
		by = append(by, u.Username)
	}
	return ApprovalStatus{
		Approved:   approvals.Approved,
		ApprovedBy: by,
		// Required: GitLab's min-approvals is not surfaced here; Approved bool is
		// authoritative for the auto-merge gate.
		Required: 0,
	}, nil
}

// — PR comments and reviews —

func (a *GitLabAdapter) Comment(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.client.PostMRNote(repoPath(repo), number, body)
}

func (a *GitLabAdapter) PostPRNote(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.client.PostMRNote(repoPath(repo), number, body)
}

func (a *GitLabAdapter) InlineComment(ctx context.Context, repo RepoRef, number int, c InlineComment, body string) error {
	// Fetch the MR to obtain the diff refs (base/start/head SHAs) required by
	// GitLab's position-based discussion API.
	mr, err := a.client.GetMR(repoPath(repo), number)
	if err != nil {
		// Fall back to a plain note when we cannot get MR details.
		return a.client.PostMRNote(repoPath(repo), number, fmt.Sprintf("[%s:%d] %s", c.Path, c.Line, body))
	}
	oldPath := c.Path
	newPath := c.Path
	var newLine, oldLine int
	if c.Side == "left" {
		oldLine = c.Line
	} else {
		newLine = c.Line
	}
	pos := &gitlab.MRDiscussionPosition{
		BaseSHA:  mr.DiffRefs.BaseSHA,
		StartSHA: mr.DiffRefs.StartSHA,
		HeadSHA:  mr.DiffRefs.HeadSHA,
		NewPath:  newPath,
		OldPath:  oldPath,
		NewLine:  newLine,
		OldLine:  oldLine,
	}
	if err := a.client.PostMRDiscussion(repoPath(repo), number, body, pos); err != nil {
		// Fall back to a plain note if position-based discussion fails
		// (e.g. the diff position is no longer valid).
		return a.client.PostMRNote(repoPath(repo), number, fmt.Sprintf("[%s:%d] %s", c.Path, c.Line, body))
	}
	return nil
}

func (a *GitLabAdapter) ListPRNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error) {
	notes, err := a.client.ListMRNotes(repoPath(repo), number)
	if err != nil {
		return nil, err
	}
	comments := make([]Comment, len(notes))
	for i, n := range notes {
		comments[i] = noteToComment(n)
	}
	return comments, nil
}

func (a *GitLabAdapter) ListPRDiscussions(ctx context.Context, repo RepoRef, number int) ([]Discussion, error) {
	glDiscs, err := a.client.ListMRDiscussions(repoPath(repo), number)
	if err != nil {
		return nil, err
	}
	discs := make([]Discussion, len(glDiscs))
	for i, d := range glDiscs {
		comments := make([]Comment, len(d.Notes))
		for j, n := range d.Notes {
			comments[j] = noteToComment(n)
		}
		discs[i] = Discussion{ID: d.ID, Comments: comments}
	}
	return discs, nil
}

func (a *GitLabAdapter) GetPRChanges(ctx context.Context, repo RepoRef, number int) ([]string, error) {
	return a.client.GetMRChanges(repoPath(repo), number)
}

func (a *GitLabAdapter) GetPRClosingIssues(ctx context.Context, repo RepoRef, number int) ([]Issue, error) {
	glIssues, err := a.client.GetMRClosingIssues(repoPath(repo), number)
	if err != nil {
		return nil, err
	}
	issues := make([]Issue, len(glIssues))
	for i, iss := range glIssues {
		issues[i] = issueToDomain(iss)
	}
	return issues, nil
}

// — CI —

func (a *GitLabAdapter) CIStatusFor(ctx context.Context, repo RepoRef, number int) (CIStatus, error) {
	pipelines, err := a.client.ListMRPipelines(repoPath(repo), number)
	if err != nil {
		return CIStatus{State: "none"}, err
	}
	if len(pipelines) == 0 {
		return CIStatus{State: "none"}, nil
	}
	// Most recent pipeline is last in the list.
	return pipelineToCIStatus(pipelines[len(pipelines)-1]), nil
}

func (a *GitLabAdapter) ListPipelinesByCommit(ctx context.Context, repo RepoRef, sha string) ([]CIStatus, error) {
	pipelines, err := a.client.ListPipelinesByCommit(repoPath(repo), sha)
	if err != nil {
		return nil, err
	}
	statuses := make([]CIStatus, len(pipelines))
	for i, p := range pipelines {
		statuses[i] = pipelineToCIStatus(p)
	}
	return statuses, nil
}

// — Issue operations —

func (a *GitLabAdapter) ListIssues(ctx context.Context, repo RepoRef, filter IssueFilter) ([]Issue, error) {
	var glIssues []gitlab.Issue
	var err error
	if filter.Label != "" {
		glIssues, err = a.client.ListIssuesByLabel(repoPath(repo), filter.Label)
	} else {
		glIssues, err = a.client.ListIssues(repoPath(repo), filter.Assignee)
	}
	if err != nil {
		return nil, err
	}
	issues := make([]Issue, len(glIssues))
	for i, iss := range glIssues {
		issues[i] = issueToDomain(iss)
	}
	return issues, nil
}

func (a *GitLabAdapter) GetIssue(ctx context.Context, repo RepoRef, number int) (Issue, error) {
	iss, err := a.client.GetIssue(repoPath(repo), number)
	if err != nil {
		return Issue{}, err
	}
	return issueToDomain(*iss), nil
}

func (a *GitLabAdapter) CreateIssue(ctx context.Context, repo RepoRef, params map[string]string) (Issue, error) {
	raw, err := a.client.CreateIssue(repoPath(repo), params)
	if err != nil {
		return Issue{}, err
	}
	var iss gitlab.Issue
	if err := json.Unmarshal(raw, &iss); err != nil {
		return Issue{}, fmt.Errorf("pressoir/gitlab: parse CreateIssue response: %w", err)
	}
	return issueToDomain(iss), nil
}

func (a *GitLabAdapter) UpdateIssue(ctx context.Context, repo RepoRef, number int, params map[string]string) error {
	return a.client.UpdateIssue(repoPath(repo), number, params)
}

func (a *GitLabAdapter) PostIssueNote(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.client.PostIssueNote(repoPath(repo), number, body)
}

func (a *GitLabAdapter) SetLabels(ctx context.Context, repo RepoRef, number int, labels []string) error {
	return a.client.UpdateIssue(repoPath(repo), number, map[string]string{
		"labels": strings.Join(labels, ","),
	})
}

// AddLabel and RemoveLabel target the merge_requests endpoint (not issues,
// which SetLabels above uses) via add_labels/remove_labels params, so they
// work correctly on MRs and never replace the existing label set.
func (a *GitLabAdapter) AddLabel(ctx context.Context, repo RepoRef, number int, label string) error {
	return a.client.UpdateMR(repoPath(repo), number, map[string]string{
		"add_labels": label,
	})
}

func (a *GitLabAdapter) RemoveLabel(ctx context.Context, repo RepoRef, number int, label string) error {
	return a.client.UpdateMR(repoPath(repo), number, map[string]string{
		"remove_labels": label,
	})
}

func (a *GitLabAdapter) ListIssueNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error) {
	notes, err := a.client.ListIssueNotes(repoPath(repo), number)
	if err != nil {
		return nil, err
	}
	comments := make([]Comment, 0, len(notes))
	for _, n := range notes {
		if !n.System {
			comments = append(comments, noteToComment(n))
		}
	}
	return comments, nil
}

func (a *GitLabAdapter) LinkIssues(ctx context.Context, repo RepoRef, number int, targetRepo RepoRef, targetNumber int, linkType string) error {
	if linkType == "" {
		linkType = "relates_to"
	}
	return a.client.CreateIssueLink(repoPath(repo), number, repoPath(targetRepo), targetNumber, linkType)
}

// AddSubIssue creates a "relates_to" issue link between parentNumber and childNumber.
// GitLab has native epics but no group-epic client here; routing through issue links
// is the closest equivalent without introducing a new GitLab API surface.
// Returns nil (non-erroring) when the link cannot be created.
func (a *GitLabAdapter) AddSubIssue(ctx context.Context, repo RepoRef, parentNumber, childNumber int) error {
	_ = a.client.CreateIssueLink(repoPath(repo), parentNumber, repoPath(repo), childNumber, "relates_to")
	return nil
}

func (a *GitLabAdapter) GetDefaultBranch(ctx context.Context, repo RepoRef) (string, error) {
	project, err := a.client.GetProject(repoPath(repo))
	if err != nil {
		return "", err
	}
	if branch, ok := project["default_branch"].(string); ok && branch != "" {
		return branch, nil
	}
	return "main", nil
}

// — User —

func (a *GitLabAdapter) ResolveUser(ctx context.Context, username string) (UserID, error) {
	numericID, err := a.client.GetUserNumericID(username)
	if err != nil {
		return UserID{}, err
	}
	return UserID{Username: username, ID: numericID}, nil
}

// — Capabilities —

func (a *GitLabAdapter) Capabilities(ctx context.Context) Capabilities {
	return Capabilities{HasEpics: true}
}

// — Worker guidance —

func (a *GitLabAdapter) WorkerGuidance(repo, host, encodedRepo, targetBranch, user, project, name string) string {
	return fmt.Sprintf(`- To open a merge request use: aoc pressoir open-pr --repo %s --src $(git branch --show-current) --dst %s --title "your title" --body "your description"
- MR titles must start with fix:/feat: (scope optional): "fix(ci): …", "feat: …". Other prefixes (docs:, chore:, ops:) are rejected.
- When you open an MR: (1) call track_mr with the MR number so review comments reach you, (2) run: aoc notify "[%s] Opened MR !<number> on %s: https://%s/%s/-/merge_requests/<number>"
- When you finish a task or address review feedback, run: aoc notify "[%s] <summary of what you did>"
- To comment on a merge request: aoc pressoir comment-pr --repo %s --number <iid> --body "<comment>"
- Comment on the MR when you start working, after each significant change, and when you finish.
- Git host CLI: %s (set as $GITLAB_HOST). For operations not covered by aoc pressoir, use: glab api projects/%s/<endpoint> --hostname %s`,
		repo, targetBranch,
		name, project, host, repo,
		name,
		repo,
		host, encodedRepo, host)
}
