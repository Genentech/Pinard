package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/spf13/cobra"
)

// mrTitlePattern is the repo-wide MR/commit title convention: only fix:/feat:
// prefixes are allowed, with an optional (scope), and a non-empty subject.
var mrTitlePattern = regexp.MustCompile(`^(fix|feat)(\([^)]+\))?: .+`)

// validateMRTitle enforces the fix:/feat: MR title convention at the single
// seam every MR-opening path goes through (aoc pressoir open-pr), so a
// non-conforming title is rejected before any network call is made.
func validateMRTitle(title string) error {
	if !mrTitlePattern.MatchString(title) {
		return fmt.Errorf("MR title %q does not follow the required convention: must start with \"fix:\", \"feat:\", \"fix(scope):\", or \"feat(scope):\" (scope optional, prefix is not) — e.g. \"fix(ci): …\" or \"feat: …\"; other prefixes like \"docs:\", \"chore:\", \"ops:\", or \"observability:\" are not permitted", title)
	}
	return nil
}

// pressoirCmd is the top-level `aoc pressoir` group.
// Subcommands expose the Pressoir interface over stdout (JSON) so the extension
// can call `aoc pressoir …` instead of shelling out to `glab` or `gh`.
var pressoirCmd = &cobra.Command{
	Use:   "pressoir",
	Short: "Provider-neutral pressoir operations (issue read, PR comment, user resolve, …)",
}

// newPressoir loads credentials and vignoble config and returns a Pressoir for
// the given repo path. Falls back to the GitLab adapter on error.
func newPressoirForRepo(creds *config.Credentials, vb *config.Vignoble, repo string) pressoir.Pressoir {
	pCfg := vb.ResolvePressoirConfig(repo)
	pr, err := pressoir.NewPressoir(pCfg, creds)
	if err != nil {
		pr = pressoir.NewGitLabAdapter(creds.GitLab.Host, creds.Token())
	}
	return pr
}

// aoc pressoir get-issue --repo <r> --number <n>
var pressoirGetIssueCmd = &cobra.Command{
	Use:   "get-issue",
	Short: "Fetch an issue and print it as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		issue, err := pr.GetIssue(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		return printJSON(issue)
	},
}

// aoc pressoir list-pr-notes --repo <r> --number <n>
var pressoirListPRNotesCmd = &cobra.Command{
	Use:   "list-pr-notes",
	Short: "List PR/MR comments and print them as a JSON array",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		notes, err := pr.ListPRNotes(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		return printJSON(notes)
	},
}

// aoc pressoir resolve-user --username <u> [--repo <r>]
var pressoirResolveUserCmd = &cobra.Command{
	Use:   "resolve-user",
	Short: "Resolve a username to a provider user identity (JSON)",
	RunE: func(cmd *cobra.Command, args []string) error {
		username, _ := cmd.Flags().GetString("username")
		if username == "" {
			return fmt.Errorf("--username is required")
		}
		repo, _ := cmd.Flags().GetString("repo")
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		user, err := pr.ResolveUser(context.Background(), username)
		if err != nil {
			return err
		}
		return printJSON(user)
	},
}

// aoc pressoir comment-pr --repo <r> --number <n> --body <b>
var pressoirCommentPRCmd = &cobra.Command{
	Use:   "comment-pr",
	Short: "Post a top-level comment on a PR/MR",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		body, _ := cmd.Flags().GetString("body")
		if repo == "" || number == 0 || body == "" {
			return fmt.Errorf("--repo, --number, and --body are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		return pr.Comment(context.Background(), pressoir.RepoRefFromPath(repo), number, body)
	},
}

// aoc pressoir comment-issue --repo <r> --number <n> --body <b>
var pressoirCommentIssueCmd = &cobra.Command{
	Use:   "comment-issue",
	Short: "Post a top-level comment on an issue",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		body, _ := cmd.Flags().GetString("body")
		if repo == "" || number == 0 || body == "" {
			return fmt.Errorf("--repo, --number, and --body are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		return pr.PostIssueNote(context.Background(), pressoir.RepoRefFromPath(repo), number, body)
	},
}

// aoc pressoir get-pr --repo <r> --number <n>
var pressoirGetPRCmd = &cobra.Command{
	Use:   "get-pr",
	Short: "Fetch a PR/MR and print it as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		result, err := pr.GetPR(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		return printJSON(result)
	},
}

// aoc pressoir track-pr --repo <r> --number <n> --session <s>
// Thin wrapper around track-mr for extension use: registers the PR with the
// MR watcher so review comments are forwarded to the worker's inbox.
var pressoirTrackPRCmd = &cobra.Command{
	Use:   "track-pr",
	Short: "Register a PR/MR with the watcher (thin wrapper over track-mr)",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		session, _ := cmd.Flags().GetString("session")
		project, _ := cmd.Flags().GetString("project")
		if number == 0 || session == "" {
			return fmt.Errorf("--number and --session are required")
		}
		// Delegate to trackMRCmd logic by building equivalent flags.
		// Reuse the existing track-mr command so the two paths stay in sync.
		trackMRCmd.Flags().Set("mr", fmt.Sprintf("%d", number))
		trackMRCmd.Flags().Set("session", session)
		if repo != "" {
			trackMRCmd.Flags().Set("repo", repo)
		}
		if project != "" {
			trackMRCmd.Flags().Set("project", project)
		}
		return trackMRCmd.RunE(trackMRCmd, nil)
	},
}

