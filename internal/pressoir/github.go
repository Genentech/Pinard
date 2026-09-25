package pressoir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimitError is returned when GitHub signals that the rate limit has been
// exceeded (403 with X-RateLimit-Remaining: 0 or a Retry-After header).
// Callers can inspect ResetAt to sleep until quota is restored.
type RateLimitError struct {
	// ResetAt is the earliest time at which the caller may retry.
	ResetAt time.Time
	msg     string
}

func (e *RateLimitError) Error() string { return e.msg }

// etagEntry caches a response body alongside its ETag for conditional GETs.
type etagEntry struct {
	etag string
	body []byte
}

// GitHubAdapter implements Pressoir using the GitHub REST API.
// Auth: fine-grained PAT via Authorization: Bearer <token>.
// Number == GitHub PR/issue number (no iid translation needed).
type GitHubAdapter struct {
	restHost   string // REST API host, e.g. "api.github.com" or "ghe.example.com/api/v3"
	graphqlURL string // full GraphQL endpoint URL, e.g. "https://api.github.com/graphql"
	token      string
	httpClient *http.Client

	cacheMu   sync.Mutex
	etagCache map[string]etagEntry // keyed by full URL path
}

// compile-time assertion: GitHubAdapter must satisfy Pressoir.
var _ Pressoir = (*GitHubAdapter)(nil)

// normalizeGitHubHost maps a user-supplied host string to the REST API host
// (used as the base for https://<restHost>/<path>) and the full GraphQL URL.
//
//   - "" or "github.com"  → restHost="api.github.com", graphql="https://api.github.com/graphql"
//   - any other host h    → restHost="h/api/v3",        graphql="https://h/api/graphql"
func normalizeGitHubHost(host string) (restHost, graphqlURL string) {
	if host == "" || host == "github.com" {
		return "api.github.com", "https://api.github.com/graphql"
	}
	return host + "/api/v3", "https://" + host + "/api/graphql"
}

// NewGitHubAdapter creates a Pressoir backed by GitHub REST API.
// host is the user-facing host (e.g. "github.com" or a GHE hostname).
// Pass "" to use the default github.com endpoints.
func NewGitHubAdapter(host, token string) *GitHubAdapter {
	restHost, graphqlURL := normalizeGitHubHost(host)
	return &GitHubAdapter{
		restHost:   restHost,
		graphqlURL: graphqlURL,
		token:      token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		etagCache: make(map[string]etagEntry),
	}
}

// — GitHub JSON types —

type ghUser struct {
	Login string `json:"login"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghHeadRef struct {
	SHA string `json:"sha"`
	Ref string `json:"ref"`
}

type ghPR struct {
	Number  int       `json:"number"`
	State   string    `json:"state"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Draft   bool      `json:"draft"`
	Merged  bool      `json:"merged"`
	HTMLURL string    `json:"html_url"`
	User    ghUser    `json:"user"`
	Labels  []ghLabel `json:"labels"`
	Head    ghHeadRef `json:"head"`
	MergeSHA *string  `json:"merge_commit_sha"`
}

type ghIssue struct {
	ID      int64     `json:"id"`
	Number  int       `json:"number"`
	State   string    `json:"state"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	HTMLURL string    `json:"html_url"`
	User    ghUser    `json:"user"`
	Labels  []ghLabel `json:"labels"`
}

type ghComment struct {
	ID      int    `json:"id"`
	Body    string `json:"body"`
	User    ghUser `json:"user"`
	HTMLURL string `json:"html_url"`
}

type ghReviewComment struct {
	ID                  int    `json:"id"`
	Body                string `json:"body"`
	User                ghUser `json:"user"`
	Path                string `json:"path"`
	Line                int    `json:"line"`
	OriginalLine        int    `json:"original_line"`
	Side                string `json:"side"` // "LEFT" or "RIGHT"
	PullRequestReviewID int64  `json:"pull_request_review_id"`
	HTMLURL             string `json:"html_url"`
}

type ghCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | cancelled | skipped | timed_out | action_required
	HTMLURL    string `json:"html_url"`
}

type ghCheckRunsResp struct {
	TotalCount int          `json:"total_count"`
	CheckRuns  []ghCheckRun `json:"check_runs"`
}

type ghWorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`     // queued | in_progress | completed
	Conclusion string `json:"conclusion"` // success | failure | neutral | cancelled | skipped | timed_out | action_required
	HTMLURL    string `json:"html_url"`
}

type ghWorkflowRunsResp struct {
	TotalCount   int               `json:"total_count"`
	WorkflowRuns []ghWorkflowRun   `json:"workflow_runs"`
}

type ghPRFile struct {
	Filename string `json:"filename"`
}

type ghReview struct {
	ID          int64  `json:"id"`
	User        ghUser `json:"user"`
	State       string `json:"state"` // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	SubmittedAt string `json:"submitted_at"`
}

