package pressoir

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestGHAdapter creates a GitHubAdapter pointed at an httptest TLS server.
// The adapter's HTTP client uses the test server's TLS transport.
func newTestGHAdapter(srv *httptest.Server) *GitHubAdapter {
	// Strip the scheme — apiURL adds "https://".
	restHost := strings.TrimPrefix(srv.URL, "https://")
	return &GitHubAdapter{
		restHost:   restHost,
		graphqlURL: srv.URL + "/graphql",
		token:      "test-token",
		httpClient: srv.Client(),
		etagCache:  make(map[string]etagEntry),
	}
}

func serveGHJSON(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("serveGHJSON: encode: %v", err)
		}
	}
}

// TestGitHub_GetPR_NumberMapping verifies that GitHub PR.number maps to PullRequest.Number.
func TestGitHub_GetPR_NumberMapping(t *testing.T) {
	pr := ghPR{
		Number:  42,
		State:   "open",
		Title:   "My PR",
		Body:    "body text",
		Draft:   false,
		Merged:  false,
		HTMLURL: "https://github.com/owner/repo/pull/42",
		User:    ghUser{Login: "alice"},
		Labels:  []ghLabel{{Name: "bug"}},
		Head:    ghHeadRef{SHA: "abc123", Ref: "feature/x"},
	}

	srv := httptest.NewTLSServer(serveGHJSON(t, pr))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	got, err := adapter.GetPR(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}

	if got.Number != 42 {
		t.Errorf("Number: got %d, want 42", got.Number)
	}
	if got.State != "open" {
		t.Errorf("State: got %q, want %q", got.State, "open")
	}
	if got.Author != "alice" {
		t.Errorf("Author: got %q, want %q", got.Author, "alice")
	}
	if got.SourceBranch != "feature/x" {
		t.Errorf("SourceBranch: got %q, want %q", got.SourceBranch, "feature/x")
	}
	if len(got.Labels) != 1 || got.Labels[0] != "bug" {
		t.Errorf("Labels: got %v, want [bug]", got.Labels)
	}
}

// TestGitHub_GetPR_MergedState verifies that merged=true maps State to "merged".
func TestGitHub_GetPR_MergedState(t *testing.T) {
	sha := "deadbeef"
	pr := ghPR{
		Number:   7,
		State:    "closed",
		Merged:   true,
		MergeSHA: &sha,
		User:     ghUser{Login: "bob"},
		Head:     ghHeadRef{SHA: "abc"},
	}

	srv := httptest.NewTLSServer(serveGHJSON(t, pr))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	got, err := adapter.GetPR(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 7)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}
	if got.State != "merged" {
		t.Errorf("State: got %q, want %q", got.State, "merged")
	}
	if got.MergeSHA != "deadbeef" {
		t.Errorf("MergeSHA: got %q, want %q", got.MergeSHA, "deadbeef")
	}
}

// TestGitHub_ListPRNotes_InlineMapping verifies top-level vs inline comment distinction.
func TestGitHub_ListPRNotes_InlineMapping(t *testing.T) {
	issueComments := []ghComment{
		{ID: 1, Body: "top-level comment", User: ghUser{Login: "bob"}},
	}
	reviewComments := []ghReviewComment{
		{
			ID:                  2,
			Body:                "inline comment",
			User:                ghUser{Login: "carol"},
			Path:                "internal/foo/bar.go",
			Line:                17,
			Side:                "RIGHT",
			PullRequestReviewID: 999,
		},
	}

	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issueComments)
	})
	mux.HandleFunc("/repos/owner/repo/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reviewComments)
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	comments, err := adapter.ListPRNotes(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1)
	if err != nil {
		t.Fatalf("ListPRNotes: %v", err)
	}

	if len(comments) != 2 {
		t.Fatalf("len(comments): got %d, want 2", len(comments))
	}

	// First: top-level issue comment — no inline.
	if comments[0].InlineComment != nil {
		t.Errorf("comments[0].InlineComment: expected nil for top-level comment")
	}
	if comments[0].Author != "bob" {
		t.Errorf("comments[0].Author: got %q, want %q", comments[0].Author, "bob")
	}

	// Second: inline review comment.
	if comments[1].InlineComment == nil {
		t.Fatalf("comments[1].InlineComment: expected non-nil for inline comment")
	}
	if comments[1].InlineComment.Path != "internal/foo/bar.go" {
		t.Errorf("InlineComment.Path: got %q, want %q", comments[1].InlineComment.Path, "internal/foo/bar.go")
	}
	if comments[1].InlineComment.Line != 17 {
		t.Errorf("InlineComment.Line: got %d, want 17", comments[1].InlineComment.Line)
	}
	if comments[1].InlineComment.Side != "right" {
		t.Errorf("InlineComment.Side: got %q, want %q", comments[1].InlineComment.Side, "right")
	}
	if comments[1].DiscussionID != "999" {
		t.Errorf("DiscussionID: got %q, want %q", comments[1].DiscussionID, "999")
	}
}