// aoc pressoir list-issue-notes --repo <r> --number <n>
var pressoirListIssueNotesCmd = &cobra.Command{
	Use:   "list-issue-notes",
	Short: "List issue comments and print them as a JSON array",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		notes, err := pr.ListIssueNotes(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		return printJSON(notes)
	},
}

// aoc pressoir update-issue --repo <r> --number <n> [--labels l] [--add-labels l] [--remove-labels l] [--state-event open|close] [--assignee u]
var pressoirUpdateIssueCmd = &cobra.Command{
	Use:   "update-issue",
	Short: "Update an issue (labels, state, assignee) and print the updated issue as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		params := map[string]string{}
		if v, _ := cmd.Flags().GetString("labels"); v != "" {
			params["labels"] = v
		}
		if v, _ := cmd.Flags().GetString("add-labels"); v != "" {
			params["add_labels"] = v
		}
		if v, _ := cmd.Flags().GetString("remove-labels"); v != "" {
			params["remove_labels"] = v
		}
		if v, _ := cmd.Flags().GetString("state-event"); v != "" {
			params["state_event"] = v
		}
		// --unassign clears all assignees. GitLab's issues API treats an empty
		// assignee_ids value as "unassign all" (documented behavior). Applied
		// before --assignee so an explicit --assignee always wins if both are given.
		if unassign, _ := cmd.Flags().GetBool("unassign"); unassign {
			params["assignee_ids"] = ""
		}
		if v, _ := cmd.Flags().GetString("assignee"); v != "" {
			user, err := pr.ResolveUser(context.Background(), v)
			if err != nil {
				return fmt.Errorf("resolve assignee %q: %w", v, err)
			}
			if user.ID != 0 {
				params["assignee_ids"] = fmt.Sprintf("%d", user.ID)
			}
		}
		if err := pr.UpdateIssue(context.Background(), pressoir.RepoRefFromPath(repo), number, params); err != nil {
			return err
		}
		// Re-fetch and print the updated issue.
		issue, err := pr.GetIssue(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		return printJSON(issue)
	},
}

// aoc pressoir open-pr --repo <r> --src <branch> --dst <branch> --title <t> [--body <b>] [--draft]
var pressoirOpenPRCmd = &cobra.Command{
	Use:   "open-pr",
	Short: "Open a PR/MR and print it as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		src, _ := cmd.Flags().GetString("src")
		dst, _ := cmd.Flags().GetString("dst")
		title, _ := cmd.Flags().GetString("title")
		if repo == "" || src == "" || dst == "" || title == "" {
			return fmt.Errorf("--repo, --src, --dst, and --title are required")
		}
		if err := validateMRTitle(title); err != nil {
			return err
		}
		body, _ := cmd.Flags().GetString("body")
		draft, _ := cmd.Flags().GetBool("draft")
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		result, err := pr.OpenPR(context.Background(), pressoir.RepoRefFromPath(repo), src, dst, title, body, draft)
		if err != nil {
			return err
		}
		return printJSON(result)
	},
}

