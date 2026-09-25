package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strconv"

	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/session"
	"github.com/Genentech/pinard/internal/state"
	"github.com/Genentech/pinard/internal/watcher"
	"github.com/spf13/cobra"
)

var respawnCmd = &cobra.Command{
	Use:   "respawn <vigne> <issue>",
	Short: "Immediately respawn a worker on an issue, bypassing the poll cycle",
	Long: "Atomically reaps any stale worker/worktree for the issue, clears stale\n" +
		"labels (pinard:discarded, in-progress), resets the issue-watcher state,\n" +
		"and spawns right away \u2014 replacing the two-step pinard:discarded label\n" +
		"ritual with one command.\n\n" +
		"Refuses if the previous worker still has an open, unmerged MR (mirrors\n" +
		"`aoc gc`'s open-MR guard) \u2014 reaping it unconditionally would delete only\n" +
		"the local branch, leaving the remote branch and MR orphaned while a\n" +
		"second worker opens a second MR for the same issue. Pass --force to\n" +
		"explicitly abandon that MR and respawn anyway.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		vigneName := args[0]
		iid, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid issue number %q: %w", args[1], err)
		}
		force, _ := cmd.Flags().GetBool("force")

		creds, vb, gl, nc := mustLoadAll()
		defer nc.Close()

		if _, ok := vb.Config.Vignes[vigneName]; !ok {
			return fmt.Errorf("vigne %q not found in vignes.yaml", vigneName)
		}

		pCfg := vb.ResolvePressoirConfig("")
		pr, err := pressoir.NewPressoir(pCfg, creds)
		if err != nil {
			log.Printf("[respawn] pressoir init failed: %v \u2014 falling back to GitLab adapter", err)
			pr = pressoir.NewGitLabAdapter(creds.GitLab.Host, creds.Token())
		}
		pressoirResolver := &watcher.PressoirResolver{
			Vignoble: vb,
			Creds:    creds,
			Fallback: pr,
		}

		kv := pnats.NewKV(nc)

		issueState, err := state.Load[state.IssueWatcherState](filepath.Join(vb.StateDir, "issue-watcher.yaml"))
		if err != nil {
			return err
		}
		mrState, err := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
		if err != nil {
			return err
		}

		sm := session.New()
		defer sm.Close()

		iw := &watcher.IssueWatcher{
			State:    issueState,
			NATS:     nc,
			KV:       kv,
			Pressoir: pr,
			Resolver: pressoirResolver,
			GitLab:   gl,
			Vignoble: vb,
			Creds:    creds,
			User:     creds.GitLab.User,
			Owner:    creds.WebtermOwner(),
			Session:  sm,
			MRState:  mrState,
		}

		summary, err := iw.RespawnIssue(vigneName, iid, force)
		if summary != "" {
			fmt.Println(summary)
		}
		return err
	},
}

func init() {
	respawnCmd.Flags().Bool("force", false, "Respawn even if the previous worker has an open, unmerged MR (abandons it — its remote branch/MR are left as-is, orphaned)")
	rootCmd.AddCommand(respawnCmd)
}