// TestGitHub_CIStatusFor_CheckRunMapping verifies check_run conclusions map to CIStatus.State.
func TestGitHub_CIStatusFor_CheckRunMapping(t *testing.T) {
	cases := []struct {
		status     string
		conclusion string
		wantState  string
	}{
		{"completed", "success", "success"},
		{"completed", "neutral", "success"},
		{"completed", "skipped", "success"},
		{"completed", "failure", "failed"},
		{"completed", "timed_out", "failed"},
		{"completed", "cancelled", "failed"},
		{"completed", "action_required", "failed"},
		{"in_progress", "", "running"},
		{"queued", "", "pending"},
		{"waiting", "", "pending"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.status+"/"+tc.conclusion, func(t *testing.T) {
			pr := ghPR{Number: 1, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "sha1"}}
			checkResp := ghCheckRunsResp{
				TotalCount: 1,
				CheckRuns: []ghCheckRun{
					{Status: tc.status, Conclusion: tc.conclusion, HTMLURL: "https://github.com/checks/1"},
				},
			}

			callNum := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
				callNum++
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(pr)
			})
			mux.HandleFunc("/repos/owner/repo/commits/sha1/check-runs", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(checkResp)
			})

			srv := httptest.NewTLSServer(mux)
			defer srv.Close()

			adapter := newTestGHAdapter(srv)
			status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1)
			if err != nil {
				t.Fatalf("CIStatusFor: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State: got %q, want %q (status=%q conclusion=%q)", status.State, tc.wantState, tc.status, tc.conclusion)
			}
		})
	}
}

// TestGitHub_CIStatusFor_NoCheckRuns verifies that an empty check-runs list returns "none".
func TestGitHub_CIStatusFor_NoCheckRuns(t *testing.T) {
	pr := ghPR{Number: 1, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "sha1"}}
	checkResp := ghCheckRunsResp{TotalCount: 0, CheckRuns: []ghCheckRun{}}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/pulls/1", serveGHJSON(t, pr))
	mux.HandleFunc("/repos/owner/repo/commits/sha1/check-runs", serveGHJSON(t, checkResp))

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1)
	if err != nil {
		t.Fatalf("CIStatusFor: %v", err)
	}
	if status.State != "none" {
		t.Errorf("State: got %q, want %q", status.State, "none")
	}
}

// TestGitHub_Capabilities verifies that the GitHub adapter reports HasEpics=false.
func TestGitHub_Capabilities(t *testing.T) {
	adapter := &GitHubAdapter{}
	caps := adapter.Capabilities(context.Background())
	if caps.HasEpics {
		t.Error("Capabilities.HasEpics: got true, want false for GitHub adapter")
	}
}

// TestGitHub_GetIssue_NumberMapping verifies that GitHub issue.number maps to Issue.Number.
func TestGitHub_GetIssue_NumberMapping(t *testing.T) {
	iss := ghIssue{
		Number:  99,
		State:   "open",
		Title:   "A bug",
		Body:    "details",
		HTMLURL: "https://github.com/owner/repo/issues/99",
		User:    ghUser{Login: "dave"},
		Labels:  []ghLabel{{Name: "bug"}},
	}

	srv := httptest.NewTLSServer(serveGHJSON(t, iss))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	issue, err := adapter.GetIssue(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 99)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Number != 99 {
		t.Errorf("Number: got %d, want 99", issue.Number)
	}
	if issue.Author != "dave" {
		t.Errorf("Author: got %q, want %q", issue.Author, "dave")
	}
	if len(issue.Labels) != 1 || issue.Labels[0] != "bug" {
		t.Errorf("Labels: got %v, want [bug]", issue.Labels)
	}
}

// TestGitHub_OpenPR_DraftFlag verifies that draft=true is sent in the request body.
func TestGitHub_OpenPR_DraftFlag(t *testing.T) {
	var receivedBody map[string]any
	responsePR := ghPR{
		Number: 5,
		State:  "open",
		Title:  "Draft PR",
		Draft:  true,
		User:   ghUser{Login: "eve"},
		Head:   ghHeadRef{SHA: "abc", Ref: "feature/draft"},
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&receivedBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(responsePR)
	}))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	pr, err := adapter.OpenPR(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, "feature/draft", "main", "Draft PR", "body", true)
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}

	if !pr.Draft {
		t.Errorf("Draft: got false, want true")
	}
	if receivedBody["draft"] != true {
		t.Errorf("request body draft: got %v, want true", receivedBody["draft"])
	}
	if receivedBody["head"] != "feature/draft" {
		t.Errorf("request body head: got %v, want feature/draft", receivedBody["head"])
	}
	if receivedBody["base"] != "main" {
		t.Errorf("request body base: got %v, want main", receivedBody["base"])
	}
}

// TestMapCheckConclusion_AllCases tests the mapCheckConclusion helper directly.
func TestMapCheckConclusion_AllCases(t *testing.T) {
	cases := []struct {
		status     string
		conclusion string
		want       string
	}{
		{"completed", "success", "success"},
		{"completed", "neutral", "success"},
		{"completed", "skipped", "success"},
		{"completed", "failure", "failed"},
		{"completed", "timed_out", "failed"},
		{"completed", "cancelled", "failed"},
		{"completed", "action_required", "failed"},
		{"completed", "stale", "none"},
		{"in_progress", "", "running"},
		{"queued", "", "pending"},
		{"waiting", "", "pending"},
		{"requested", "", "pending"},
		{"pending", "", "pending"},
		{"unknown_status", "", "pending"},
	}
	for _, tc := range cases {
		got := mapCheckConclusion(tc.status, tc.conclusion)
		if got != tc.want {
			t.Errorf("mapCheckConclusion(%q, %q) = %q, want %q", tc.status, tc.conclusion, got, tc.want)
		}
	}
}

