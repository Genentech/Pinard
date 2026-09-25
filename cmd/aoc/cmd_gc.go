package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/git"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/session"
	"github.com/Genentech/pinard/internal/state"
	"github.com/spf13/cobra"
)

// abandonedTTL is the lastSeen age beyond which a worker with no local tmux
// session is considered truly abandoned (process dead, no heartbeat).
const abandonedTTL = 4 * time.Hour

// defaultGCGrace is the idle grace period before a finished ephemeral/scheduled
// worker is reaped — used both as the CLI --older-than default and as the
// daemon's event-driven/startup grace.
const defaultGCGrace = 10 * time.Minute

// gcBackstopInterval is how often the daemon's low-frequency backstop sweep runs.
const gcBackstopInterval = 24 * time.Hour

// scheduleNameRe matches the scheduled-worker name pattern:
// <vignoble>-<schedule-name>-<period-suffix> where the period suffix is digits/letters.
// We accept any trailing alphanumeric+dash segment (PeriodSuffix produces yyyymmddhh etc.).
var scheduleNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+-[a-zA-Z0-9_-]+-\d{6,}$`)

// webTermSocketRe matches socket names created by webterm tests / go test runs.
var webTermSocketRe = regexp.MustCompile(`^(webterm-cr-|webterm-test-)`)

type gcResult struct {
	reaped  []string
	sockets []string
}

// gcSweepOptions configures a single GC sweep. Shared by the manual `aoc gc`
// command and the daemon (startup sweep, daily backstop) so there is exactly
// one code path for worker/socket collection.
type gcSweepOptions struct {
	VignobleFilter string
	Grace          time.Duration
	DryRun         bool
	NoWorkers      bool
	NoSockets      bool
	// Backstop enables the low-frequency-only checks (completed --process
	// workers with a merged MR, archived-parcelle maître zombies) that are not
	// safe/desirable to run on every manual `aoc gc` invocation or on every
	// event-driven reap.
	Backstop bool
}

// runGCSweep performs one full GC pass — worker reaping, socket sweeping, and
// (when requested) the backstop-only checks — and returns what was collected.
// It does not print anything; callers decide how to surface the result (the
// CLI prints a human summary, the daemon logs a one-line summary).
func runGCSweep(kv *pnats.KV, vb *config.Vignoble, opts gcSweepOptions) *gcResult {
	result := &gcResult{}

	if !opts.NoWorkers {
		if err := gcWorkers(kv, vb, opts.VignobleFilter, opts.Grace, opts.DryRun, result); err != nil {
			log.Printf("[gc] worker sweep error: %v", err)
		}
		if opts.Backstop {
			gcBackstopWorkers(kv, vb, opts.VignobleFilter, opts.DryRun, result)
		}
	}

	if !opts.NoSockets {
		gcSockets(opts.DryRun, result)
	}

	if opts.Backstop {
		gcArchivedMaitreZombies(vb, opts.DryRun, result)
	}

	return result
}

// printGCSummary prints the human-readable summary block for the manual `aoc
// gc` command.
func printGCSummary(result *gcResult, dryRun bool) {
	fmt.Printf("\n--- GC summary ---\n")
	if len(result.reaped) == 0 && len(result.sockets) == 0 {
		fmt.Println("Nothing to collect.")
		return
	}
	if len(result.reaped) > 0 {
		label := "Reaped"
		if dryRun {
			label = "Would reap"
		}
		fmt.Printf("%s %d worker(s):\n", label, len(result.reaped))
		for _, r := range result.reaped {
			fmt.Printf("  - %s\n", r)
		}
	}
	if len(result.sockets) > 0 {
		label := "Removed"
		if dryRun {
			label = "Would remove"
		}
		fmt.Printf("%s %d socket(s):\n", label, len(result.sockets))
		for _, s := range result.sockets {
			fmt.Printf("  - %s\n", s)
		}
	}
}