// aoc pressoir get-repo --repo <r>
var pressoirGetRepoCmd = &cobra.Command{
	Use:   "get-repo",
	Short: "Fetch repository metadata (default_branch, …) as JSON",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		if repo == "" {
			return fmt.Errorf("--repo is required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		defaultBranch, err := pr.GetDefaultBranch(context.Background(), pressoir.RepoRefFromPath(repo))
		if err != nil {
			return err
		}
		return printJSON(map[string]string{"default_branch": defaultBranch})
	},
}

// aoc pressoir link-issues --repo <r> --number <n> --target-repo <r2> --target-number <n2> [--link-type blocks]
var pressoirLinkIssuesCmd = &cobra.Command{
	Use:   "link-issues",
	Short: "Create a link between two issues (blocks, is_blocked_by, relates_to)",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		targetRepo, _ := cmd.Flags().GetString("target-repo")
		targetNumber, _ := cmd.Flags().GetInt("target-number")
		if repo == "" || number == 0 || targetNumber == 0 {
			return fmt.Errorf("--repo, --number, and --target-number are required")
		}
		if targetRepo == "" {
			targetRepo = repo
		}
		linkType, _ := cmd.Flags().GetString("link-type")
		if linkType == "" {
			linkType = "blocks"
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		if err := pr.LinkIssues(context.Background(), pressoir.RepoRefFromPath(repo), number, pressoir.RepoRefFromPath(targetRepo), targetNumber, linkType); err != nil {
			return err
		}
		fmt.Printf("Linked %s#%d %s %s#%d\n", repo, number, linkType, targetRepo, targetNumber)
		return nil
	},
}

// newPressoirForRepoWithOwnerToken is like newPressoirForRepo but substitutes the
// PINARD_OWNER_GITLAB_TOKEN (operator identity) for the bot token. Used for
// operations that must be attributed to the owner account (e.g. MR approval —
// the owner is a different identity than the bot that authored the MR, so
// GitLab's self-approval restriction does not apply).
func newPressoirForRepoWithOwnerToken(creds *config.Credentials, vb *config.Vignoble, repo string) pressoir.Pressoir {
	ownerToken := os.Getenv("PINARD_OWNER_GITLAB_TOKEN")
	if ownerToken == "" {
		// Fall back to the normal adapter when no owner token is available.
		return newPressoirForRepo(creds, vb, repo)
	}
	pCfg := vb.ResolvePressoirConfig(repo)
	// Override the token for the GitLab adapter with the owner token.
	ownerCreds := *creds
	ownerCreds.GitLab.TokenEnv = ""
	// Build directly with the token we already resolved.
	return pressoir.NewGitLabAdapter(pCfg.Host, ownerToken)
}

// aoc pressoir approve-pr --repo <r> --number <n>
var pressoirApprovePRCmd = &cobra.Command{
	Use:   "approve-pr",
	Short: "Approve a PR/MR using the owner identity (PINARD_OWNER_GITLAB_TOKEN)",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepoWithOwnerToken(creds, vb, repo)
		if err := pr.Approve(context.Background(), pressoir.RepoRefFromPath(repo), number); err != nil {
			return err
		}
		fmt.Printf("Approved MR !%d on %s\n", number, repo)
		return nil
	},
}

// aoc pressoir get-pr-changes --repo <r> --number <n>
var pressoirGetPRChangesCmd = &cobra.Command{
	Use:   "get-pr-changes",
	Short: "List the files changed in a PR/MR as a JSON array of paths",
	RunE: func(cmd *cobra.Command, args []string) error {
		repo, _ := cmd.Flags().GetString("repo")
		number, _ := cmd.Flags().GetInt("number")
		if repo == "" || number == 0 {
			return fmt.Errorf("--repo and --number are required")
		}
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pr := newPressoirForRepo(creds, vb, repo)
		changes, err := pr.GetPRChanges(context.Background(), pressoir.RepoRefFromPath(repo), number)
		if err != nil {
			return err
		}
		fmt.Println(strings.Join(changes, "\n"))
		return nil
	},
}

// printJSON marshals v to indented JSON and writes it to stdout.
func printJSON(v any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	fmt.Println(string(out))
	return nil
}

