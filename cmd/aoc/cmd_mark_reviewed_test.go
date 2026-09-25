package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/watcher"
)

// fakePressoir is a minimal pressoir.Pressoir stand-in that records call
// order for Comment/AddLabel and can be made to fail either call. Any other
// method is unimplemented (nil embed) — markMRReviewed never touches them.
type fakePressoir struct {
	pressoir.Pressoir
	calls      []string
	commentErr error
	labelErr   error
	lastNote   string
}

func (f *fakePressoir) Comment(_ context.Context, _ pressoir.RepoRef, _ int, body string) error {
	f.calls = append(f.calls, "comment")
	f.lastNote = body
	return f.commentErr
}

func (f *fakePressoir) AddLabel(_ context.Context, _ pressoir.RepoRef, _ int, _ string) error {
	f.calls = append(f.calls, "label")
	return f.labelErr
}

func TestBuildReviewedNoteBody(t *testing.T) {
	body := buildReviewedNoteBody("pinard", "checked the diff, no findings")
	if !strings.Contains(body, "the pinard maître") {
		t.Errorf("buildReviewedNoteBody(%q) = %q, want it to name the parcelle's maître", "pinard", body)
	}
	if !strings.Contains(body, "checked the diff, no findings") {
		t.Errorf("buildReviewedNoteBody(...) = %q, want it to contain the summary", body)
	}
	if strings.Contains(body, watcher.ConductorMarker) {
		t.Errorf("buildReviewedNoteBody(...) = %q, must NOT contain %q — it has to stay unmarked", body, watcher.ConductorMarker)
	}
}

func TestBuildReviewedNoteBody_NoParcelle(t *testing.T) {
	body := buildReviewedNoteBody("", "looks good")
	if !strings.Contains(body, "the maître") {
		t.Errorf("buildReviewedNoteBody(\"\", ...) = %q, want a generic fallback", body)
	}
	if strings.Contains(body, "the  maître") {
		t.Errorf("buildReviewedNoteBody(\"\", ...) = %q, want no double space from an empty parcelle", body)
	}
}

// BUG REGRESSION (#335): a blank/whitespace-only summary must be rejected
// before any network call — the required-summary is a quality
// forcing-function, not just an audit-trail nicety.
func TestMarkMRReviewedCmd_BlankSummaryRejected(t *testing.T) {
	cmd := markMRReviewedCmd
	if err := cmd.Flags().Set("project", "some-project"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("mr", "42"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Flags().Set("project", "")
		_ = cmd.Flags().Set("mr", "0")
		_ = cmd.Flags().Set("summary", "")
	}()

	for _, summary := range []string{"", "   ", "\t\n"} {
		if err := cmd.Flags().Set("summary", summary); err != nil {
			t.Fatal(err)
		}
		err := cmd.RunE(cmd, nil)
		if err == nil {
			t.Errorf("RunE with summary=%q: want an error, got nil", summary)
			continue
		}
		if !strings.Contains(err.Error(), "--summary") {
			t.Errorf("RunE with summary=%q: err = %q, want it to mention --summary", summary, err)
		}
	}
}

// BUG REGRESSION (#335): note-then-label ordering. The note must be posted
// before the label is applied, and a note-post failure must short-circuit
// before AddLabel is ever called — otherwise a failed note silently degrades
// to today's label-only ack with no human-visible record.
func TestMarkMRReviewed_NoteBeforeLabel(t *testing.T) {
	fp := &fakePressoir{}
	repoRef := pressoir.RepoRef{Owner: "group", Name: "repo"}

	if err := markMRReviewed(context.Background(), fp, repoRef, 42, "pinard", "looked at the diff"); err != nil {
		t.Fatalf("markMRReviewed: unexpected error: %v", err)
	}
	if want := []string{"comment", "label"}; !equalStrings(fp.calls, want) {
		t.Errorf("call order = %v, want %v (note before label)", fp.calls, want)
	}
	if !strings.Contains(fp.lastNote, "pinard") || !strings.Contains(fp.lastNote, "looked at the diff") {
		t.Errorf("posted note = %q, want it to name the parcelle and the summary", fp.lastNote)
	}
}

func TestMarkMRReviewed_NoteFailureSkipsLabel(t *testing.T) {
	fp := &fakePressoir{commentErr: errors.New("network down")}
	repoRef := pressoir.RepoRef{Owner: "group", Name: "repo"}

	err := markMRReviewed(context.Background(), fp, repoRef, 42, "pinard", "looked at the diff")
	if err == nil {
		t.Fatal("markMRReviewed: want an error when the note fails to post")
	}
	if want := []string{"comment"}; !equalStrings(fp.calls, want) {
		t.Errorf("call order = %v, want %v (label must not be applied when the note fails)", fp.calls, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