var gcCmd = &cobra.Command{
	Use:   "gc",
	Short: "Garbage-collect finished workers and orphaned tmux sockets",
	Long: `Reap workers that are done (ephemeral/scheduled with idle tempo, completed
process workers, truly abandoned) and sweep orphaned tmux socket files.

Safe by default: never touches conductor/maitre/regisseur, open-MR workers,
active-turn workers, or fresh remote/standalone workers.

Use --dry-run to preview what would be collected without making any changes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		olderThan, _ := cmd.Flags().GetDuration("older-than")
		noSockets, _ := cmd.Flags().GetBool("no-sockets")
		noWorkers, _ := cmd.Flags().GetBool("no-workers")
		vignobleFlag, _ := cmd.Flags().GetString("vignoble")
		allFlag, _ := cmd.Flags().GetBool("all")

		if dryRun {
			fmt.Println("=== DRY RUN — no changes will be made ===")
		}

		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}

		// Determine which vignoble(s) to scope to.
		// --vignoble <name>: explicit scope; --all: no filter (all in KV bucket);
		// default: the current vignoble.
		var vignobleFilter string
		if vignobleFlag != "" {
			vignobleFilter = vignobleFlag
		} else if !allFlag {
			vignobleFilter = vb.Name
		}

		nc := pnats.NewClient(creds)
		if err := nc.Connect(); err != nil {
			return fmt.Errorf("NATS connect: %w", err)
		}
		defer nc.Close()
		kv := pnats.NewKV(nc)

		result := runGCSweep(kv, vb, gcSweepOptions{
			VignobleFilter: vignobleFilter,
			Grace:          olderThan,
			DryRun:         dryRun,
			NoWorkers:      noWorkers,
			NoSockets:      noSockets,
		})

		printGCSummary(result, dryRun)
		return nil
	},
}

func gcWorkers(kv pnats.KVWriter, vb *config.Vignoble, vignobleFilter string, grace time.Duration, dryRun bool, result *gcResult) error {
	keys, err := kv.Keys("pinard-agents")
	if err != nil {
		return fmt.Errorf("listing pinard-agents: %w", err)
	}

	// Load MR watcher state for the open-MR guard (read-only).
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))

	now := time.Now()

	for _, key := range keys {
		data, err := kv.Get("pinard-agents", key)
		if err != nil || data == nil {
			continue
		}

		agentVignoble, _ := data["vignoble"].(string)
		if vignobleFilter != "" && agentVignoble != "" && agentVignoble != vignobleFilter {
			continue
		}
		// Use the current vignoble for ops; if it belongs to another vignoble, skip
		// operations that need a path (worktrees, run dirs).
		effectiveVignoble := agentVignoble
		if effectiveVignoble == "" {
			effectiveVignoble = vb.Name
		}

		name, _ := data["name"].(string)
		if name == "" {
			name = key
		}
		tempo, _ := data["tempo"].(string)
		ephemeral, _ := data["ephemeral"].(bool)
		process, _ := data["process"].(string)
		standalone, _ := data["standalone"].(bool)
		lastSeenStr, _ := data["lastSeen"].(string)
		project, _ := data["project"].(string)
		state_, _ := data["state"].(string)

		// Guard: never reap conductor session or reserved windows.
		if name == "conductor" || session.IsReservedWindow(name) {
			continue
		}
		// Guard: skip active-turn workers.
		if tempo == "active" {
			continue
		}
		// Guard: skip workers with an open, unmerged MR.
		if hasOpenMR(mrState, key, name) {
			continue
		}

		// Parse lastSeen for idle calculations.
		var lastSeen time.Time
		if lastSeenStr != "" {
			if t, err := time.Parse(time.RFC3339, lastSeenStr); err == nil {
				lastSeen = t
			}
		}
		idleTime := time.Duration(0)
		if !lastSeen.IsZero() {
			idleTime = now.Sub(lastSeen)
		}

		// Guard: fresh remote/standalone workers — never reap a standalone worker
		// that heartbeated recently (within abandonedTTL). They have no local tmux.
		if standalone {
			if lastSeen.IsZero() || idleTime < abandonedTTL {
				continue
			}
		}

		shouldReap := false
		reason := ""

		// Criterion 1: ephemeral/scheduled with idle tempo and idle > grace.
		isScheduled := ephemeral || scheduleNameRe.MatchString(name)
		if isScheduled && tempo != "active" && idleTime >= grace {
			shouldReap = true
			reason = fmt.Sprintf("ephemeral/scheduled (tempo=%s, idle=%s)", tempo, idleTime.Round(time.Second))
		}

		// Criterion 2: truly abandoned — no local tmux session, heartbeat stale past TTL.
		if !shouldReap && !standalone && !isScheduled && process == "" {
			alive := tmuxSessionAlive(effectiveVignoble, name)
			if !alive && (lastSeen.IsZero() || idleTime > abandonedTTL) {
				shouldReap = true
				reason = fmt.Sprintf("abandoned (no tmux session, last seen %s)", lastSeenStr)
			}
		}

		// Criterion 3: stopped/done state marker.
		if !shouldReap && (state_ == "stopped" || state_ == "done") && idleTime >= grace {
			shouldReap = true
			reason = fmt.Sprintf("state=%s, idle=%s", state_, idleTime.Round(time.Second))
		}

		if !shouldReap {
			continue
		}

		reapAgentNow(kv, vb, key, name, effectiveVignoble, project, reason, dryRun, result)
	}
	return nil
}

// reapAgentNow is the single teardown action shared by every GC path (manual
// sweep, event-driven grace timer, backstop): record the reap, kill the tmux
// session, delete the KV entry, and remove the worker's git worktree.
func reapAgentNow(kv pnats.KVWriter, vb *config.Vignoble, key, name, effectiveVignoble, project, reason string, dryRun bool, result *gcResult) {
	label := fmt.Sprintf("%s (%s)", key, reason)
	fmt.Printf("[gc] %s %s\n", actionVerb("Reaping", dryRun), label)
	result.reaped = append(result.reaped, label)

	if dryRun {
		return
	}

	// Kill tmux session.
	socket := "pinard-" + effectiveVignoble
	exec.Command("tmux", "-L", socket, "kill-session", "-t", name).Run()

	// Delete KV entry.
	if err := kv.Del("pinard-agents", key); err != nil {
		log.Printf("[gc] failed to delete KV entry %q: %v", key, err)
	}

	// Remove worktree + prune (best-effort).
	reapWorktree(vb, project, name)
}

// hasOpenMR checks whether the worker (by key or by name) has an open, unmerged
// MR in the watcher state. Returns false when the MR state cannot be determined.
// Thin wrapper over state.OpenMR (shared with internal/watcher's respawn guard).
func hasOpenMR(mrState *state.Store[state.MRWatcherState], key, name string) bool {
	_, open := state.OpenMR(mrState, key, name)
	return open
}

// mrIsDone reports whether the worker's tracked MR reached the terminal
// post_merge state — i.e. it finished, as opposed to hasOpenMR's "still in
// flight" check. Returns false when there is no MR record at all (can't tell).
func mrIsDone(mrState *state.Store[state.MRWatcherState], key, name string) bool {
	if mrState == nil {
		return false
	}
	var done bool
	mrState.Read(func(s *state.MRWatcherState) {
		for k, entry := range s.Watched {
			if k != key && entry.Name != name {
				continue
			}
			if entry.MR > 0 && entry.State == "post_merge" {
				done = true
			}
		}
	})
	return done
}

// gcBackstopWorkers extends the regular worker sweep with a check that is only
// safe as a low-frequency safety net: --process workers whose tracked MR
// reached post_merge (fully done) but whose KV entry/session survived —
// normally reaped by mrs.go's reapWorker, this catches cases where that path
// was interrupted (crash, restart mid-teardown, etc).
func gcBackstopWorkers(kv pnats.KVWriter, vb *config.Vignoble, vignobleFilter string, dryRun bool, result *gcResult) {
	keys, err := kv.Keys("pinard-agents")
	if err != nil {
		return
	}
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
	now := time.Now()

	for _, key := range keys {
		data, err := kv.Get("pinard-agents", key)
		if err != nil || data == nil {
			continue
		}

		agentVignoble, _ := data["vignoble"].(string)
		if vignobleFilter != "" && agentVignoble != "" && agentVignoble != vignobleFilter {
			continue
		}
		effectiveVignoble := agentVignoble
		if effectiveVignoble == "" {
			effectiveVignoble = vb.Name
		}

		name, _ := data["name"].(string)
		if name == "" {
			name = key
		}
		if name == "conductor" || session.IsReservedWindow(name) {
			continue
		}

		process, _ := data["process"].(string)
		if process == "" {
			continue // the regular sweep already covers non-process workers
		}
		tempo, _ := data["tempo"].(string)
		if tempo == "active" {
			continue
		}
		if hasOpenMR(mrState, key, name) || !mrIsDone(mrState, key, name) {
			continue
		}

		lastSeenStr, _ := data["lastSeen"].(string)
		var lastSeen time.Time
		if lastSeenStr != "" {
			lastSeen, _ = time.Parse(time.RFC3339, lastSeenStr)
		}
		if !lastSeen.IsZero() && now.Sub(lastSeen) < abandonedTTL {
			continue
		}

		project, _ := data["project"].(string)
		reapAgentNow(kv, vb, key, name, effectiveVignoble, project, "process worker with completed MR (backstop)", dryRun, result)
	}
}

// gcArchivedMaitreZombies kills conductor-session maître windows whose parcelle
// has been archived (parcelle.yaml status: archived). Unlike the rest of GC,
// this is explicitly allowed to touch a maître window — but only for a
// parcelle that is no longer active.
func gcArchivedMaitreZombies(vb *config.Vignoble, dryRun bool, result *gcResult) {
	socket := "pinard-" + vb.Name
	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", "conductor", "-F", "#{window_name}").Output()
	if err != nil {
		return // no conductor session running — nothing to sweep
	}
	for _, w := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if w == "" || w == session.RegisseurWindow {
			continue
		}
		parcelleYaml := filepath.Join(vb.Path, "parcelles", w, "parcelle.yaml")
		cfg, err := config.LoadParcelleConfig(parcelleYaml)
		if err != nil || cfg.Status != "archived" {
			continue
		}

		label := fmt.Sprintf("ma\u00eetre window %q (parcelle archived)", w)
		fmt.Printf("[gc] %s %s\n", actionVerb("Killing", dryRun), label)
		result.reaped = append(result.reaped, label)
		if dryRun {
			continue
		}
		if err := exec.Command("tmux", "-L", socket, "kill-window", "-t", "conductor:"+w).Run(); err != nil {
			log.Printf("[gc] failed to kill ma\u00eetre window %q: %v", w, err)
		}
	}
}

// reapWorktree removes the git worktree for the given worker session name
// from the project's .worktrees directory, then prunes.
func reapWorktree(vb *config.Vignoble, project, sessionName string) {
	vigne, ok := vb.Config.Vignes[project]
	if !ok {
		return
	}
	projectPath := vigne.ExpandedPath()
	wtPath := filepath.Join(projectPath, ".worktrees", sessionName)
	if _, err := os.Stat(wtPath); err != nil {
		return
	}
	branch, err := worktreeBranch(wtPath)
	if err == nil && branch != "" {
		git.WorktreeRemove(projectPath, wtPath)
		git.DeleteBranch(projectPath, branch)
	}
	git.WorktreePrune(projectPath)
}

// gcSockets sweeps /tmp/tmux-<uid>/ for dead webterm test sockets and orphaned
// pinard-* sockets whose server has no live sessions.
func gcSockets(dryRun bool, result *gcResult) {
	u, err := user.Current()
	if err != nil {
		return
	}
	sockDir := filepath.Join("/tmp", fmt.Sprintf("tmux-%s", u.Uid))
	entries, err := os.ReadDir(sockDir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		socketName := e.Name()
		socketPath := filepath.Join(sockDir, socketName)

		dead := false
		if webTermSocketRe.MatchString(socketName) {
			// webterm test sockets are always reclaimable — they're test artifacts.
			// Confirm the server is actually dead before removing.
			dead = isSocketDead(socketPath)
		} else if strings.HasPrefix(socketName, "pinard-") {
			// Only remove if the tmux server is dead (no live sessions).
			dead = isSocketDead(socketPath)
		}

		if !dead {
			continue
		}

		label := socketPath
		fmt.Printf("[gc] %s socket %s\n", actionVerb("Removing", dryRun), socketPath)
		result.sockets = append(result.sockets, label)

		if !dryRun {
			if err := os.Remove(socketPath); err != nil {
				log.Printf("[gc] failed to remove socket %q: %v", socketPath, err)
			}
		}
	}
}

// isSocketDead reports whether the tmux server for the given socket path is dead
// (i.e., the socket exists but the server has no live sessions or the server is
// unreachable).
func isSocketDead(socketPath string) bool {
	err := exec.Command("tmux", "-S", socketPath, "list-sessions").Run()
	// list-sessions exits non-zero when the server is unreachable/dead.
	return err != nil
}

func actionVerb(present string, dryRun bool) string {
	if dryRun {
		return "[dry-run] would " + strings.ToLower(present)
	}
	return present
}

func init() {
	gcCmd.Flags().Bool("dry-run", false, "List what would be reaped/removed without making changes")
	gcCmd.Flags().String("vignoble", "", "Scope to a specific vignoble (default: current vignoble)")
	gcCmd.Flags().Bool("all", false, "Scope to all vignobles in the KV bucket")
	gcCmd.Flags().Duration("older-than", defaultGCGrace, "Idle grace period before a finished worker is reaped")
	gcCmd.Flags().Bool("no-sockets", false, "Skip orphaned tmux socket sweep")
	gcCmd.Flags().Bool("no-workers", false, "Skip worker reaping")
	rootCmd.AddCommand(gcCmd)
}