// TestGhPRToPR_AllFields verifies the full field mapping from ghPR.
func TestGhPRToPR_AllFields(t *testing.T) {
	sha := "cafebabe"
	pr := ghPR{
		Number:   3,
		State:    "open",
		Title:    "Fix thing",
		Body:     "desc",
		Draft:    true,
		Merged:   false,
		HTMLURL:  "https://github.com/owner/repo/pull/3",
		User:     ghUser{Login: "frank"},
		Labels:   []ghLabel{{Name: "enhancement"}, {Name: "ready"}},
		Head:     ghHeadRef{SHA: "head123", Ref: "fix/thing"},
		MergeSHA: &sha,
	}
	got := ghPRToPR(pr)
	if got.Number != 3 {
		t.Errorf("Number: got %d", got.Number)
	}
	if got.State != "open" {
		t.Errorf("State: got %q", got.State)
	}
	if got.Body != "desc" {
		t.Errorf("Body: got %q", got.Body)
	}
	if !got.Draft {
		t.Error("Draft: got false, want true")
	}
	if len(got.Labels) != 2 || got.Labels[0] != "enhancement" {
		t.Errorf("Labels: got %v", got.Labels)
	}
	if got.SourceBranch != "fix/thing" {
		t.Errorf("SourceBranch: got %q", got.SourceBranch)
	}
	if got.MergeSHA != "cafebabe" {
		t.Errorf("MergeSHA: got %q", got.MergeSHA)
	}
	if got.WebURL != "https://github.com/owner/repo/pull/3" {
		t.Errorf("WebURL: got %q", got.WebURL)
	}
}

// serveEmptyGraphQL returns a GraphQL handler that returns empty check-suite
// data, causing batchedCIStatus to fall through to the REST fallback path.
func serveEmptyGraphQL(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Empty PR/commit data — triggers REST fallback in batchedCIStatus.
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"headRefOid":"","commits":{"nodes":[]}}}}}`)) //nolint:errcheck
	}
}

// TestGitHub_CIStatusFor_WorkflowRunAggregation verifies that Actions workflow runs
// are included in the CIStatus aggregation alongside check runs (via REST fallback).
func TestGitHub_CIStatusFor_WorkflowRunAggregation(t *testing.T) {
	cases := []struct {
		name      string
		checkRuns []ghCheckRun
		workflows []ghWorkflowRun
		wantState string
	}{
		{
			name:      "both empty returns none",
			wantState: "none",
		},
		{
			name:      "check-run success + workflow running returns running",
			checkRuns: []ghCheckRun{{Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/checks/1"}},
			workflows: []ghWorkflowRun{{Status: "in_progress", Conclusion: "", HTMLURL: "https://github.com/runs/1"}},
			wantState: "running",
		},
		{
			name:      "check-run success + workflow failed returns failed",
			checkRuns: []ghCheckRun{{Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/checks/1"}},
			workflows: []ghWorkflowRun{{Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/runs/1"}},
			wantState: "failed",
		},
		{
			name:      "all success returns success",
			checkRuns: []ghCheckRun{{Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/checks/1"}},
			workflows: []ghWorkflowRun{{Status: "completed", Conclusion: "success", HTMLURL: "https://github.com/runs/1"}},
			wantState: "success",
		},
		{
			name:      "check-run running + workflow failed returns failed",
			checkRuns: []ghCheckRun{{Status: "in_progress", Conclusion: "", HTMLURL: "https://github.com/checks/1"}},
			workflows: []ghWorkflowRun{{Status: "completed", Conclusion: "failure", HTMLURL: "https://github.com/runs/1"}},
			wantState: "failed",
		},
		{
			name:      "only workflow run pending returns pending",
			workflows: []ghWorkflowRun{{Status: "queued", Conclusion: "", HTMLURL: "https://github.com/runs/1"}},
			wantState: "pending",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pr := ghPR{Number: 1, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "sha42"}}
			checkResp := ghCheckRunsResp{TotalCount: len(tc.checkRuns), CheckRuns: tc.checkRuns}
			wfResp := ghWorkflowRunsResp{TotalCount: len(tc.workflows), WorkflowRuns: tc.workflows}

			mux := http.NewServeMux()
			// GraphQL returns empty data — triggers REST fallback.
			mux.HandleFunc("/graphql", serveEmptyGraphQL(t))
			mux.HandleFunc("/repos/owner/repo/pulls/1", serveGHJSON(t, pr))
			mux.HandleFunc("/repos/owner/repo/commits/sha42/check-runs", serveGHJSON(t, checkResp))
			mux.HandleFunc("/repos/owner/repo/actions/runs", serveGHJSON(t, wfResp))

			srv := httptest.NewTLSServer(mux)
			defer srv.Close()

			adapter := newTestGHAdapter(srv)
			status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1)
			if err != nil {
				t.Fatalf("CIStatusFor: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State: got %q, want %q", status.State, tc.wantState)
			}
		})
	}
}

// TestGitHub_ETag_ConditionalGet verifies that ghGet sends If-None-Match on the
// second call and treats a 304 response as a cache hit (no new quota consumed,
// the original body is returned without error).
func TestGitHub_ETag_ConditionalGet(t *testing.T) {
	issue := ghIssue{
		Number: 7, State: "open", Title: "Cached issue",
		User: ghUser{Login: "alice"}, HTMLURL: "https://github.com/o/r/issues/7",
	}

	callCount := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Header.Get("If-None-Match") == `"etag-v1"` {
			// Second call: honour the conditional request.
			w.WriteHeader(http.StatusNotModified)
			return
		}
		// First call: respond with body + ETag.
		w.Header().Set("ETag", `"etag-v1"`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issue)
	}))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	repo := RepoRef{Owner: "o", Name: "r"}

	// First call: populates ETag cache.
	got1, err := adapter.GetIssue(context.Background(), repo, 7)
	if err != nil {
		t.Fatalf("GetIssue (first): %v", err)
	}
	if got1.Number != 7 {
		t.Errorf("first call: Number: got %d, want 7", got1.Number)
	}
	if callCount != 1 {
		t.Errorf("expected 1 server call after first GetIssue, got %d", callCount)
	}

	// Second call: should send If-None-Match and receive 304.
	got2, err := adapter.GetIssue(context.Background(), repo, 7)
	if err != nil {
		t.Fatalf("GetIssue (second / 304): %v", err)
	}
	if got2.Number != 7 {
		t.Errorf("second call (304): Number: got %d, want 7", got2.Number)
	}
	if got2.Title != "Cached issue" {
		t.Errorf("second call (304): Title: got %q, want %q", got2.Title, "Cached issue")
	}
	// The handler must have been called again (to return 304), but the body
	// must come from cache — handler was called exactly twice total.
	if callCount != 2 {
		t.Errorf("expected 2 server calls total (first=200, second=304), got %d", callCount)
	}
}

// TestGitHub_RateLimit_Primary verifies that a 403 with X-RateLimit-Remaining: 0
// returns a *RateLimitError with a ResetAt time derived from X-RateLimit-Reset.
func TestGitHub_RateLimit_Primary(t *testing.T) {
	resetUnix := time.Now().Add(5 * time.Minute).Unix()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetUnix, 10))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	_, err := adapter.GetIssue(context.Background(), RepoRef{Owner: "o", Name: "r"}, 1)
	if err == nil {
		t.Fatal("GetIssue: expected error, got nil")
	}
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("GetIssue: expected *RateLimitError, got %T: %v", err, err)
	}
	wantReset := time.Unix(resetUnix, 0)
	if rle.ResetAt.Unix() != wantReset.Unix() {
		t.Errorf("RateLimitError.ResetAt: got %v, want %v", rle.ResetAt, wantReset)
	}
	if rle.Error() == "" {
		t.Error("RateLimitError.Error(): got empty string")
	}
}

// TestGitHub_RateLimit_Secondary verifies that a 403 with a Retry-After header
// (secondary rate limit) returns a *RateLimitError with an appropriate ResetAt.
func TestGitHub_RateLimit_Secondary(t *testing.T) {
	before := time.Now()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	_, err := adapter.GetIssue(context.Background(), RepoRef{Owner: "o", Name: "r"}, 1)
	if err == nil {
		t.Fatal("GetIssue: expected error, got nil")
	}
	rle, ok := err.(*RateLimitError)
	if !ok {
		t.Fatalf("GetIssue: expected *RateLimitError, got %T: %v", err, err)
	}
	after := time.Now().Add(30 * time.Second)
	// ResetAt should be approximately now+30s.
	if rle.ResetAt.Before(before.Add(29*time.Second)) || rle.ResetAt.After(after.Add(time.Second)) {
		t.Errorf("RateLimitError.ResetAt out of expected range: %v", rle.ResetAt)
	}
	if rle.Error() == "" {
		t.Error("RateLimitError.Error(): got empty string")
	}
}

// TestGitHub_RateLimit_403_NotRateLimit verifies that a plain 403 (no rate-limit
// headers) is not misclassified as a *RateLimitError.
func TestGitHub_RateLimit_403_NotRateLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	_, err := adapter.GetIssue(context.Background(), RepoRef{Owner: "o", Name: "r"}, 1)
	if err == nil {
		t.Fatal("GetIssue: expected error, got nil")
	}
	if _, ok := err.(*RateLimitError); ok {
		t.Fatalf("GetIssue: plain 403 should not be *RateLimitError, got one: %v", err)
	}
}

// TestGitHub_BatchedCIStatus_GraphQL verifies that batchedCIStatus correctly
// aggregates check runs returned by the GraphQL endpoint (no REST fallback).
func TestGitHub_BatchedCIStatus_GraphQL(t *testing.T) {
	cases := []struct {
		name      string
		status    string
		conclusion string
		wantState string
	}{
		{"completed success", "COMPLETED", "SUCCESS", "success"},
		{"in_progress", "IN_PROGRESS", "", "running"},
		{"completed failure", "COMPLETED", "FAILURE", "failed"},
		{"queued", "QUEUED", "", "pending"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gqlResp := fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"headRefOid":"sha99","commits":{"nodes":[{"commit":{"checkSuites":{"nodes":[{"checkRuns":{"nodes":[{"name":"ci","status":%q,"conclusion":%q,"url":"https://github.com/checks/1"}]}}]}}}]}}}}}`,
				tc.status, tc.conclusion)

			mux := http.NewServeMux()
			mux.HandleFunc("/graphql", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(gqlResp))
			})
			srv := httptest.NewTLSServer(mux)
			defer srv.Close()

			adapter := newTestGHAdapter(srv)
			status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1)
			if err != nil {
				t.Fatalf("CIStatusFor: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State: got %q, want %q", status.State, tc.wantState)
			}
		})
	}
}

