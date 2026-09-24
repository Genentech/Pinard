package main

import (
	"context"
	"fmt"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/watcher"
	"github.com/spf13/cobra"
)

// appendConductorMarker appends the conductor marker to a comment body so the
// mr-watcher forwards it to the vendangeur despite the shared git-host
// identity (see watcher.ConductorMarker).
func appendConductorMarker(body string) string {
	return fmt.Sprintf("%s\n\n%s", body, watcher.ConductorMarker)
}

// aoc comment-mr --project <p> --mr <n> --body <b>
//
// This is a pinard-policy wrapper around the provider-neutral
// `aoc pressoir comment-pr`: it appends the conductor marker so the
// mr-watcher recognizes the note as conductor-directed feedback and forwards
// it to the vendangeur, instead of filtering it out as self-authored.
// `pressoir comment-pr` itself stays marker-free and provider-neutral.
var commentMRCmd = &cobra.Command{
	Use:   "comment-mr",
	Short: "Post a conductor-marked comment on a MR (forwarded to the vendangeur)",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		repo, _ := cmd.Flags().GetString("repo")
		mr, _ := cmd.Flags().GetInt("mr")
		body, _ := cmd.Flags().GetString("body")
		if (project == "" && repo == "") || mr == 0 || body == "" {
			return fmt.Errorf("--mr and --body are required, and one of --project or --repo")
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
		if err := pr.Comment(context.Background(), pressoir.RepoRefFromPath(repo), mr, appendConductorMarker(body)); err != nil {
			return err
		}
		fmt.Printf("Commented on MR !%d (%s) — the vendangeur will receive it as review feedback.\n", mr, repo)
		return nil
	},
}

func init() {
	commentMRCmd.Flags().String("project", "", "Vigne/project name from vignes.yaml")
	commentMRCmd.Flags().String("repo", "", "Repository path (overrides --project lookup)")
	commentMRCmd.Flags().Int("mr", 0, "Merge request IID (number)")
	commentMRCmd.Flags().String("body", "", "Comment / instruction for the vendangeur")
	rootCmd.AddCommand(commentMRCmd)
}
