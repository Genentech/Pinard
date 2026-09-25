package pressoir

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/gitlab"
)

// newTestAdapter creates a GitLabAdapter pointed at a TLS test server.
func newTestAdapter(srv *httptest.Server) *GitLabAdapter {
	// Strip the scheme — the gitlab client prepends "https://".
	host := strings.TrimPrefix(srv.URL, "https://")
	return &GitLabAdapter{
		client: &gitlab.Client{
			Host:  host,
			Token: "test-token",
			HTTP:  srv.Client(), // uses the test server's TLS transport
		},
	}
}

func serveJSON(t *testing.T, v any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("serveJSON: encode: %v", err)
		}
	}
}

// TestGetPR_IIDToNumber verifies that GitLab MR.IID is surfaced as PullRequest.Number.
func TestGetPR_IIDToNumber(t *testing.T) {
	mr := gitlab.MergeRequest{
		IID:            42,
		State:          "opened",
		Title:          "My MR",
		Description:    "body",
		Labels:         []string{"bug"},
		SourceBranch:   "feature/x",
		WebURL:         "https://example.com/mr/42",
		Author:         gitlab.Author{Username: "alice"},
		MergeCommitSHA: "abc123",
	}

	srv := httptest.NewTLSServer(serveJSON(t, mr))
	defer srv.Close()

	adapter := newTestAdapter(srv)
	pr, err := adapter.GetPR(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 42)
	if err != nil {
		t.Fatalf("GetPR: %v", err)
	}

	if pr.Number != 42 {
		t.Errorf("Number: got %d, want 42", pr.Number)
	}
	if pr.State != "opened" {
		t.Errorf("State: got %q, want %q", pr.State, "opened")
	}
	if pr.Title != "My MR" {
		t.Errorf("Title: got %q, want %q", pr.Title, "My MR")
	}
	if pr.Author != "alice" {
		t.Errorf("Author: got %q, want %q", pr.Author, "alice")
	}
	if pr.MergeSHA != "abc123" {
		t.Errorf("MergeSHA: got %q, want %q", pr.MergeSHA, "abc123")
	}
}

// TestListPRNotes_InlineMapping verifies that Notes with Position are mapped to InlineComment.
func TestListPRNotes_InlineMapping(t *testing.T) {
	notes := []gitlab.Note{
		{
			ID:     1,
			Body:   "top-level comment",
			Author: gitlab.Author{Username: "bob"},
		},
		{
			ID:     2,
			Body:   "inline comment",
			Author: gitlab.Author{Username: "carol"},
			Position: gitlab.Position{
				NewPath: "internal/foo/bar.go",
				NewLine: 17,
			},
			DiscussionID: "disc-abc",
		},
	}

	srv := httptest.NewTLSServer(serveJSON(t, notes))
	defer srv.Close()

	adapter := newTestAdapter(srv)
	comments, err := adapter.ListPRNotes(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 1)
	if err != nil {
		t.Fatalf("ListPRNotes: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("len(comments): got %d, want 2", len(comments))
	}

	// First comment: top-level, no inline.
	if comments[0].InlineComment != nil {
		t.Errorf("comments[0].InlineComment: expected nil for top-level comment")
	}
	if comments[0].Author != "bob" {
		t.Errorf("comments[0].Author: got %q, want %q", comments[0].Author, "bob")
	}

	// Second comment: inline.
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
	if comments[1].DiscussionID != "disc-abc" {
		t.Errorf("DiscussionID: got %q, want %q", comments[1].DiscussionID, "disc-abc")
	}
}

// TestCIStatusFor_PipelineMapping verifies pipeline status -> CIStatus.State mapping.
func TestCIStatusFor_PipelineMapping(t *testing.T) {
	cases := []struct {
		gitlabStatus string
		wantState    string
	}{
		{"success", "success"},
		{"failed", "failed"},
		{"canceled", "failed"},
		{"running", "running"},
		{"pending", "pending"},
		{"created", "pending"},
		{"waiting_for_resource", "pending"},
		{"preparing", "pending"},
		{"scheduled", "pending"},
		{"skipped", "none"},
		{"manual", "none"},
		{"", "none"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.gitlabStatus, func(t *testing.T) {
			pipelines := []gitlab.Pipeline{
				{ID: 1, Status: tc.gitlabStatus, WebURL: "https://example.com/pipelines/1"},
			}
			srv := httptest.NewTLSServer(serveJSON(t, pipelines))
			defer srv.Close()

			adapter := newTestAdapter(srv)
			status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 1)
			if err != nil {
				t.Fatalf("CIStatusFor: %v", err)
			}
			if status.State != tc.wantState {
				t.Errorf("State: got %q, want %q (gitlab status %q)", status.State, tc.wantState, tc.gitlabStatus)
			}
		})
	}
}