// TestParseLinkNext verifies the Link header parser.
func TestParseLinkNext(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{
			`<https://api.github.com/repos/owner/repo/issues?page=2&per_page=100>; rel="next", <https://api.github.com/repos/owner/repo/issues?page=5>; rel="last"`,
			"repos/owner/repo/issues?page=2&per_page=100",
		},
		{`<https://api.github.com/repos/owner/repo/pulls?page=3>; rel="prev"`, ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := parseLinkNext(tc.header)
		if got != tc.want {
			t.Errorf("parseLinkNext(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// TestNormalizeGitHubHost verifies that user-supplied host strings are correctly
// mapped to REST API host and GraphQL endpoint URL.
func TestNormalizeGitHubHost(t *testing.T) {
	cases := []struct {
		input      string
		wantRest   string
		wantGQL    string
	}{
		{"", "api.github.com", "https://api.github.com/graphql"},
		{"github.com", "api.github.com", "https://api.github.com/graphql"},
		{"ghe.example.com", "ghe.example.com/api/v3", "https://ghe.example.com/api/graphql"},
		{"internal.corp", "internal.corp/api/v3", "https://internal.corp/api/graphql"},
	}
	for _, tc := range cases {
		gotRest, gotGQL := normalizeGitHubHost(tc.input)
		if gotRest != tc.wantRest {
			t.Errorf("normalizeGitHubHost(%q) restHost = %q, want %q", tc.input, gotRest, tc.wantRest)
		}
		if gotGQL != tc.wantGQL {
			t.Errorf("normalizeGitHubHost(%q) graphqlURL = %q, want %q", tc.input, gotGQL, tc.wantGQL)
		}
	}
}

// TestNewGitHubAdapter_HostNormalization verifies that NewGitHubAdapter correctly
// normalises the user-facing host to the API endpoints.
func TestNewGitHubAdapter_HostNormalization(t *testing.T) {
	cases := []struct {
		inputHost  string
		wantRest   string
		wantGQL    string
	}{
		{"", "api.github.com", "https://api.github.com/graphql"},
		{"github.com", "api.github.com", "https://api.github.com/graphql"},
		{"ghe.corp.com", "ghe.corp.com/api/v3", "https://ghe.corp.com/api/graphql"},
	}
	for _, tc := range cases {
		a := NewGitHubAdapter(tc.inputHost, "tok")
		if a.restHost != tc.wantRest {
			t.Errorf("NewGitHubAdapter(%q).restHost = %q, want %q", tc.inputHost, a.restHost, tc.wantRest)
		}
		if a.graphqlURL != tc.wantGQL {
			t.Errorf("NewGitHubAdapter(%q).graphqlURL = %q, want %q", tc.inputHost, a.graphqlURL, tc.wantGQL)
		}
	}
}

// ── GetApprovalStatus tests ────────────────────────────────────────────────────

// makeApprovalSrv returns an httptest.Server that serves:
//   - GET /repos/o/r/pulls/1 → {"base":{"ref":<branch>}}
//   - GET /repos/o/r/pulls/1/reviews → reviews JSON
//   - GET /repos/o/r/branches/<branch>/protection → protection JSON or status code
func makeApprovalSrv(t *testing.T, branch string, reviews []ghReview, protectionCode int, protection *ghBranchProtection) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		type baseRef struct {
			Ref string `json:"ref"`
		}
		type prResp struct {
			Base baseRef `json:"base"`
		}
		json.NewEncoder(w).Encode(prResp{Base: baseRef{Ref: branch}})
	})

	mux.HandleFunc("/repos/o/r/pulls/1/reviews", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(reviews)
	})

	mux.HandleFunc("/repos/o/r/branches/"+branch+"/protection", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(protectionCode)
		if protection != nil && protectionCode == http.StatusOK {
			json.NewEncoder(w).Encode(protection)
		}
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func makeApprovalAdapter(t *testing.T, srv *httptest.Server) *GitHubAdapter {
	t.Helper()
	return newTestGHAdapter(srv)
}

func TestGitHub_ApprovalStatus_ApprovesWhenLatestIsApprove(t *testing.T) {
	// alice: CHANGES_REQUESTED then APPROVED (latest wins) → approved
	// bob:   APPROVED → approved
	reviews := []ghReview{
		{ID: 1, User: ghUser{Login: "alice"}, State: "CHANGES_REQUESTED"},
		{ID: 2, User: ghUser{Login: "bob"}, State: "APPROVED"},
		{ID: 3, User: ghUser{Login: "alice"}, State: "APPROVED"},
	}
	prot := &ghBranchProtection{}
	prot.RequiredPullRequestReviews.RequiredApprovingReviewCount = 2

	srv := makeApprovalSrv(t, "main", reviews, http.StatusOK, prot)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if !status.Approved {
		t.Error("expected Approved=true (both reviewers have latest APPROVED)")
	}
	if status.ChangesRequested {
		t.Error("expected ChangesRequested=false")
	}
	if status.Required != 2 {
		t.Errorf("Required = %d, want 2", status.Required)
	}
	if len(status.ApprovedBy) != 2 {
		t.Errorf("ApprovedBy = %v, want 2 entries", status.ApprovedBy)
	}
}

func TestGitHub_ApprovalStatus_BlockedByChangesRequested(t *testing.T) {
	reviews := []ghReview{
		{ID: 1, User: ghUser{Login: "alice"}, State: "APPROVED"},
		{ID: 2, User: ghUser{Login: "bob"}, State: "CHANGES_REQUESTED"},
	}
	srv := makeApprovalSrv(t, "main", reviews, http.StatusForbidden, nil)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if status.Approved {
		t.Error("expected Approved=false (bob has outstanding CHANGES_REQUESTED)")
	}
	if !status.ChangesRequested {
		t.Error("expected ChangesRequested=true")
	}
	// Protection returned 403 → Required degrades to 0.
	if status.Required != 0 {
		t.Errorf("Required = %d, want 0 (protection unreadable)", status.Required)
	}
}

func TestGitHub_ApprovalStatus_CommentedDoesNotBlock(t *testing.T) {
	// alice: APPROVED then COMMENTED — COMMENTED must not reset the APPROVED state.
	reviews := []ghReview{
		{ID: 1, User: ghUser{Login: "alice"}, State: "APPROVED"},
		{ID: 2, User: ghUser{Login: "alice"}, State: "COMMENTED"},
	}
	srv := makeApprovalSrv(t, "main", reviews, http.StatusNotFound, nil)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if !status.Approved {
		t.Error("expected Approved=true (COMMENTED does not override prior APPROVED)")
	}
}

func TestGitHub_ApprovalStatus_BranchProtectionUnreadable(t *testing.T) {
	reviews := []ghReview{
		{ID: 1, User: ghUser{Login: "bot"}, State: "APPROVED"},
	}
	// 403 on protection endpoint → Required must degrade to 0.
	srv := makeApprovalSrv(t, "main", reviews, http.StatusForbidden, nil)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if !status.Approved {
		t.Error("expected Approved=true")
	}
	if status.Required != 0 {
		t.Errorf("Required = %d, want 0 (protection unreadable → graceful degrade)", status.Required)
	}
}

// TestGitHub_ApprovalStatus_NoReviews_Required0 guards the #279 baseline:
// a repo with no branch-protection review requirement (Required=0) and no reviews
// must return Approved=true so that CI-only auto-merge still fires.
func TestGitHub_ApprovalStatus_NoReviews_Required0(t *testing.T) {
	// Protection returns Required=0 (zero-value ghBranchProtection).
	srv := makeApprovalSrv(t, "main", nil, http.StatusOK, &ghBranchProtection{})
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	// 0 reviews >= Required(0) && !ChangesRequested → Approved=true.
	// This allows CI-gated repos to auto-merge without a human review.
	if !status.Approved {
		t.Error("expected Approved=true for no-reviews + Required=0 (CI-only gate repo)")
	}
	if len(status.ApprovedBy) != 0 {
		t.Errorf("ApprovedBy = %v, want empty", status.ApprovedBy)
	}
	if status.Required != 0 {
		t.Errorf("Required = %d, want 0", status.Required)
	}
}

// TestGitHub_ApprovalStatus_NoReviews_ProtectionUnreadable guards the same baseline
// when the protection endpoint is unreadable (403): Required degrades to 0, so an
// unprotected/CI-only repo still auto-merges (GitHub enforces if reviews are needed).
func TestGitHub_ApprovalStatus_NoReviews_ProtectionUnreadable(t *testing.T) {
	srv := makeApprovalSrv(t, "main", nil, http.StatusForbidden, nil)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if !status.Approved {
		t.Error("expected Approved=true for no-reviews + protection-unreadable (Required=0 degrade)")
	}
	if status.Required != 0 {
		t.Errorf("Required = %d, want 0 (graceful degrade)", status.Required)
	}
}

// TestGitHub_InlineComment_Payload verifies that InlineComment POSTs to
// /pulls/{n}/comments with the correct path, line, side, commit_id, and body.
func TestGitHub_InlineComment_Payload(t *testing.T) {
	pr := ghPR{Number: 7, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "headsha42"}}

	var gotURL string
	var gotBody map[string]any

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/pulls/7", serveGHJSON(t, pr))
	mux.HandleFunc("/repos/owner/repo/pulls/7/comments", func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.InlineComment(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 7,
		InlineComment{Path: "pkg/foo/bar.go", Line: 42, Side: "right"}, "nice change")
	if err != nil {
		t.Fatalf("InlineComment: %v", err)
	}

	if gotURL != "/repos/owner/repo/pulls/7/comments" {
		t.Errorf("URL: got %q, want /repos/owner/repo/pulls/7/comments", gotURL)
	}
	if gotBody["path"] != "pkg/foo/bar.go" {
		t.Errorf("path: got %v, want pkg/foo/bar.go", gotBody["path"])
	}
	if int(gotBody["line"].(float64)) != 42 {
		t.Errorf("line: got %v, want 42", gotBody["line"])
	}
	if gotBody["side"] != "RIGHT" {
		t.Errorf("side: got %v, want RIGHT", gotBody["side"])
	}
	if gotBody["commit_id"] != "headsha42" {
		t.Errorf("commit_id: got %v, want headsha42", gotBody["commit_id"])
	}
	if gotBody["body"] != "nice change" {
		t.Errorf("body: got %v, want 'nice change'", gotBody["body"])
	}
	// Confirm no review-level wrapping keys are present.
	if _, ok := gotBody["comments"]; ok {
		t.Error("payload must not contain 'comments' array (not using reviews endpoint)")
	}
	if _, ok := gotBody["event"]; ok {
		t.Error("payload must not contain 'event' field (not using reviews endpoint)")
	}
}

// TestGitHub_InlineComment_SideMapping verifies case-insensitive Side mapping.
func TestGitHub_InlineComment_SideMapping(t *testing.T) {
	cases := []struct {
		inputSide string
		wantSide  string
	}{
		{"left", "LEFT"},
		{"LEFT", "LEFT"},
		{"Left", "LEFT"},
		{"right", "RIGHT"},
		{"RIGHT", "RIGHT"},
		{"", "RIGHT"},
		{"other", "RIGHT"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("side="+tc.inputSide, func(t *testing.T) {
			pr := ghPR{Number: 1, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "sha1"}}
			var gotBody map[string]any

			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo/pulls/1", serveGHJSON(t, pr))
			mux.HandleFunc("/repos/owner/repo/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{}`))
			})

			srv := httptest.NewTLSServer(mux)
			defer srv.Close()

			adapter := newTestGHAdapter(srv)
			err := adapter.InlineComment(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 1,
				InlineComment{Path: "f.go", Line: 1, Side: tc.inputSide}, "body")
			if err != nil {
				t.Fatalf("InlineComment: %v", err)
			}
			if gotBody["side"] != tc.wantSide {
				t.Errorf("side: got %v, want %q", gotBody["side"], tc.wantSide)
			}
		})
	}
}

// TestGitHub_InlineComment_FallbackOnPRFetchError verifies that an error fetching
// the PR falls back to a plain issue comment containing "[path:line]".
func TestGitHub_InlineComment_FallbackOnPRFetchError(t *testing.T) {
	var issueCommentBody string

	mux := http.NewServeMux()
	// PR endpoint returns 500 — triggers fallback.
	mux.HandleFunc("/repos/owner/repo/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"internal error"}`))
	})
	mux.HandleFunc("/repos/owner/repo/issues/5/comments", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		issueCommentBody, _ = b["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.InlineComment(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 5,
		InlineComment{Path: "cmd/main.go", Line: 10, Side: "right"}, "fallback test")
	if err != nil {
		t.Fatalf("InlineComment (fallback): expected nil error from fallback path, got %v", err)
	}
	if !strings.Contains(issueCommentBody, "[cmd/main.go:10]") {
		t.Errorf("fallback issue comment body: got %q, want it to contain '[cmd/main.go:10]'", issueCommentBody)
	}
	if !strings.Contains(issueCommentBody, "fallback test") {
		t.Errorf("fallback issue comment body: got %q, want it to contain 'fallback test'", issueCommentBody)
	}
}

// TestGitHub_InlineComment_FallbackOnPostError verifies that a failure on the
// /pulls/{n}/comments POST falls back to a plain issue comment with "[path:line]".
func TestGitHub_InlineComment_FallbackOnPostError(t *testing.T) {
	pr := ghPR{Number: 3, State: "open", User: ghUser{Login: "u"}, Head: ghHeadRef{SHA: "sha99"}}
	var issueCommentBody string

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/pulls/3", serveGHJSON(t, pr))
	// Inline comments endpoint returns 422 (line not in diff) — triggers fallback.
	mux.HandleFunc("/repos/owner/repo/pulls/3/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"line not in diff"}`))
	})
	mux.HandleFunc("/repos/owner/repo/issues/3/comments", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		issueCommentBody, _ = b["body"].(string)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.InlineComment(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 3,
		InlineComment{Path: "internal/foo.go", Line: 7, Side: "left"}, "my note")
	if err != nil {
		t.Fatalf("InlineComment (fallback on POST error): expected nil, got %v", err)
	}
	if !strings.Contains(issueCommentBody, "[internal/foo.go:7]") {
		t.Errorf("fallback body: got %q, want '[internal/foo.go:7]'", issueCommentBody)
	}
	if !strings.Contains(issueCommentBody, "my note") {
		t.Errorf("fallback body: got %q, want 'my note'", issueCommentBody)
	}
}

// TestGitHub_ApprovalStatus_Required2_OneApproval tests that Required=2 with only
// 1 APPROVE returns Approved=false (threshold not met).
func TestGitHub_ApprovalStatus_Required2_OneApproval(t *testing.T) {
	reviews := []ghReview{
		{ID: 1, User: ghUser{Login: "alice"}, State: "APPROVED"},
	}
	prot := &ghBranchProtection{}
	prot.RequiredPullRequestReviews.RequiredApprovingReviewCount = 2

	srv := makeApprovalSrv(t, "main", reviews, http.StatusOK, prot)
	a := makeApprovalAdapter(t, srv)

	ref := RepoRef{Owner: "o", Name: "r"}
	status, err := a.GetApprovalStatus(t.Context(), ref, 1)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	// 1 approval < Required(2) → Approved=false.
	if status.Approved {
		t.Error("expected Approved=false (only 1 approval, Required=2)")
	}
	if status.Required != 2 {
		t.Errorf("Required = %d, want 2", status.Required)
	}
}

// — AddSubIssue tests —

// TestGitHub_Capabilities_HasEpicsFalse verifies GitHub adapter reports HasEpics=false.
func TestGitHub_Capabilities_HasEpicsFalse(t *testing.T) {
	// No server needed — Capabilities is a static return.
	adapter := &GitHubAdapter{}
	caps := adapter.Capabilities(context.Background())
	if caps.HasEpics {
		t.Error("Capabilities.HasEpics: got true, want false for GitHub adapter")
	}
}

// TestGitHub_AddSubIssue_NativePath verifies the happy path:
// GET issues/{child} → resolve global id → POST sub_issues with correct payload.
func TestGitHub_AddSubIssue_NativePath(t *testing.T) {
	const parentNum = 10
	const childNum = 20
	const childGlobalID int64 = 999888

	var gotSubIssuePayload map[string]any
	mux := http.NewServeMux()

	// Stub: GET /repos/owner/repo/issues/20
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", childNum), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		serveGHJSON(t, ghIssue{
			ID:     childGlobalID,
			Number: childNum,
			State:  "open",
			Title:  "child issue",
		})(w, r)
	})

	// Stub: POST /repos/owner/repo/issues/10/sub_issues
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d/sub_issues", parentNum), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&gotSubIssuePayload); err != nil {
			t.Errorf("decode sub_issues payload: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, parentNum, childNum)
	if err != nil {
		t.Fatalf("AddSubIssue: %v", err)
	}

	// Verify the payload contains the child's global id, not its number.
	if gotSubIssuePayload == nil {
		t.Fatal("sub_issues POST was never called")
	}
	gotID, ok := gotSubIssuePayload["sub_issue_id"].(float64) // JSON numbers decode as float64
	if !ok {
		t.Fatalf("sub_issue_id missing or wrong type in payload: %v", gotSubIssuePayload)
	}
	if int64(gotID) != childGlobalID {
		t.Errorf("sub_issue_id = %d, want %d", int64(gotID), childGlobalID)
	}
}

