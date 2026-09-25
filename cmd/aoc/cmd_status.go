package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/engram"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/state"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show tracked MRs, issues, vendangeurs, and schedules",
	RunE: func(cmd *cobra.Command, args []string) error {
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		creds, _ := config.LoadCredentials()

		fmt.Printf("Vignoble: %s (%s)\n", vb.Name, creds.NATSUrl())

		// Daemon status
		pid := getDaemonPID(vb)
		if pid != "" {
			fmt.Printf("Daemon: active (pid %s)\n", pid)
		} else {
			fmt.Printf("Daemon: inactive\n")
		}

		// Tracked MRs
		mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
		fmt.Println("\nTracked MRs:")
		if len(mrState.Data.Watched) == 0 {
			fmt.Println("  (none)")
		}
		for _, entry := range mrState.Data.Watched {
			if entry.MR == 0 {
				continue
			}
			flags := []string{}
			if entry.State == "post_merge" {
				flags = append(flags, fmt.Sprintf("pipeline check %d/10", entry.PostMergeChecks))
			}
			if entry.NeedsApprovalNotified {
				flags = append(flags, "needs approval")
			}
			if entry.PipelineFailCount > 0 {
				flags = append(flags, fmt.Sprintf("CI failed x%d", entry.PipelineFailCount))
			}
			if entry.AutoMergeLabeled {
				flags = append(flags, "auto-merge")
			}
			st := entry.State
			if st == "" {
				st = "opened"
			}
			extra := ""
			if len(flags) > 0 {
				extra = "  " + strings.Join(flags, ", ")
			}
			fmt.Printf("  MR !%-4d %-18s %s%s\n", entry.MR, entry.Project, st, extra)
		}

		// Tracked Issues
		issueState, _ := state.Load[state.IssueWatcherState](filepath.Join(vb.StateDir, "issue-watcher.yaml"))
		fmt.Println("\nTracked Issues:")
		hasIssues := false
		for project, issues := range issueState.Data.Seen {
			for iid, entry := range issues {
				if entry.Status == "closed" {
					continue
				}
				hasIssues = true
				title := entry.Title
				if len(title) > 50 {
					title = title[:50] + "..."
				}
				fmt.Printf("  #%-5s %-18s %-8s %s\n", iid, project, entry.Status, title)
			}
		}
		if !hasIssues {
			fmt.Println("  (none)")
		}

		// Schedules
		schedules, _ := config.LoadSchedules(filepath.Join(vb.Path, "schedules.yaml"))
		schedRuns, _ := state.Load[state.SchedulerRuns](filepath.Join(vb.StateDir, "scheduler-runs.yaml"))
		fmt.Println("\nSchedules:")
		if len(schedules) == 0 {
			fmt.Println("  (none)")
		}
		for _, sched := range schedules {
			if !sched.Enabled {
				continue
			}
			lastRun := schedRuns.Data.Runs[sched.Name]
			if lastRun == "" {
				lastRun = "never"
			} else if t, err := time.Parse(time.RFC3339, lastRun); err == nil {
				lastRun = t.Format("2006-01-02 15:04")
			}
			fmt.Printf("  %-20s cron: %-15s last: %s\n", sched.Name, sched.Cron, lastRun)
		}

		// Workers (from NATS KV)
		fmt.Println("\n🧺 Vendangeurs:")
		var kvHandle *pnats.KV
		var ncHandle *pnats.Client
		if creds != nil {
			nc := pnats.NewClient(creds)
			ncHandle = nc
			if err := nc.Connect(); err == nil {
				kv := pnats.NewKV(nc)
				kvHandle = kv
				keys, err := kv.Keys("pinard-agents")
				if err == nil && len(keys) > 0 {
					for _, key := range keys {
						data, err := kv.Get("pinard-agents", key)
						if err != nil || data == nil {
							continue
						}
						vig, _ := data["vignoble"].(string)
						if vig != vb.Name {
							continue
						}
						project, _ := data["project"].(string)
						tempo, _ := data["tempo"].(string)
						if tempo == "" {
							tempo = "unknown"
						}
						health := pnats.DeriveAgentHealth(data, time.Now(), agentLivenessThreshold)
						warn := ""
						if health.Errored {
							warn = fmt.Sprintf("  \u26a0 errored: %s", firstLine(health.LastError))
						} else if health.Stalled {
							warn = "  \u26a0 stalled"
						}
						fmt.Printf("  %-30s %-18s %s%s\n", key, project, tempo, warn)
					}
				} else {
					fmt.Println("  (none)")
				}
			} else {
				fmt.Println("  (NATS unavailable)")
			}
		}

		// Engram sync status
		fmt.Println("\n🧠 Engram sync:")
		printEngramStatus(vb, creds, kvHandle)
		if ncHandle != nil {
			ncHandle.Close()
		}

		return nil
	},
}