// TestCIStatusFor_NoPipelines verifies that an empty pipeline list returns state "none".
func TestCIStatusFor_NoPipelines(t *testing.T) {
	srv := httptest.NewTLSServer(serveJSON(t, []gitlab.Pipeline{}))
	defer srv.Close()

	adapter := newTestAdapter(srv)
	status, err := adapter.CIStatusFor(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 1)
	if err != nil {
		t.Fatalf("CIStatusFor: %v", err)
	}
	if status.State != "none" {
		t.Errorf("State: got %q, want %q", status.State, "none")
	}
}

// TestCapabilities_GitLab verifies that the GitLab adapter reports HasEpics=true.
func TestCapabilities_GitLab(t *testing.T) {
	adapter := &GitLabAdapter{}
	caps := adapter.Capabilities(context.Background())
	if !caps.HasEpics {
		t.Error("Capabilities.HasEpics: got false, want true for GitLab adapter")
	}
}

// TestGetIssue_IIDToNumber verifies that GitLab issue iid is surfaced as Issue.Number.
func TestGetIssue_IIDToNumber(t *testing.T) {
	iss := gitlab.Issue{
		IID:         99,
		State:       "opened",
		Title:       "A bug",
		Description: "details",
		Labels:      []string{"bug"},
		WebURL:      "https://example.com/issues/99",
		Author:      gitlab.Author{Username: "dave"},
	}

	srv := httptest.NewTLSServer(serveJSON(t, iss))
	defer srv.Close()

	adapter := newTestAdapter(srv)
	issue, err := adapter.GetIssue(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 99)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if issue.Number != 99 {
		t.Errorf("Number: got %d, want 99", issue.Number)
	}
	if issue.Author != "dave" {
		t.Errorf("Author: got %q, want %q", issue.Author, "dave")
	}
}

// TestMrToPR_AllFields verifies the full field mapping from gitlab.MergeRequest.
func TestMrToPR_AllFields(t *testing.T) {
	mr := gitlab.MergeRequest{
		IID:            7,
		State:          "merged",
		Title:          "Fix thing",
		Description:    "desc",
		Labels:         []string{"enhancement", "ready"},
		SourceBranch:   "fix/thing",
		WebURL:         "https://gl.example.com/mr/7",
		Author:         gitlab.Author{Username: "eve"},
		MergeCommitSHA: "deadbeef",
	}
	pr := mrToPR(mr)
	if pr.Number != 7 {
		t.Errorf("Number: got %d", pr.Number)
	}
	if pr.State != "merged" {
		t.Errorf("State: got %q", pr.State)
	}
	if pr.Body != "desc" {
		t.Errorf("Body: got %q", pr.Body)
	}
	if len(pr.Labels) != 2 || pr.Labels[0] != "enhancement" {
		t.Errorf("Labels: got %v", pr.Labels)
	}
	if pr.SourceBranch != "fix/thing" {
		t.Errorf("SourceBranch: got %q", pr.SourceBranch)
	}
	if pr.WebURL != "https://gl.example.com/mr/7" {
		t.Errorf("WebURL: got %q", pr.WebURL)
	}
}

// TestNoteToComment_InlineDetection tests inline vs top-level note detection.
func TestNoteToComment_InlineDetection(t *testing.T) {
	topLevel := noteToComment(gitlab.Note{
		ID: 1, Body: "hi", Author: gitlab.Author{Username: "u"},
	})
	if topLevel.InlineComment != nil {
		t.Error("expected nil InlineComment for note with empty position")
	}

	inline := noteToComment(gitlab.Note{
		ID: 2, Body: "here", Author: gitlab.Author{Username: "v"},
		Position: gitlab.Position{NewPath: "main.go", NewLine: 5},
	})
	if inline.InlineComment == nil {
		t.Fatal("expected non-nil InlineComment for note with position")
	}
	if inline.InlineComment.Side != "right" {
		t.Errorf("Side: got %q, want right", inline.InlineComment.Side)
	}
}

