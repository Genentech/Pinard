package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/watcher"
	"github.com/spf13/cobra"
)

// buildReviewedNoteBody composes the unmarked, human-visible review note.
// Deliberately does NOT include watcher.ConductorMarker: the forge identity
// is shared with the vendangeur/conductor, so an unmarked note is the thing
// that keeps this silent to the agent pipeline (see shouldForwardNote) while
// still being visible to a human reading the MR. Naming the reviewing
// parcelle compensates for that shared identity — a bare label can't tell a
// human which maître looked at it.
func buildReviewedNoteBody(parcelle, summary string) string {
	who := "the maître"
	if parcelle != "" {
		who = fmt.Sprintf("the %s maître", parcelle)
	}
	return fmt.Sprintf("🍇 Reviewed by %s — %s", who, strings.TrimSpace(summary))
}

// markMRReviewed posts the unmarked review note, then applies
// watcher.ReviewedLabel. Note-then-label, not the reverse: a note-post
// failure must short-circuit before the label is applied, so the MR is never
// left with a silent label-only ack.
func markMRReviewed(ctx context.Context, pr pressoir.Pressoir, repoRef pressoir.RepoRef, mr int, parcelle, summary string) error {
	if err := pr.Comment(ctx, repoRef, mr, buildReviewedNoteBody(parcelle, summary)); err != nil {
		return fmt.Errorf("failed to post review note (label not applied, retry): %w", err)
	}
	return pr.AddLabel(ctx, repoRef, mr, watcher.ReviewedLabel)
}

// aoc mark-mr-reviewed --project <p>|--repo <r> --mr <n> --summary <s>
//
// Records a maître's "reviewed, nothing to say" ALL-CLEAR: posts an unmarked,
// human-visible note naming the reviewing maître and the required --summary,
// then applies the watcher.ReviewedLabel label (additive, never disturbs
// other labels) so the mr-watcher stops re-dispatching needs_review for this
// head SHA. The note carries no watcher.ConductorMarker, so it is invisible
// to the agent pipeline (shouldForwardNote filters it) — no vendangeur turn,
// no re-dispatch — while still giving a human a record of who reviewed and
// what was checked. Cross-forge (GitLab notes / GitHub issue comments), and
// deliberately does NOT call GitLab's Approve.
//
// An earlier version also self-approved on GitLab (self-approval there
// auto-invalidates on push, unlike a label). MR !608 review flagged that as
// a governance bug: with no user/group restriction on a project's GitLab
// approval rule (the default, and the case for at least one live vignoble),
// the bot's own approval can satisfy the required-approvals count on its own
// MR — silently removing the human approval gate on vignobles configured
// auto_merge: false. Approval is the human/forge's responsibility (#289's
// actual intent, not just "don't use the owner token"); the label is the
// only thing the watcher reads (pr.Labels) and is sufficient on its own for
// every Defect-4 acceptance criterion, so Approve() was removed rather than
// gated — it bought nothing functionally and cost this failure mode.
//
// Uses the maître's own identity (newPressoirForRepo), never the owner token
// (PINARD_OWNER_GITLAB_TOKEN) — per the #289 governance decision, Pinard
// must never auto-approve as the human owner.
var markMRReviewedCmd = &cobra.Command{
	Use:   "mark-mr-reviewed",
	Short: "Record a silent ALL-CLEAR review ack on a MR (unmarked note + label)",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		repo, _ := cmd.Flags().GetString("repo")
		mr, _ := cmd.Flags().GetInt("mr")
		summary, _ := cmd.Flags().GetString("summary")
		parcelle, _ := cmd.Flags().GetString("parcelle")
		if (project == "" && repo == "") || mr == 0 {
			return fmt.Errorf("--mr is required, and one of --project or --repo")
		}
		if strings.TrimSpace(summary) == "" {
			return fmt.Errorf("--summary is required and must not be blank — state what was checked")
		}
		if parcelle == "" {
			parcelle = os.Getenv("PINARD_PARCELLE")
		}

		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}

		if repo == "" {
			vigne, ok := vb.Config.Vignes[project]
			if !ok || vigne.Repo == "" {
				return fmt.Errorf("project %q not found in vignes.yaml", project)
			}
			repo = vigne.Repo
		}

		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		repoRef := pressoir.RepoRefFromPath(repo)
		ctx := context.Background()

		if err := markMRReviewed(ctx, pr, repoRef, mr, parcelle, summary); err != nil {
			return err
		}

		fmt.Printf("Marked MR !%d (%s) reviewed — no vendangeur turn; posted an unmarked review note.\n", mr, repo)
		return nil
	},
}

func init() {
	markMRReviewedCmd.Flags().String("project", "", "Vigne/project name from vignes.yaml")
	markMRReviewedCmd.Flags().String("repo", "", "Repository path (overrides --project lookup)")
	markMRReviewedCmd.Flags().Int("mr", 0, "Merge request IID (number)")
	markMRReviewedCmd.Flags().String("summary", "", "What was checked (required) — posted as an unmarked, human-visible MR note")
	markMRReviewedCmd.Flags().String("parcelle", "", "Reviewing parcelle name (defaults to $PINARD_PARCELLE)")
	rootCmd.AddCommand(markMRReviewedCmd)
}