// TestGitHub_AddSubIssue_Fallback verifies that when the sub_issues POST fails (422),
// the parent body is updated with a "- [ ] #<child>" task-list entry.
func TestGitHub_AddSubIssue_Fallback(t *testing.T) {
	const parentNum = 10
	const childNum = 20
	const childGlobalID int64 = 777666
	const parentOriginalBody = "Parent issue body."

	var patchedBody string
	mux := http.NewServeMux()

	// Stub: GET /repos/owner/repo/issues/20 (child)
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", childNum), func(w http.ResponseWriter, r *http.Request) {
		serveGHJSON(t, ghIssue{ID: childGlobalID, Number: childNum, State: "open"})(w, r)
	})

	// Stub: POST /repos/owner/repo/issues/10/sub_issues → 422 (feature not enabled)
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d/sub_issues", parentNum), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Sub-issues not enabled"}`))
	})

	// Stub: GET /repos/owner/repo/issues/10 (parent — for fallback)
	// Stub: PATCH /repos/owner/repo/issues/10 (parent body update)
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", parentNum), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveGHJSON(t, ghIssue{ID: 111, Number: parentNum, State: "open", Body: parentOriginalBody})(w, r)
		case http.MethodPatch:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode PATCH payload: %v", err)
			}
			if b, ok := payload["body"].(string); ok {
				patchedBody = b
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, parentNum, childNum)
	if err != nil {
		t.Fatalf("AddSubIssue (fallback): expected nil error, got %v", err)
	}

	taskEntry := fmt.Sprintf("- [ ] #%d", childNum)
	if !strings.Contains(patchedBody, taskEntry) {
		t.Errorf("patched body %q does not contain task entry %q", patchedBody, taskEntry)
	}
	if !strings.Contains(patchedBody, parentOriginalBody) {
		t.Errorf("patched body %q does not preserve original body %q", patchedBody, parentOriginalBody)
	}
}

// TestGitHub_AddSubIssue_Idempotent verifies that calling AddSubIssue when the
// task-list entry already exists does not duplicate it.
func TestGitHub_AddSubIssue_Idempotent(t *testing.T) {
	const parentNum = 10
	const childNum = 20
	const childGlobalID int64 = 555444
	taskEntry := fmt.Sprintf("- [ ] #%d", childNum)
	parentBody := "Existing tasks:\n" + taskEntry

	patchCalled := false
	mux := http.NewServeMux()

	// Child GET
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", childNum), func(w http.ResponseWriter, r *http.Request) {
		serveGHJSON(t, ghIssue{ID: childGlobalID, Number: childNum, State: "open"})(w, r)
	})

	// Sub-issues POST → 422
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d/sub_issues", parentNum), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"not enabled"}`))
	})

	// Parent GET / PATCH
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", parentNum), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveGHJSON(t, ghIssue{ID: 111, Number: parentNum, State: "open", Body: parentBody})(w, r)
		case http.MethodPatch:
			patchCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, parentNum, childNum)
	if err != nil {
		t.Fatalf("AddSubIssue (idempotent): %v", err)
	}
	if patchCalled {
		t.Error("PATCH was called even though task-list entry already existed (idempotency violation)")
	}
}

