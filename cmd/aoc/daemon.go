package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/logrotate"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/session"
	"github.com/Genentech/pinard/internal/state"
	"github.com/Genentech/pinard/internal/watcher"
	"github.com/Genentech/pinard/internal/webterm"
	"github.com/Genentech/pinard/internal/wiki"
	"github.com/spf13/cobra"
)

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Run all watchers continuously (MRs, issues, schedules)",
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, vb, gl, nc := mustLoadAll()
		defer nc.Close()

		if err := os.MkdirAll(vb.LogDir, 0755); err != nil {
			log.Printf("[daemon] mkdir %s failed (non-fatal): %v", vb.LogDir, err)
		}
		logCfg := logrotate.DefaultConfig()
		logWriter := logrotate.NewWriter(filepath.Join(vb.LogDir, "aoc-daemon.log"), logCfg)
		reclaimed, reclaimErr := logrotate.ReclaimIfOversized(logWriter, logCfg)
		log.SetOutput(logWriter)
		if reclaimErr != nil {
			log.Printf("[daemon] reclaim aoc-daemon.log failed (non-fatal): %v", reclaimErr)
		} else if reclaimed > 0 {
			log.Printf("[daemon] reclaimed %d bytes: rotated+archived oversized aoc-daemon.log", reclaimed)
		}

		pCfg := vb.ResolvePressoirConfig("")
		pr, err := pressoir.NewPressoir(pCfg, creds)
		if err != nil {
			log.Printf("[daemon] pressoir init failed: %v — falling back to GitLab adapter", err)
			pr = pressoir.NewGitLabAdapter(creds.GitLab.Host, creds.Token())
		}
		// pressoirResolver selects the correct adapter per repo so that a vigne
		// with pressoir: github is not polled via the vignoble-default GitLab adapter.
		pressoirResolver := &watcher.PressoirResolver{
			Vignoble: vb,
			Creds:    creds,
			Fallback: pr,
		}

		kv := pnats.NewKV(nc)

		if err := kv.EnsureBucket(pnats.KVNameMaitreStatus); err != nil {
			log.Printf("[daemon] EnsureBucket %s failed (non-fatal): %v", pnats.KVNameMaitreStatus, err)
		}

		mrState, err := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
		if err != nil {
			return err
		}
		issueState, err := state.Load[state.IssueWatcherState](filepath.Join(vb.StateDir, "issue-watcher.yaml"))
		if err != nil {
			return err
		}
		runs, err := state.Load[state.SchedulerRuns](filepath.Join(vb.StateDir, "scheduler-runs.yaml"))
		if err != nil {
			return err
		}

		schedCfg, _ := config.LoadSchedules(filepath.Join(vb.Path, "schedules.yaml"))

		sm := session.New()
		defer sm.Close()

		mrWatcher := &watcher.MRWatcher{
			State:          mrState,
			IssueState:     issueState,
			NATS:           nc,
			KV:             kv,
			Pressoir:       pr,
			Resolver:       pressoirResolver,
			GitLab:         gl,
			Vignoble:       vb,
			IgnoredAuthors: map[string]bool{creds.GitLab.User: true},
			Session:        sm,
			User:           creds.GitLab.User,
		}

		capsulePoller := &watcher.CapsulePoller{
			State:    issueState,
			GitLab:   gl,
			Vignoble: vb,
			User:     creds.GitLab.User,
			Owner:    creds.WebtermOwner(),
		}

		issueWatcher := &watcher.IssueWatcher{
			State:         issueState,
			NATS:          nc,
			KV:            kv,
			Pressoir:      pr,
			Resolver:      pressoirResolver,
			GitLab:        gl,
			Vignoble:      vb,
			Creds:         creds,
			User:          creds.GitLab.User,
			Owner:         creds.WebtermOwner(),
			CapsulePoller: capsulePoller,
			Session:       sm,
			MRState:       mrState,
		}
		// Wire the back-pointer so CapsulePoller routes funded spawns through
		// the owner gate (spawnIfApproved) — externally-funded work still needs
		// the trust anchor.
		capsulePoller.SetIssueWatcher(issueWatcher)

		scheduler := &watcher.Scheduler{
			Runs:      runs,
			NATS:      nc,
			KV:        kv,
			Vignoble:  vb,
			AOCBin:    mustFindAOC(),
			Schedules: schedCfg,
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

		// Own the PID file: write on startup, remove on clean shutdown. A
		// self-reload re-execs in-place (same PID) and does not run defers, so
		// the file stays valid across reloads.
		pidFile := daemonPIDFile(vb)
		os.MkdirAll(vb.StateDir, 0755)
		os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)

		session.EnsureWorkspaces([]string{vb.Name})

		// Engram serve: the daemon owns the single per-vignoble `engram serve` so
		// agents (régisseur, maîtres, vendangeurs) can attach via ENGRAM_URL rather
		// than each racing to spawn their own serve with a worktree cwd.
		engramServer := &watcher.EngramServer{
			VignobleName: vb.Name,
			VignoblePath: vb.Path,
		}
		engramBin := ""
		if err := engramServer.Start(); err != nil {
			log.Printf("[engram-serve] disabled: %v", err)
		} else {
			engramBin = engramServer.Bin()
			go engramServer.Supervise(ctx)
		}

		if err := wiki.EnsureBundle(vb.Path); err != nil {
			log.Printf("[wiki] EnsureBundle failed: %v", err)
		}

		log.Printf("[daemon] Started for vignoble %s (NATS: %s)", vb.Name, creds.NATSUrl())

		// Config/binary hot-reload: poll mtimes and re-exec ourselves when the
		// aoc binary or this vignoble's config changes. Replaces the systemd
		// path watchers that used to restart the service.
		reloadTargets := []string{
			selfPath(),
			vb.ConfigPath,
			filepath.Join(vb.Path, "schedules.yaml"),
		}
		if engramBin != "" {
			reloadTargets = append(reloadTargets, engramBin)
		}
		baseline := map[string]time.Time{}
		for _, p := range reloadTargets {
			baseline[p] = fileMTime(p)
		}
		go tick(ctx, "reloader", 3*time.Second, func() {
			for _, p := range reloadTargets {
				mt := fileMTime(p)
				if mt.IsZero() {
					continue
				}
				if prev, ok := baseline[p]; ok && !mt.Equal(prev) {
					log.Printf("[reloader] %s changed — restarting daemon", p)
					// Cycle engram when a binary (aoc or engram itself) changed so
					// the new daemon image launches a fresh serve at the updated
					// version. Skip for pure config changes to avoid needless churn.
					if p == selfPath() || (engramBin != "" && p == engramBin) {
						log.Printf("[reloader] %s changed — cycling engram serve", p)
						engramServer.Stop()
					}
					// Re-exec in place: same PID, keeps stdout/stderr (log file)
					// and cwd; reloads all config exactly like a restart.
					if err := syscall.Exec(selfPath(), os.Args, os.Environ()); err != nil {
						log.Printf("[reloader] re-exec failed: %v", err)
						baseline[p] = mt // avoid hot-looping on a broken exec
					}
					return
				}
			}
		})

		// GC: reap finished workers automatically instead of relying on a human
		// to run `aoc gc`. Startup sweep clears anything left by a previous
		// run/crash; the event-driven watcher reaps ephemeral/scheduled workers
		// shortly after their turn ends; the daily backstop catches what the
		// other two miss (orphaned sockets, archived-parcelle maître zombies,
		// completed --process workers whose reapWorker teardown was interrupted).
		startupGC := runGCSweep(kv, vb, gcSweepOptions{VignobleFilter: vb.Name, Grace: defaultGCGrace})
		log.Printf("[gc] startup sweep: %d reaped, %d sockets removed", len(startupGC.reaped), len(startupGC.sockets))

		gcWatcher := newBlockedGraceWatcher(kv, vb, mrState, defaultGCGrace)
		go gcWatcher.Run(ctx)

		go runGCBackstopLoop(ctx, kv, vb)

		// Error log: durable, append-only record of abnormal turn ends (#337) —
		// the pinard-agents KV's lastError/erroredAt are cleared optimistically
		// by the worker on the next turn_start, so this watcher is the only
		// surviving trace once that happens.
		errWatcher := newErrorLogWatcher(kv, vb)
		go errWatcher.Run(ctx)

		// Run tickers
		go tick(ctx, "mr-watcher", 30*time.Second, func() { mrWatcher.Run() })
		go tick(ctx, "issue-watcher", 60*time.Second, func() { issueWatcher.Run() })
		go tick(ctx, "capsule-poller", watcher.CapsulePollInterval(), func() { capsulePoller.Run() })
		go tick(ctx, "scheduler", 60*time.Second, func() { scheduler.Run() })

		// KV compaction: purge delete markers from pinard-agents so tombstones
		// from kv.Delete calls don't accumulate in the JetStream stream unbounded.
		// Removes markers older than 1h; live values are never affected.
		go tick(ctx, "kv-compactor", 30*time.Minute, func() {
			before, _ := kv.NumSubjects("pinard-agents")
			if err := kv.PurgeDeletes("pinard-agents", time.Hour); err != nil {
				log.Printf("[kv-compactor] PurgeDeletes pinard-agents: %v", err)
				return
			}
			after, _ := kv.NumSubjects("pinard-agents")
			log.Printf("[kv-compactor] pinard-agents: subjects %d\u2192%d (purged %d markers)", before, after, before-after)
		})

		// Remote mirror: maintain local tmux mirror sessions for remote agents so
		// they appear in `tmux ls` without manual `aoc attach` per agent.
		remoteMirror := &watcher.RemoteMirrorWatcher{
			Vignoble: vb,
			KV:       kv,
			AOCBin:   mustFindAOC(),
		}
		go tick(ctx, "remote-mirror", 30*time.Second, func() { remoteMirror.Run() })

		// Orphan run recovery: scan parcelles for incomplete runs without active workers
		orphanRecovery := &watcher.OrphanRecovery{
			Vignoble:     vb,
			KV:           kv,
			NATS:         nc,
			Session:      sm,
			MRState:      mrState,
			Pressoir:     pr,
			Resolver:     pressoirResolver,
			GitLab:       gl,
			GitLabIssues: gl,
		}
		go tick(ctx, "orphan-recovery", 120*time.Second, func() { orphanRecovery.Run() })

		// Engram cloud sync: replicate this vignoble's local memory store to the
		// central backend. Always-on here (was a per-session bash loop in bin/pinard);
		// per-vignoble (DataDir = <vignoble>/.engram, project = vignoble name). Gated
		// on an engram binary + a cloud token+server; otherwise a no-op (local only).
		engramSync := &watcher.EngramSyncer{
			Server:   creds.EngramServer(),
			Token:    creds.EngramCloudToken(),
			Project:  vb.Name,
			DataDir:  filepath.Join(vb.Path, ".engram"),
			KV:       kv,
			Vignoble: vb.Name,
		}
		if engramSync.Enabled() {
			engramSync.Setup() // config + enroll + initial drain (once)
			go tick(ctx, "engram-sync", engramSync.Interval(), func() { engramSync.Run() })
		} else {
			log.Printf("[engram-sync] disabled (no engram binary or cloud token/server) — memory stays local")
		}

		// Web-terminal responder: streams local tmux targets to the gateway over
		// NATS (read-only). Only runs when a grant secret is configured.
		if creds.WebtermResponderEnabled() {
			// Publish this vignoble's owner for gateway operator authorization (D7).
			if err := webterm.PublishOwner(kv, vb.Name, creds.WebtermOwner()); err != nil {
				log.Printf("[webterm] publish owner failed: %v", err)
			}
			resp := &webterm.Responder{
				NC:          nc.Conn(),
				Vignoble:    vb.Name,
				GrantSecret: creds.WebtermGrantSecret(),
				MaxViewers:  creds.WebtermMaxViewers(),
				IdleTimeout: creds.WebtermIdleTimeout(),
				KV:          kv,
			}
			go func() {
				if err := resp.Run(ctx); err != nil && ctx.Err() == nil {
					log.Printf("[webterm] responder exited: %v", err)
				}
			}()
		}

		<-sig
		log.Printf("[daemon] Shutting down...")
		cancel()
		engramServer.Stop()
		os.Remove(pidFile)
		return nil
	},
}

// fileMTime returns the modification time of path, or the zero time if it
// cannot be stat'd (e.g. the file does not exist yet).
func fileMTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func tick(ctx context.Context, name string, interval time.Duration, fn func()) {
	safeFn := func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] PANIC recovered: %v", name, r)
			}
		}()

		// Timeout: don't let a stuck API call block the ticker forever
		done := make(chan struct{})
		go func() {
			fn()
			close(done)
		}()

		select {
		case <-done:
			// completed normally
		case <-time.After(2 * time.Minute):
			log.Printf("[%s] TIMEOUT: tick took >2min, skipping", name)
		case <-ctx.Done():
		}
	}

	// Run immediately on start
	safeFn()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safeFn()
		}
	}
}

func init() {
	rootCmd.AddCommand(daemonCmd)
}