type ghBranchProtection struct {
	RequiredPullRequestReviews struct {
		RequiredApprovingReviewCount int `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
}

// — HTTP helpers —

func (a *GitHubAdapter) apiURL(path string) string {
	return fmt.Sprintf("https://%s/%s", a.restHost, path)
}

func (a *GitHubAdapter) ghDo(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.apiURL(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusForbidden {
		if rle := parseRateLimitError(resp); rle != nil {
			resp.Body.Close()
			return nil, rle
		}
	}
	return resp, nil
}

// parseRateLimitError inspects a 403 response for GitHub rate-limit signals.
// Returns a *RateLimitError when X-RateLimit-Remaining is "0" or a Retry-After
// header is present; returns nil otherwise (ordinary 403).
func parseRateLimitError(resp *http.Response) *RateLimitError {
	// Primary rate limit: X-RateLimit-Remaining == 0.
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		var resetAt time.Time
		if s := resp.Header.Get("X-RateLimit-Reset"); s != "" {
			if unix, err := strconv.ParseInt(s, 10, 64); err == nil {
				resetAt = time.Unix(unix, 0)
			}
		}
		if resetAt.IsZero() {
			resetAt = time.Now().Add(60 * time.Second)
		}
		return &RateLimitError{
			ResetAt: resetAt,
			msg:     fmt.Sprintf("pressoir/github: primary rate limit exceeded; resets at %s", resetAt.Format(time.RFC3339)),
		}
	}
	// Secondary rate limit: Retry-After header.
	if s := resp.Header.Get("Retry-After"); s != "" {
		var resetAt time.Time
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
			resetAt = time.Now().Add(time.Duration(secs) * time.Second)
		} else {
			resetAt = time.Now().Add(60 * time.Second)
		}
		return &RateLimitError{
			ResetAt: resetAt,
			msg:     fmt.Sprintf("pressoir/github: secondary rate limit; retry after %s", s),
		}
	}
	return nil
}

func (a *GitHubAdapter) ghGet(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiURL(path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	a.cacheMu.Lock()
	entry, cached := a.etagCache[path]
	a.cacheMu.Unlock()
	if cached {
		req.Header.Set("If-None-Match", entry.etag)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		if rle := parseRateLimitError(resp); rle != nil {
			return rle
		}
	}
	if resp.StatusCode == http.StatusNotModified {
		// Cache hit: return the previously stored body without consuming quota.
		return json.Unmarshal(entry.body, result)
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pressoir/github: GET %s: %d %s", path, resp.StatusCode, string(b))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("pressoir/github: GET %s: read body: %w", path, err)
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		a.cacheMu.Lock()
		a.etagCache[path] = etagEntry{etag: etag, body: body}
		a.cacheMu.Unlock()
	}
	return json.Unmarshal(body, result)
}

func (a *GitHubAdapter) ghPost(ctx context.Context, path string, payload any, result any) error {
	return a.ghMutate(ctx, http.MethodPost, path, payload, result)
}

func (a *GitHubAdapter) ghPatch(ctx context.Context, path string, payload any, result any) error {
	return a.ghMutate(ctx, http.MethodPatch, path, payload, result)
}

func (a *GitHubAdapter) ghPut(ctx context.Context, path string, payload any, result any) error {
	return a.ghMutate(ctx, http.MethodPut, path, payload, result)
}

func (a *GitHubAdapter) ghDelete(ctx context.Context, path string) error {
	return a.ghMutate(ctx, http.MethodDelete, path, nil, nil)
}

func (a *GitHubAdapter) ghMutate(ctx context.Context, method, path string, payload any, result any) error {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("pressoir/github: marshal payload: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	resp, err := a.ghDo(ctx, method, path, bodyReader)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pressoir/github: %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	if result != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// ghGetAll paginates through all pages of a list endpoint, appending to result.
func (a *GitHubAdapter) ghGetAll(ctx context.Context, path string, result any) error {
	// result must be a pointer to a slice; we accumulate by calling ghGetPage.
	type page = []json.RawMessage
	var all page

	nextPath := path
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	nextPath = path + sep + "per_page=100"

	for nextPath != "" {
		var pg page
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.apiURL(nextPath), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := a.httpClient.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusForbidden {
			if rle := parseRateLimitError(resp); rle != nil {
				resp.Body.Close()
				return rle
			}
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return fmt.Errorf("pressoir/github: GET %s: %d %s", nextPath, resp.StatusCode, string(b))
		}
		if err := json.NewDecoder(resp.Body).Decode(&pg); err != nil {
			resp.Body.Close()
			return fmt.Errorf("pressoir/github: decode page: %w", err)
		}
		resp.Body.Close()
		all = append(all, pg...)
		nextPath = parseLinkNext(resp.Header.Get("Link"))
	}

	// Re-marshal the accumulated raw messages, then unmarshal into result.
	combined, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return json.Unmarshal(combined, result)
}

// parseLinkNext extracts the URL from a Link header's rel="next" entry.
// Returns "" when there is no next page. The returned URL is the full URL;
// callers must strip the scheme+host prefix before passing to ghDo.
func parseLinkNext(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		segments := strings.Split(part, ";")
		if len(segments) != 2 {
			continue
		}
		rel := strings.TrimSpace(segments[1])
		if rel != `rel="next"` {
			continue
		}
		u := strings.TrimSpace(segments[0])
		u = strings.TrimPrefix(u, "<")
		u = strings.TrimSuffix(u, ">")
		// Strip scheme+host, keep path+query
		parsed, err := url.Parse(u)
		if err != nil {
			return ""
		}
		result := parsed.Path
		if parsed.RawQuery != "" {
			result += "?" + parsed.RawQuery
		}
		// Strip leading slash since apiURL adds "https://host/"
		result = strings.TrimPrefix(result, "/")
		return result
	}
	return ""
}

// — Mapping helpers —

func ghPRToPR(pr ghPR) PullRequest {
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	labels := make([]string, len(pr.Labels))
	for i, l := range pr.Labels {
		labels[i] = l.Name
	}
	mergeSHA := ""
	if pr.MergeSHA != nil {
		mergeSHA = *pr.MergeSHA
	}
	return PullRequest{
		Number:       pr.Number,
		State:        state,
		Title:        pr.Title,
		Body:         pr.Body,
		Labels:       labels,
		SourceBranch: pr.Head.Ref,
		WebURL:       pr.HTMLURL,
		Author:       pr.User.Login,
		MergeSHA:     mergeSHA,
		HeadSHA:      pr.Head.SHA,
		Draft:        pr.Draft,
	}
}

func ghIssueToDomain(iss ghIssue) Issue {
	labels := make([]string, len(iss.Labels))
	for i, l := range iss.Labels {
		labels[i] = l.Name
	}
	return Issue{
		Number: iss.Number,
		State:  iss.State,
		Title:  iss.Title,
		Body:   iss.Body,
		Labels: labels,
		WebURL: iss.HTMLURL,
		Author: iss.User.Login,
	}
}

func mapCheckConclusion(status, conclusion string) string {
	// If not completed yet, map by status.
	if status != "completed" {
		switch status {
		case "in_progress":
			return "running"
		case "queued", "waiting", "requested", "pending":
			return "pending"
		default:
			return "pending"
		}
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return "success"
	case "failure", "timed_out", "cancelled", "action_required":
		return "failed"
	default:
		return "none"
	}
}

// repoPath returns "repos/owner/name" for use in API paths.
func ghRepoPath(repo RepoRef) string {
	return "repos/" + repo.Owner + "/" + repo.Name
}

// — Pull request operations —

func (a *GitHubAdapter) OpenPR(ctx context.Context, repo RepoRef, src, dst, title, body string, draft bool) (PullRequest, error) {
	payload := map[string]any{
		"title": title,
		"body":  body,
		"head":  src,
		"base":  dst,
		"draft": draft,
	}
	var pr ghPR
	if err := a.ghPost(ctx, ghRepoPath(repo)+"/pulls", payload, &pr); err != nil {
		return PullRequest{}, err
	}
	return ghPRToPR(pr), nil
}

func (a *GitHubAdapter) ListOpenPRs(ctx context.Context, repo RepoRef) ([]PullRequest, error) {
	var prs []ghPR
	if err := a.ghGetAll(ctx, ghRepoPath(repo)+"/pulls?state=open", &prs); err != nil {
		return nil, err
	}
	result := make([]PullRequest, len(prs))
	for i, pr := range prs {
		result[i] = ghPRToPR(pr)
	}
	return result, nil
}

func (a *GitHubAdapter) GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error) {
	var pr ghPR
	if err := a.ghGet(ctx, fmt.Sprintf("%s/pulls/%d", ghRepoPath(repo), number), &pr); err != nil {
		return PullRequest{}, err
	}
	return ghPRToPR(pr), nil
}

func (a *GitHubAdapter) UpdatePR(ctx context.Context, repo RepoRef, number int, params map[string]string) error {
	payload := make(map[string]any, len(params))
	for k, v := range params {
		payload[k] = v
	}
	return a.ghPatch(ctx, fmt.Sprintf("%s/pulls/%d", ghRepoPath(repo), number), payload, nil)
}

func (a *GitHubAdapter) Merge(ctx context.Context, repo RepoRef, number int, opts MergeOpts) error {
	payload := map[string]any{}
	if opts.MergeCommitMessage != "" {
		payload["commit_message"] = opts.MergeCommitMessage
	}
	return a.ghPut(ctx, fmt.Sprintf("%s/pulls/%d/merge", ghRepoPath(repo), number), payload, nil)
}

func (a *GitHubAdapter) Approve(ctx context.Context, repo RepoRef, number int) error {
	payload := map[string]any{
		"event": "APPROVE",
	}
	return a.ghPost(ctx, fmt.Sprintf("%s/pulls/%d/reviews", ghRepoPath(repo), number), payload, nil)
}

// GetApprovalStatus reads the approval state for a GitHub PR.
//
// It lists all reviews and collapses to the latest review per reviewer (the
// API returns them in chronological order). COMMENTED reviews are ignored for
// approval purposes. Approved = at least one APPROVED and no outstanding
// CHANGES_REQUESTED.
//
// It also tries to read the branch-protection required-reviewer count via
// GET /branches/{branch}/protection. A 403/404 (common with fine-grained PATs
// lacking admin scope) is silently treated as Required=0 — GitHub will enforce
// the rule at merge time.
func (a *GitHubAdapter) GetApprovalStatus(ctx context.Context, repo RepoRef, number int) (ApprovalStatus, error) {
	// Fetch the PR to get the target branch for protection lookup.
	var rawPR struct {
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := a.ghGet(ctx, fmt.Sprintf("%s/pulls/%d", ghRepoPath(repo), number), &rawPR); err != nil {
		return ApprovalStatus{}, fmt.Errorf("pressoir/github: get pr for approval: %w", err)
	}
	baseBranch := rawPR.Base.Ref

	// List all reviews (chronological order).
	var reviews []ghReview
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/pulls/%d/reviews", ghRepoPath(repo), number), &reviews); err != nil {
		return ApprovalStatus{}, fmt.Errorf("pressoir/github: list reviews: %w", err)
	}

	// Collapse to latest non-COMMENTED review per reviewer.
	// API order is chronological; iterating forward means last write wins.
	latest := map[string]string{} // login → state
	for _, r := range reviews {
		if r.User.Login == "" {
			continue
		}
		// COMMENTED does not change approval state — skip.
		if strings.ToUpper(r.State) == "COMMENTED" || strings.ToUpper(r.State) == "PENDING" {
			continue
		}
		latest[r.User.Login] = strings.ToUpper(r.State)
	}

	var approvedBy []string
	hasChangesRequested := false
	for login, state := range latest {
		switch state {
		case "APPROVED":
			approvedBy = append(approvedBy, login)
		case "CHANGES_REQUESTED":
			hasChangesRequested = true
		}
	}

	// Try to read required approving reviewer count from branch protection.
	// Degrade gracefully on 403/404 (fine-grained PAT without admin scope).
	required := 0
	if baseBranch != "" {
		var bp ghBranchProtection
		protPath := fmt.Sprintf("%s/branches/%s/protection", ghRepoPath(repo), url.PathEscape(baseBranch))
		if err := a.ghGet(ctx, protPath, &bp); err == nil {
			required = bp.RequiredPullRequestReviews.RequiredApprovingReviewCount
		}
		// On error (403, 404, etc.) keep required=0 and let GitHub enforce at merge.
	}

	// Approved when the number of approvals meets or exceeds the required count
	// AND no reviewer has an outstanding CHANGES_REQUESTED.
	// When required=0 (no protection rule or rule unreadable), any non-empty
	// approval set satisfies the threshold, but zero reviews also satisfies it
	// (0 >= 0) — meaning an unprotected repo with only CI gates auto-merges
	// without needing a human review, matching the intended degrade behaviour.
	approved := len(approvedBy) >= required && !hasChangesRequested

	return ApprovalStatus{
		Approved:         approved,
		ApprovedBy:       approvedBy,
		ChangesRequested: hasChangesRequested,
		Required:         required,
	}, nil
}

// — PR comments and reviews —

func (a *GitHubAdapter) Comment(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.postIssueComment(ctx, repo, number, body)
}

func (a *GitHubAdapter) PostPRNote(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.postIssueComment(ctx, repo, number, body)
}

func (a *GitHubAdapter) postIssueComment(ctx context.Context, repo RepoRef, number int, body string) error {
	payload := map[string]any{"body": body}
	return a.ghPost(ctx, fmt.Sprintf("%s/issues/%d/comments", ghRepoPath(repo), number), payload, nil)
}

func (a *GitHubAdapter) InlineComment(ctx context.Context, repo RepoRef, number int, c InlineComment, body string) error {
	// GitHub requires the PR head commit SHA for pull request review comments.
	// We use POST /pulls/{n}/comments (standalone review comment) rather than
	// POST /pulls/{n}/reviews (batch review) because a single neutral InlineComment
	// maps 1:1 to a standalone comment and avoids the review-level body duplication
	// that the batch endpoint would introduce.
	var rawPR ghPR
	if err := a.ghGet(ctx, fmt.Sprintf("%s/pulls/%d", ghRepoPath(repo), number), &rawPR); err != nil {
		return a.postIssueComment(ctx, repo, number, fmt.Sprintf("[%s:%d] %s", c.Path, c.Line, body))
	}

	side := "RIGHT"
	if strings.ToLower(c.Side) == "left" {
		side = "LEFT"
	}

	payload := map[string]any{
		"body":      body,
		"commit_id": rawPR.Head.SHA,
		"path":      c.Path,
		"line":      c.Line,
		"side":      side,
	}
	if err := a.ghPost(ctx, fmt.Sprintf("%s/pulls/%d/comments", ghRepoPath(repo), number), payload, nil); err != nil {
		return a.postIssueComment(ctx, repo, number, fmt.Sprintf("[%s:%d] %s", c.Path, c.Line, body))
	}
	return nil
}

func (a *GitHubAdapter) ListPRNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error) {
	// Top-level issue comments (non-inline).
	var issueComments []ghComment
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/issues/%d/comments", ghRepoPath(repo), number), &issueComments); err != nil {
		return nil, err
	}

	// Inline review comments.
	var reviewComments []ghReviewComment
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/pulls/%d/comments", ghRepoPath(repo), number), &reviewComments); err != nil {
		return nil, err
	}

	result := make([]Comment, 0, len(issueComments)+len(reviewComments))
	for _, c := range issueComments {
		result = append(result, Comment{
			ID:     c.ID,
			Body:   c.Body,
			Author: c.User.Login,
		})
	}
	for _, c := range reviewComments {
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		side := "right"
		if strings.EqualFold(c.Side, "LEFT") {
			side = "left"
		}
		result = append(result, Comment{
			ID:     c.ID,
			Body:   c.Body,
			Author: c.User.Login,
			InlineComment: &InlineComment{
				Path: c.Path,
				Line: line,
				Side: side,
			},
			DiscussionID: strconv.FormatInt(c.PullRequestReviewID, 10),
		})
	}
	return result, nil
}

func (a *GitHubAdapter) ListPRDiscussions(ctx context.Context, repo RepoRef, number int) ([]Discussion, error) {
	var reviewComments []ghReviewComment
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/pulls/%d/comments", ghRepoPath(repo), number), &reviewComments); err != nil {
		return nil, err
	}

	// Group by pull_request_review_id.
	order := []string{}
	byID := map[string][]Comment{}
	for _, c := range reviewComments {
		reviewID := strconv.FormatInt(c.PullRequestReviewID, 10)
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		side := "right"
		if strings.EqualFold(c.Side, "LEFT") {
			side = "left"
		}
		comment := Comment{
			ID:     c.ID,
			Body:   c.Body,
			Author: c.User.Login,
			InlineComment: &InlineComment{
				Path: c.Path,
				Line: line,
				Side: side,
			},
			DiscussionID: reviewID,
		}
		if _, seen := byID[reviewID]; !seen {
			order = append(order, reviewID)
		}
		byID[reviewID] = append(byID[reviewID], comment)
	}

	discs := make([]Discussion, 0, len(order))
	for _, id := range order {
		discs = append(discs, Discussion{ID: id, Comments: byID[id]})
	}
	return discs, nil
}

func (a *GitHubAdapter) GetPRChanges(ctx context.Context, repo RepoRef, number int) ([]string, error) {
	var files []ghPRFile
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/pulls/%d/files", ghRepoPath(repo), number), &files); err != nil {
		return nil, err
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.Filename
	}
	return names, nil
}

var closesPattern = regexp.MustCompile(`(?i)(?:closes|fixes|resolves)\s+#(\d+)`)

func (a *GitHubAdapter) GetPRClosingIssues(ctx context.Context, repo RepoRef, number int) ([]Issue, error) {
	pr, err := a.GetPR(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	matches := closesPattern.FindAllStringSubmatch(pr.Body, -1)
	var issues []Issue
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		iss, err := a.GetIssue(ctx, repo, n)
		if err != nil {
			continue
		}
		issues = append(issues, iss)
	}
	return issues, nil
}

// — GraphQL batching —

// ghGraphQL executes a GraphQL query against the GitHub GraphQL endpoint.
// variables is a map of query variables; result receives the decoded "data" object.
func (a *GitHubAdapter) ghGraphQL(ctx context.Context, query string, variables map[string]any, result any) error {
	payload := map[string]any{"query": query}
	if len(variables) > 0 {
		payload["variables"] = variables
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("pressoir/github: graphql marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.graphqlURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		if rle := parseRateLimitError(resp); rle != nil {
			return rle
		}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pressoir/github: graphql: %d %s", resp.StatusCode, string(body))
	}

	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors []map[string]any  `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("pressoir/github: graphql decode: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("pressoir/github: graphql errors: %v", envelope.Errors)
	}
	return json.Unmarshal(envelope.Data, result)
}

// ghBatchedCheckRunsQuery is the GraphQL query that fetches PR head SHA plus
// check suites (with check runs) in a single round-trip, avoiding the two
// REST calls (GET /pulls/:n + GET /commits/:sha/check-runs).
const ghBatchedCheckRunsQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      headRefOid
      commits(last: 1) {
        nodes {
          commit {
            checkSuites(first: 10) {
              nodes {
                checkRuns(first: 100) {
                  nodes {
                    name
                    status
                    conclusion
                    url
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}
`

type ghGQLCheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`     // QUEUED | IN_PROGRESS | COMPLETED
	Conclusion string `json:"conclusion"` // SUCCESS | FAILURE | NEUTRAL | CANCELLED | SKIPPED | TIMED_OUT | ACTION_REQUIRED
	URL        string `json:"url"`
}

type ghGQLBatchedData struct {
	Repository struct {
		PullRequest struct {
			HeadRefOid string `json:"headRefOid"`
			Commits    struct {
				Nodes []struct {
					Commit struct {
						CheckSuites struct {
							Nodes []struct {
								CheckRuns struct {
									Nodes []ghGQLCheckRun `json:"nodes"`
								} `json:"checkRuns"`
							} `json:"nodes"`
						} `json:"checkSuites"`
					} `json:"commit"`
				} `json:"nodes"`
			} `json:"commits"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

// mapGQLConclusion converts GraphQL enum values (upper-case) to the neutral
// CIStatus states understood by the rest of the system.
func mapGQLConclusion(status, conclusion string) string {
	// GraphQL uses upper-case enum values.
	switch strings.ToLower(status) {
	case "in_progress":
		return "running"
	case "queued", "waiting", "requested", "pending":
		return "pending"
	}
	// completed
	switch strings.ToLower(conclusion) {
	case "success", "neutral", "skipped":
		return "success"
	case "failure", "timed_out", "cancelled", "action_required":
		return "failed"
	}
	return "pending"
}

// batchedCIStatus fetches PR head SHA and check runs in one GraphQL round-trip.
// Falls back to the REST path if GraphQL is unavailable or returns no check runs.
func (a *GitHubAdapter) batchedCIStatus(ctx context.Context, repo RepoRef, number int) (CIStatus, error) {
	vars := map[string]any{
		"owner":  repo.Owner,
		"name":   repo.Name,
		"number": number,
	}
	var data ghGQLBatchedData
	if err := a.ghGraphQL(ctx, ghBatchedCheckRunsQuery, vars, &data); err != nil {
		// GraphQL failed — fall back to two REST calls.
		return a.restCIStatus(ctx, repo, number)
	}

	var runs []ghGQLCheckRun
	for _, commitNode := range data.Repository.PullRequest.Commits.Nodes {
		for _, suite := range commitNode.Commit.CheckSuites.Nodes {
			runs = append(runs, suite.CheckRuns.Nodes...)
		}
	}

	if len(runs) == 0 {
		// No check runs found via GraphQL; fall back to REST (which also tries workflow runs).
		return a.restCIStatus(ctx, repo, number)
	}

	webURL := ""
	hasRunning := false
	hasPending := false
	hasFailed := false
	for _, cr := range runs {
		if webURL == "" {
			webURL = cr.URL
		}
		switch mapGQLConclusion(cr.Status, cr.Conclusion) {
		case "failed":
			hasFailed = true
		case "running":
			hasRunning = true
		case "pending":
			hasPending = true
		}
	}

	var state string
	switch {
	case hasFailed:
		state = "failed"
	case hasRunning:
		state = "running"
	case hasPending:
		state = "pending"
	default:
		state = "success"
	}
	return CIStatus{State: state, WebURL: webURL}, nil
}

// restCIStatus is the original two-REST-call path used as fallback.
func (a *GitHubAdapter) restCIStatus(ctx context.Context, repo RepoRef, number int) (CIStatus, error) {
	var rawPR ghPR
	if err := a.ghGet(ctx, fmt.Sprintf("%s/pulls/%d", ghRepoPath(repo), number), &rawPR); err != nil {
		return CIStatus{State: "none"}, err
	}
	return a.checkRunsStatus(ctx, repo, rawPR.Head.SHA)
}

// — CI —

func (a *GitHubAdapter) CIStatusFor(ctx context.Context, repo RepoRef, number int) (CIStatus, error) {
	return a.batchedCIStatus(ctx, repo, number)
}

func (a *GitHubAdapter) checkRunsStatus(ctx context.Context, repo RepoRef, sha string) (CIStatus, error) {
	var checkResp ghCheckRunsResp
	if err := a.ghGet(ctx, fmt.Sprintf("%s/commits/%s/check-runs", ghRepoPath(repo), sha), &checkResp); err != nil {
		return CIStatus{State: "none"}, err
	}

	// Also fetch Actions workflow runs for this SHA — some required workflows are
	// not surfaced via the Checks API (e.g. workflows without a check_suite).
	var wfResp ghWorkflowRunsResp
	// Failure to fetch workflow runs is non-fatal: check runs may be sufficient.
	_ = a.ghGet(ctx, fmt.Sprintf("%s/actions/runs?head_sha=%s&per_page=100", ghRepoPath(repo), sha), &wfResp)

	if len(checkResp.CheckRuns) == 0 && len(wfResp.WorkflowRuns) == 0 {
		return CIStatus{State: "none"}, nil
	}

	// Aggregate both sources: any failure -> failed; any running -> running;
	// any pending -> pending; all success -> success.
	webURL := ""
	hasRunning := false
	hasPending := false
	hasFailed := false

	for _, cr := range checkResp.CheckRuns {
		state := mapCheckConclusion(cr.Status, cr.Conclusion)
		if webURL == "" {
			webURL = cr.HTMLURL
		}
		switch state {
		case "failed":
			hasFailed = true
		case "running":
			hasRunning = true
		case "pending":
			hasPending = true
		}
	}

	for _, wr := range wfResp.WorkflowRuns {
		state := mapCheckConclusion(wr.Status, wr.Conclusion)
		if webURL == "" {
			webURL = wr.HTMLURL
		}
		switch state {
		case "failed":
			hasFailed = true
		case "running":
			hasRunning = true
		case "pending":
			hasPending = true
		}
	}

	var state string
	switch {
	case hasFailed:
		state = "failed"
	case hasRunning:
		state = "running"
	case hasPending:
		state = "pending"
	default:
		state = "success"
	}
	return CIStatus{State: state, WebURL: webURL}, nil
}

func (a *GitHubAdapter) ListPipelinesByCommit(ctx context.Context, repo RepoRef, sha string) ([]CIStatus, error) {
	var resp ghCheckRunsResp
	if err := a.ghGet(ctx, fmt.Sprintf("%s/commits/%s/check-runs", ghRepoPath(repo), sha), &resp); err != nil {
		return nil, err
	}
	statuses := make([]CIStatus, len(resp.CheckRuns))
	for i, cr := range resp.CheckRuns {
		statuses[i] = CIStatus{
			State:  mapCheckConclusion(cr.Status, cr.Conclusion),
			WebURL: cr.HTMLURL,
		}
	}
	return statuses, nil
}

// — Issue operations —

func (a *GitHubAdapter) ListIssues(ctx context.Context, repo RepoRef, filter IssueFilter) ([]Issue, error) {
	q := url.Values{}
	if filter.Assignee != "" {
		q.Set("assignee", filter.Assignee)
	}
	if filter.Label != "" {
		q.Set("labels", filter.Label)
	}
	state := filter.State
	if state == "" {
		state = "open"
	}
	q.Set("state", state)

	path := ghRepoPath(repo) + "/issues?" + q.Encode()
	var issues []ghIssue
	if err := a.ghGetAll(ctx, path, &issues); err != nil {
		return nil, err
	}

	// GitHub /issues returns PRs too when they exist; filter them out (PRs have no body that matters here but the "pull_request" key won't be in our struct — use a raw approach).
	// Simple heuristic: ghIssue doesn't include pull_request key, so all decode fine.
	// GitHub includes PRs in /issues — filter by checking if issue number is a PR is expensive.
	// Per GitHub docs, filter with `?pulls=false` is not standard; instead we accept all and let callers handle.
	result := make([]Issue, len(issues))
	for i, iss := range issues {
		result[i] = ghIssueToDomain(iss)
	}
	return result, nil
}

func (a *GitHubAdapter) GetIssue(ctx context.Context, repo RepoRef, number int) (Issue, error) {
	var iss ghIssue
	if err := a.ghGet(ctx, fmt.Sprintf("%s/issues/%d", ghRepoPath(repo), number), &iss); err != nil {
		return Issue{}, err
	}
	return ghIssueToDomain(iss), nil
}

func (a *GitHubAdapter) CreateIssue(ctx context.Context, repo RepoRef, params map[string]string) (Issue, error) {
	payload := make(map[string]any, len(params))
	for k, v := range params {
		payload[k] = v
	}
	var iss ghIssue
	if err := a.ghPost(ctx, ghRepoPath(repo)+"/issues", payload, &iss); err != nil {
		return Issue{}, err
	}
	return ghIssueToDomain(iss), nil
}

func (a *GitHubAdapter) UpdateIssue(ctx context.Context, repo RepoRef, number int, params map[string]string) error {
	payload := make(map[string]any, len(params))
	for k, v := range params {
		payload[k] = v
	}
	return a.ghPatch(ctx, fmt.Sprintf("%s/issues/%d", ghRepoPath(repo), number), payload, nil)
}

func (a *GitHubAdapter) PostIssueNote(ctx context.Context, repo RepoRef, number int, body string) error {
	return a.postIssueComment(ctx, repo, number, body)
}

func (a *GitHubAdapter) SetLabels(ctx context.Context, repo RepoRef, number int, labels []string) error {
	payload := map[string]any{"labels": labels}
	return a.ghPut(ctx, fmt.Sprintf("%s/issues/%d/labels", ghRepoPath(repo), number), payload, nil)
}

// AddLabel adds a single label without disturbing existing ones (unlike
// SetLabels, which replaces the full set). GitHub auto-creates an unknown
// label on first use.
func (a *GitHubAdapter) AddLabel(ctx context.Context, repo RepoRef, number int, label string) error {
	payload := map[string]any{"labels": []string{label}}
	return a.ghPost(ctx, fmt.Sprintf("%s/issues/%d/labels", ghRepoPath(repo), number), payload, nil)
}

// RemoveLabel removes a single label. A 404 (label not present on the
// issue/PR) is treated as a successful no-op, since callers invoke this
// idempotently regardless of whether the label was ever applied.
func (a *GitHubAdapter) RemoveLabel(ctx context.Context, repo RepoRef, number int, label string) error {
	err := a.ghDelete(ctx, fmt.Sprintf("%s/issues/%d/labels/%s", ghRepoPath(repo), number, url.PathEscape(label)))
	if err != nil && strings.Contains(err.Error(), ": 404 ") {
		return nil
	}
	return err
}

// ListIssueNotes returns top-level comments on a GitHub issue.
// GET /repos/{owner}/{repo}/issues/{number}/comments
func (a *GitHubAdapter) ListIssueNotes(ctx context.Context, repo RepoRef, number int) ([]Comment, error) {
	var comments []ghComment
	if err := a.ghGetAll(ctx, fmt.Sprintf("%s/issues/%d/comments", ghRepoPath(repo), number), &comments); err != nil {
		return nil, err
	}
	result := make([]Comment, len(comments))
	for i, c := range comments {
		result[i] = Comment{
			ID:     c.ID,
			Body:   c.Body,
			Author: c.User.Login,
		}
	}
	return result, nil
}

// GetDefaultBranch returns the repository's default branch name.
// GET /repos/{owner}/{repo}
func (a *GitHubAdapter) GetDefaultBranch(ctx context.Context, repo RepoRef) (string, error) {
	var result struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := a.ghGet(ctx, ghRepoPath(repo), &result); err != nil {
		return "", err
	}
	if result.DefaultBranch == "" {
		return "main", nil
	}
	return result.DefaultBranch, nil
}

// AddSubIssue attaches childNumber as a native sub-issue of parentNumber on GitHub.
// It resolves the child's global issue id first (the sub-issues API requires the id,
// not the number), then POSTs to the sub_issues endpoint. On any failure (404/403/422
// — feature not enabled, insufficient scope, or endpoint unavailable) it degrades
// gracefully to a task-list entry in the parent body. Never returns an error for the
// "sub-issues unavailable" case.
func (a *GitHubAdapter) AddSubIssue(ctx context.Context, repo RepoRef, parentNumber, childNumber int) error {
	// Step 1: resolve the child issue's global id.
	var childIssue ghIssue
	if err := a.ghGet(ctx, fmt.Sprintf("%s/issues/%d", ghRepoPath(repo), childNumber), &childIssue); err != nil {
		// Cannot resolve child — fall back to task-list.
		return a.addSubIssueTaskList(ctx, repo, parentNumber, childNumber)
	}

	// Step 2: attempt native sub-issues API.
	payload := map[string]any{"sub_issue_id": childIssue.ID}
	if err := a.ghPost(ctx, fmt.Sprintf("%s/issues/%d/sub_issues", ghRepoPath(repo), parentNumber), payload, nil); err != nil {
		// Native API unavailable or disabled — fall back to task-list.
		return a.addSubIssueTaskList(ctx, repo, parentNumber, childNumber)
	}
	return nil
}

// addSubIssueTaskList appends a `- [ ] #childNumber` task-list entry to the
// parent issue body (idempotent — does not duplicate if already present).
// Idempotency is checked line-by-line to avoid false positives when a child
// number is a numeric prefix of an existing entry (e.g. #5 vs #50).
func (a *GitHubAdapter) addSubIssueTaskList(ctx context.Context, repo RepoRef, parentNumber, childNumber int) error {
	var parentIssue ghIssue
	if err := a.ghGet(ctx, fmt.Sprintf("%s/issues/%d", ghRepoPath(repo), parentNumber), &parentIssue); err != nil {
		// Cannot read parent — degrade silently.
		return nil
	}
	taskEntry := fmt.Sprintf("- [ ] #%d", childNumber)
	// Check line-by-line (exact match) so that #5 is not confused with #50.
	for _, ln := range strings.Split(parentIssue.Body, "\n") {
		s := strings.TrimSpace(ln)
		if s == taskEntry || s == fmt.Sprintf("- [x] #%d", childNumber) {
			// Already present (open or completed) — idempotent.
			return nil
		}
	}
	newBody := parentIssue.Body
	if newBody != "" {
		newBody += "\n"
	}
	newBody += taskEntry
	// Use UpdateIssue to PATCH the body.
	_ = a.UpdateIssue(ctx, repo, parentNumber, map[string]string{"body": newBody})
	return nil
}

// LinkIssues posts a cross-reference comment on the source issue pointing to the target.
// GitHub has no native issue-link API (unlike GitLab's blocks/relates_to primitives).
// A best-effort approach is used: post a comment noting the relationship.
func (a *GitHubAdapter) LinkIssues(ctx context.Context, repo RepoRef, number int, targetRepo RepoRef, targetNumber int, linkType string) error {
	// Build a cross-reference comment body.
	var verb string
	switch linkType {
	case "is_blocked_by":
		verb = "is blocked by"
	case "relates_to":
		verb = "relates to"
	default: // "blocks" and unknown types
		verb = "blocks"
	}
	var targetRef string
	if repoPath := ghRepoPath(targetRepo); repoPath == ghRepoPath(repo) {
		targetRef = fmt.Sprintf("#%d", targetNumber)
	} else {
		targetRef = fmt.Sprintf("%s/%s#%d", targetRepo.Owner, targetRepo.Name, targetNumber)
	}
	body := fmt.Sprintf("This issue %s %s.", verb, targetRef)
	payload := map[string]any{"body": body}
	return a.ghPost(ctx, fmt.Sprintf("%s/issues/%d/comments", ghRepoPath(repo), number), payload, nil)
}

// — User —

func (a *GitHubAdapter) ResolveUser(ctx context.Context, username string) (UserID, error) {
	var u ghUser
	if err := a.ghGet(ctx, "users/"+username, &u); err != nil {
		return UserID{}, err
	}
	return UserID{Username: u.Login}, nil
}

// — Capabilities —

func (a *GitHubAdapter) Capabilities(_ context.Context) Capabilities {
	return Capabilities{HasEpics: false}
}

// — Worker guidance —

func (a *GitHubAdapter) WorkerGuidance(repo, host, encodedRepo, targetBranch, user, project, name string) string {
	return fmt.Sprintf(`- To open a pull request use: aoc pressoir open-pr --repo %s --src $(git branch --show-current) --dst %s --title "your title" --body "your description"
- MR titles must start with fix:/feat: (scope optional): "fix(ci): …", "feat: …". Other prefixes (docs:, chore:, ops:) are rejected.
- When you open a PR: (1) call track_mr with the PR number so review comments reach you, (2) run: aoc notify "[%s] Opened PR #<number> on %s: https://%s/%s/pull/<number>"
- When you finish a task or address review feedback, run: aoc notify "[%s] <summary of what you did>"
- To comment on a pull request: aoc pressoir comment-pr --repo %s --number <number> --body "<comment>"
- Comment on the PR when you start working, after each significant change, and when you finish.`,
		repo, targetBranch,
		name, project, host, repo,
		name,
		repo)
}