// TestGitHub_AddSubIssue_PrefixCollision guards against the idempotency check
// treating #5 as already-present when the body contains #50.
// Parent body has "- [ ] #50"; we add child #5 → body must gain "- [ ] #5".
func TestGitHub_AddSubIssue_PrefixCollision(t *testing.T) {
	const parentNum = 10
	const childNum = 5
	const childGlobalID int64 = 333222
	parentBody := "- [ ] #50"

	var patchedBody string
	mux := http.NewServeMux()

	// Child GET
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", childNum), func(w http.ResponseWriter, r *http.Request) {
		serveGHJSON(t, ghIssue{ID: childGlobalID, Number: childNum, State: "open"})(w, r)
	})

	// Sub-issues POST → 422
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d/sub_issues", parentNum), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"not enabled"}`))
	})

	// Parent GET / PATCH
	mux.HandleFunc(fmt.Sprintf("/repos/owner/repo/issues/%d", parentNum), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveGHJSON(t, ghIssue{ID: 111, Number: parentNum, State: "open", Body: parentBody})(w, r)
		case http.MethodPatch:
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode PATCH payload: %v", err)
			}
			if b, ok := payload["body"].(string); ok {
				patchedBody = b
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, parentNum, childNum)
	if err != nil {
		t.Fatalf("AddSubIssue (prefix collision): %v", err)
	}

	wantEntry := fmt.Sprintf("- [ ] #%d", childNum)
	if !strings.Contains(patchedBody, wantEntry) {
		t.Errorf("patched body %q does not contain %q (child was incorrectly treated as duplicate of #50)", patchedBody, wantEntry)
	}
}