func init() {
	// get-issue
	pressoirGetIssueCmd.Flags().String("repo", "", "Repository path (e.g. group/project)")
	pressoirGetIssueCmd.Flags().Int("number", 0, "Issue number")
	pressoirCmd.AddCommand(pressoirGetIssueCmd)

	// list-pr-notes
	pressoirListPRNotesCmd.Flags().String("repo", "", "Repository path")
	pressoirListPRNotesCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirCmd.AddCommand(pressoirListPRNotesCmd)

	// resolve-user
	pressoirResolveUserCmd.Flags().String("username", "", "Username to resolve")
	pressoirResolveUserCmd.Flags().String("repo", "", "Repository path to determine the provider (optional; defaults to vignoble default)")
	pressoirCmd.AddCommand(pressoirResolveUserCmd)

	// comment-pr
	pressoirCommentPRCmd.Flags().String("repo", "", "Repository path")
	pressoirCommentPRCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirCommentPRCmd.Flags().String("body", "", "Comment body")
	pressoirCmd.AddCommand(pressoirCommentPRCmd)

	// comment-issue
	pressoirCommentIssueCmd.Flags().String("repo", "", "Repository path")
	pressoirCommentIssueCmd.Flags().Int("number", 0, "Issue number")
	pressoirCommentIssueCmd.Flags().String("body", "", "Comment body")
	pressoirCmd.AddCommand(pressoirCommentIssueCmd)

	// get-pr
	pressoirGetPRCmd.Flags().String("repo", "", "Repository path")
	pressoirGetPRCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirCmd.AddCommand(pressoirGetPRCmd)

	// track-pr
	pressoirTrackPRCmd.Flags().String("repo", "", "Repository path")
	pressoirTrackPRCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirTrackPRCmd.Flags().String("session", "", "Worker session name")
	pressoirTrackPRCmd.Flags().String("project", "", "Project name (optional)")
	pressoirCmd.AddCommand(pressoirTrackPRCmd)

	// list-issue-notes
	pressoirListIssueNotesCmd.Flags().String("repo", "", "Repository path")
	pressoirListIssueNotesCmd.Flags().Int("number", 0, "Issue number")
	pressoirCmd.AddCommand(pressoirListIssueNotesCmd)

	// update-issue
	pressoirUpdateIssueCmd.Flags().String("repo", "", "Repository path")
	pressoirUpdateIssueCmd.Flags().Int("number", 0, "Issue number")
	pressoirUpdateIssueCmd.Flags().String("labels", "", "Comma-separated labels (replaces existing)")
	pressoirUpdateIssueCmd.Flags().String("add-labels", "", "Comma-separated labels to add")
	pressoirUpdateIssueCmd.Flags().String("remove-labels", "", "Comma-separated labels to remove")
	pressoirUpdateIssueCmd.Flags().String("state-event", "", "State transition: 'close' or 'reopen'")
	pressoirUpdateIssueCmd.Flags().String("assignee", "", "Username to assign")
	pressoirUpdateIssueCmd.Flags().Bool("unassign", false, "Clear all assignees")
	pressoirCmd.AddCommand(pressoirUpdateIssueCmd)

	// open-pr
	pressoirOpenPRCmd.Flags().String("repo", "", "Repository path")
	pressoirOpenPRCmd.Flags().String("src", "", "Source branch")
	pressoirOpenPRCmd.Flags().String("dst", "", "Target branch")
	pressoirOpenPRCmd.Flags().String("title", "", "PR title")
	pressoirOpenPRCmd.Flags().String("body", "", "PR description")
	pressoirOpenPRCmd.Flags().Bool("draft", false, "Open as draft")
	pressoirCmd.AddCommand(pressoirOpenPRCmd)

	// get-repo
	pressoirGetRepoCmd.Flags().String("repo", "", "Repository path")
	pressoirCmd.AddCommand(pressoirGetRepoCmd)

	// link-issues
	pressoirLinkIssuesCmd.Flags().String("repo", "", "Source repository path")
	pressoirLinkIssuesCmd.Flags().Int("number", 0, "Source issue number")
	pressoirLinkIssuesCmd.Flags().String("target-repo", "", "Target repository path (defaults to source repo)")
	pressoirLinkIssuesCmd.Flags().Int("target-number", 0, "Target issue number")
	pressoirLinkIssuesCmd.Flags().String("link-type", "blocks", "Link type: blocks, is_blocked_by, relates_to")
	pressoirCmd.AddCommand(pressoirLinkIssuesCmd)

	// approve-pr
	pressoirApprovePRCmd.Flags().String("repo", "", "Repository path")
	pressoirApprovePRCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirCmd.AddCommand(pressoirApprovePRCmd)

	// get-pr-changes
	pressoirGetPRChangesCmd.Flags().String("repo", "", "Repository path")
	pressoirGetPRChangesCmd.Flags().Int("number", 0, "PR/MR number")
	pressoirCmd.AddCommand(pressoirGetPRChangesCmd)

	rootCmd.AddCommand(pressoirCmd)
}