func printEngramStatus(vb *config.Vignoble, creds *config.Credentials, kv *pnats.KV) {
	dbPath := filepath.Join(vb.Path, ".engram", "engram.db")
	st, err := engram.QueryStatus(dbPath)
	if err != nil {
		fmt.Printf("  %s  error reading db: %v\n", vb.Name, err)
		return
	}
	if !st.DBExists {
		fmt.Printf("  %-18s · no store\n", vb.Name)
		return
	}

	cloudConfigured := creds != nil && creds.EngramServer() != "" && creds.EngramCloudToken() != ""

	// Derive last-sync time from the engram DB (sync_state.updated_at for the
	// cloud target). This reflects real cloud-ack activity regardless of which
	// path (daemon drain or conductor autosync) performed the sync, and is not
	// subject to the daemon's tick timeouts. The KV record is still consulted
	// for the result/error verdict string.
	lastSyncStr := "never"
	if st.LastSync != nil {
		age := time.Since(*st.LastSync)
		if age < time.Minute {
			lastSyncStr = "just now"
		} else if age < time.Hour {
			lastSyncStr = fmt.Sprintf("%dm ago", int(age.Minutes()))
		} else {
			lastSyncStr = st.LastSync.Local().Format("2006-01-02 15:04")
		}
	}

	var syncResult string
	if kv != nil && cloudConfigured {
		if data, err := kv.Get("pinard-engram", vb.Name); err == nil && data != nil {
			if b, e := json.Marshal(data); e == nil {
				var rec engram.SyncRecord
				if json.Unmarshal(b, &rec) == nil {
					syncResult = rec.Result
				}
			}
		}
	}

	verdict := engramVerdict(st, cloudConfigured, syncResult)
	if cloudConfigured {
		// verdict already carries the reason phrase for degraded targets.
		fmt.Printf("  %-18s total:%-5d unacked:%-5d last-sync:%-12s %s\n",
			vb.Name, st.Total, st.UnackedMutations, lastSyncStr, verdict)
	} else {
		fmt.Printf("  %-18s total:%-5d · local-only\n",
			vb.Name, st.Total)
	}
}

func engramVerdict(st *engram.DBStatus, cloudConfigured bool, syncResult string) string {
	if !cloudConfigured {
		return "· local-only"
	}
	if syncResult == "error" {
		return "✗ sync error"
	}
	if st.IsDegraded() {
		if reason := st.ReasonPhrase(); reason != "" {
			return fmt.Sprintf("⚠ degraded: %d blocked (%s)", st.UnackedMutations, reason)
		}
		return fmt.Sprintf("⚠ degraded: %d mutations blocked", st.UnackedMutations)
	}
	if st.UnackedMutations > 0 {
		return fmt.Sprintf("⚠ %d pending push", st.UnackedMutations)
	}
	return "✓ synced"
}

func getDaemonPID(vb *config.Vignoble) string {
	pid := readPIDFile(daemonPIDFile(vb))
	if processAlive(pid) {
		return strconv.Itoa(pid)
	}
	return ""
}

// firstLine returns the first line of s, for compact single-line status display.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