// TestGitHub_AddLabel_PostsAdditive verifies AddLabel POSTs to the issues
// labels endpoint (additive per the GitHub API — never replaces the set,
// unlike SetLabels' PUT).
func TestGitHub_AddLabel_PostsAdditive(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	if err := adapter.AddLabel(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 42, "pinard:reviewed"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/repos/owner/repo/issues/42/labels" {
		t.Errorf("path: got %q", gotPath)
	}
	labels, _ := gotBody["labels"].([]any)
	if len(labels) != 1 || labels[0] != "pinard:reviewed" {
		t.Errorf("labels body: got %v, want [\"pinard:reviewed\"]", gotBody["labels"])
	}
}

// TestGitHub_RemoveLabel_Deletes verifies RemoveLabel DELETEs the specific
// label path.
func TestGitHub_RemoveLabel_Deletes(t *testing.T) {
	var gotMethod, gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/labels/pinard:reviewed", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	if err := adapter.RemoveLabel(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 42, "pinard:reviewed"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method: got %q, want DELETE", gotMethod)
	}
	if gotPath != "/repos/owner/repo/issues/42/labels/pinard:reviewed" {
		t.Errorf("path: got %q", gotPath)
	}
}

// TestGitHub_RemoveLabel_404IsSuccess verifies that removing a label the
// issue/PR doesn't have (404) is treated as a successful no-op — callers
// invoke this idempotently on every SHA change.
func TestGitHub_RemoveLabel_404IsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/labels/pinard:reviewed", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestGHAdapter(srv)
	if err := adapter.RemoveLabel(context.Background(), RepoRef{Owner: "owner", Name: "repo"}, 42, "pinard:reviewed"); err != nil {
		t.Errorf("RemoveLabel on a 404 should be a no-op success, got: %v", err)
	}
}