// TestMapPipelineState_AllCases tests the mapPipelineState helper directly.
func TestMapPipelineState_AllCases(t *testing.T) {
	cases := map[string]string{
		"success":              "success",
		"failed":               "failed",
		"canceled":             "failed",
		"running":              "running",
		"pending":              "pending",
		"created":              "pending",
		"waiting_for_resource": "pending",
		"preparing":            "pending",
		"scheduled":            "pending",
		"skipped":              "none",
		"manual":               "none",
		"unknown":              "none",
	}
	for input, want := range cases {
		got := mapPipelineState(input)
		if got != want {
			t.Errorf("mapPipelineState(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGitLab_ApprovalStatus_Wraps_GetMRApprovals(t *testing.T) {
	approvals := gitlab.Approvals{
		Approved: true,
		ApprovedBy: []gitlab.Author{
			{Username: "alice"},
			{Username: "bob"},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Frepo/merge_requests/7/approvals", serveJSON(t, approvals))
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv)
	ref := RepoRef{Owner: "group", Name: "repo"}

	status, err := a.GetApprovalStatus(context.Background(), ref, 7)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if !status.Approved {
		t.Error("expected Approved=true")
	}
	if len(status.ApprovedBy) != 2 {
		t.Errorf("ApprovedBy = %v, want [alice bob]", status.ApprovedBy)
	}
	wantBy := map[string]bool{"alice": true, "bob": true}
	for _, u := range status.ApprovedBy {
		if !wantBy[u] {
			t.Errorf("unexpected ApprovedBy entry %q", u)
		}
	}
	// Required is always 0 for GitLab (Approved bool is authoritative).
	if status.Required != 0 {
		t.Errorf("Required = %d, want 0", status.Required)
	}
}

func TestGitLab_ApprovalStatus_NotApproved(t *testing.T) {
	approvals := gitlab.Approvals{Approved: false}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Frepo/merge_requests/8/approvals", serveJSON(t, approvals))
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	a := newTestAdapter(srv)
	ref := RepoRef{Owner: "group", Name: "repo"}

	status, err := a.GetApprovalStatus(context.Background(), ref, 8)
	if err != nil {
		t.Fatalf("GetApprovalStatus: %v", err)
	}
	if status.Approved {
		t.Error("expected Approved=false")
	}
	if len(status.ApprovedBy) != 0 {
		t.Errorf("ApprovedBy = %v, want empty", status.ApprovedBy)
	}
}

// — AddSubIssue tests —

// TestGitLab_Capabilities_HasEpicsTrue verifies the GitLab adapter reports HasEpics=true.
// (Duplicates TestCapabilities_GitLab for symmetry with GitHub test; both should pass.)
func TestGitLab_Capabilities_HasEpicsTrue(t *testing.T) {
	adapter := &GitLabAdapter{}
	caps := adapter.Capabilities(context.Background())
	if !caps.HasEpics {
		t.Error("Capabilities.HasEpics: got false, want true for GitLab adapter")
	}
}

// TestGitLab_AddSubIssue_NonErroring verifies that AddSubIssue always returns nil
// on GitLab, even when the underlying issue-link call fails.
func TestGitLab_AddSubIssue_NonErroring(t *testing.T) {
	// Serve a 404 for the issue-link endpoint — AddSubIssue must not propagate it.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 1, 2)
	if err != nil {
		t.Errorf("AddSubIssue: expected nil error (non-erroring contract), got %v", err)
	}
}

// TestGitLab_AddSubIssue_CallsIssueLink verifies that AddSubIssue invokes the
// issue-link endpoint with the correct project and issue numbers.
func TestGitLab_AddSubIssue_CallsIssueLink(t *testing.T) {
	linkCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Frepo/issues/5/links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		linkCalled = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestAdapter(srv)
	err := adapter.AddSubIssue(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 5, 7)
	if err != nil {
		t.Errorf("AddSubIssue: unexpected error %v", err)
	}
	if !linkCalled {
		t.Error("expected issue-link POST to be called, but it was not")
	}
}

// TestGitLab_AddLabel_HitsMergeRequestEndpoint verifies that AddLabel PUTs
// add_labels to the merge_requests endpoint (not issues, which SetLabels
// mistakenly targets for MRs) and never replaces the existing label set.
func TestGitLab_AddLabel_HitsMergeRequestEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Frepo/merge_requests/42", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotMethod = r.Method
		_ = r.ParseForm()
		gotBody = r.Form.Get("add_labels")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/api/v4/projects/group%2Frepo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AddLabel must not hit the issues endpoint for a MR, got %s %s", r.Method, r.URL.Path)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestAdapter(srv)
	if err := adapter.AddLabel(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 42, "pinard:reviewed"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method: got %q, want PUT", gotMethod)
	}
	if gotPath != "/api/v4/projects/group%2Frepo/merge_requests/42" {
		t.Errorf("path: got %q", gotPath)
	}
	if gotBody != "pinard:reviewed" {
		t.Errorf("add_labels param: got %q, want %q", gotBody, "pinard:reviewed")
	}
}

// TestGitLab_RemoveLabel_HitsMergeRequestEndpoint mirrors AddLabel for the
// remove_labels param.
func TestGitLab_RemoveLabel_HitsMergeRequestEndpoint(t *testing.T) {
	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/group%2Frepo/merge_requests/42", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotBody = r.Form.Get("remove_labels")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	adapter := newTestAdapter(srv)
	if err := adapter.RemoveLabel(context.Background(), RepoRef{Owner: "group", Name: "repo"}, 42, "pinard:reviewed"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if gotBody != "pinard:reviewed" {
		t.Errorf("remove_labels param: got %q, want %q", gotBody, "pinard:reviewed")
	}
}
