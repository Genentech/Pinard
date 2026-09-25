package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/state"
	"github.com/spf13/cobra"
)

// issueStatusCmd is a read-only helper: it prints the issue-watcher's tracked
// status for a project/issue without touching NATS or the git host, so
// spawn_agent (the conductor tool) can detect an already-`spawned` issue and
// route to `aoc respawn` instead of silently re-assigning and no-op'ing.
var issueStatusCmd = &cobra.Command{
	Use:   "issue-status",
	Short: "Print the issue-watcher's tracked status for an issue (read-only, JSON)",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		issue, _ := cmd.Flags().GetInt("issue")
		if project == "" || issue == 0 {
			return fmt.Errorf("--project and --issue are required")
		}

		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}

		issueState, err := state.Load[state.IssueWatcherState](filepath.Join(vb.StateDir, "issue-watcher.yaml"))
		if err != nil {
			return err
		}

		var status string
		issueState.Read(func(s *state.IssueWatcherState) {
			if s.Seen == nil {
				return
			}
			if proj, ok := s.Seen[project]; ok {
				if entry := proj[fmt.Sprintf("%d", issue)]; entry != nil {
					status = entry.Status
				}
			}
		})

		out, err := json.Marshal(map[string]string{"status": status})
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	},
}

func init() {
	issueStatusCmd.Flags().String("project", "", "Vigne/project name")
	issueStatusCmd.Flags().Int("issue", 0, "Issue IID")
	rootCmd.AddCommand(issueStatusCmd)
}
