package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/state"
)

func testVignoble(t *testing.T) *config.Vignoble {
	t.Helper()
	dir := t.TempDir()
	return &config.Vignoble{
		Name:     "testv",
		Path:     dir,
		StateDir: dir,
		Config:   &config.VignobleConfig{},
	}
}

// TestGCWorkers_EphemeralBlockedIdle_Reaped locks in the existing (pre-#299)
// lastSeen-based criterion for the manual `aoc gc` sweep — unchanged behavior,
// just now flowing through the shared reapAgentNow helper.
func TestGCWorkers_EphemeralBlockedIdle_Reaped(t *testing.T) {
	vb := testVignoble(t)
	kv := newMockKV()
	kv.setAgent("w1", map[string]any{
		"vignoble":  "testv",
		"name":      "w1",
		"ephemeral": true,
		"tempo":     "blocked",
		"lastSeen":  time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
	})

	result := &gcResult{}
	if err := gcWorkers(kv, vb, "testv", 10*time.Minute, false, result); err != nil {
		t.Fatalf("gcWorkers: %v", err)
	}

	if len(result.reaped) != 1 {
		t.Fatalf("expected 1 reap, got %v", result.reaped)
	}
	if data, _ := kv.Get("pinard-agents", "w1"); data != nil {
		t.Error("expected KV entry to be deleted")
	}
}

func TestGCWorkers_DryRun_LeavesKVUntouched(t *testing.T) {
	vb := testVignoble(t)
	kv := newMockKV()
	kv.setAgent("w1", map[string]any{
		"vignoble":  "testv",
		"name":      "w1",
		"ephemeral": true,
		"tempo":     "blocked",
		"lastSeen":  time.Now().Add(-20 * time.Minute).Format(time.RFC3339),
	})

	result := &gcResult{}
	if err := gcWorkers(kv, vb, "testv", 10*time.Minute, true, result); err != nil {
		t.Fatalf("gcWorkers: %v", err)
	}

	if len(result.reaped) != 1 {
		t.Fatalf("expected 1 (dry-run) reap entry, got %v", result.reaped)
	}
	if data, _ := kv.Get("pinard-agents", "w1"); data == nil {
		t.Error("dry-run must not delete the KV entry")
	}
}

func TestGCWorkers_ActiveTurnNeverReaped(t *testing.T) {
	vb := testVignoble(t)
	kv := newMockKV()
	kv.setAgent("w1", map[string]any{
		"vignoble":  "testv",
		"name":      "w1",
		"ephemeral": true,
		"tempo":     "active",
		"lastSeen":  time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
	})

	result := &gcResult{}
	if err := gcWorkers(kv, vb, "testv", 10*time.Minute, false, result); err != nil {
		t.Fatalf("gcWorkers: %v", err)
	}
	if len(result.reaped) != 0 {
		t.Fatalf("an active-turn worker must never be reaped, got %v", result.reaped)
	}
}

func TestGCBackstopWorkers_ReapsCompletedProcessWorker(t *testing.T) {
	vb := testVignoble(t)
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"pinard-swe-1": {Name: "pinard-swe-1", MR: 42, State: "post_merge"},
		}
	})

	kv := newMockKV()
	kv.setAgent("pinard-swe-1", map[string]any{
		"vignoble": "testv",
		"name":     "pinard-swe-1",
		"process":  "swe",
		"tempo":    "blocked",
		"lastSeen": time.Now().Add(-5 * time.Hour).Format(time.RFC3339),
	})

	result := &gcResult{}
	gcBackstopWorkers(kv, vb, "testv", false, result)

	if len(result.reaped) != 1 {
		t.Fatalf("expected the completed process worker to be reaped, got %v", result.reaped)
	}
	if data, _ := kv.Get("pinard-agents", "pinard-swe-1"); data != nil {
		t.Error("expected KV entry deleted")
	}
}

func TestGCBackstopWorkers_SkipsOpenMR(t *testing.T) {
	vb := testVignoble(t)
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			// State "" (not e.g. "review_pending") is deliberate: MRWatcher never
			// writes anything but "post_merge" on completion (see mrs.go), so this
			// is the shape every real open MR has. A placeholder non-empty state
			// here would mask the state.OpenMR inert-predicate bug from MR !626.
			"pinard-swe-1": {Name: "pinard-swe-1", MR: 42, State: ""},
		}
	})

	kv := newMockKV()
	kv.setAgent("pinard-swe-1", map[string]any{
		"vignoble": "testv",
		"name":     "pinard-swe-1",
		"process":  "swe",
		"tempo":    "blocked",
		"lastSeen": time.Now().Add(-5 * time.Hour).Format(time.RFC3339),
	})

	result := &gcResult{}
	gcBackstopWorkers(kv, vb, "testv", false, result)

	if len(result.reaped) != 0 {
		t.Fatalf("a process worker with an open MR must never be reaped, got %v", result.reaped)
	}
}

func TestGCBackstopWorkers_SkipsUnfinishedIdle(t *testing.T) {
	vb := testVignoble(t)
	mrState, _ := state.Load[state.MRWatcherState](filepath.Join(vb.StateDir, "mr-watcher.yaml"))
	mrState.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			"pinard-swe-1": {Name: "pinard-swe-1", MR: 42, State: "post_merge"},
		}
	})

	kv := newMockKV()
	kv.setAgent("pinard-swe-1", map[string]any{
		"vignoble": "testv",
		"name":     "pinard-swe-1",
		"process":  "swe",
		"tempo":    "blocked",
		// lastSeen recent — below abandonedTTL, backstop must not touch it yet.
		"lastSeen": time.Now().Add(-1 * time.Minute).Format(time.RFC3339),
	})

	result := &gcResult{}
	gcBackstopWorkers(kv, vb, "testv", false, result)

	if len(result.reaped) != 0 {
		t.Fatalf("a recently-seen process worker must not be reaped by the backstop, got %v", result.reaped)
	}
}

func TestGCArchivedMaitreZombies(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	vbName := fmt.Sprintf("gctest%d", time.Now().UnixNano())
	socket := "pinard-" + vbName
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })

	// Régisseur window name deliberately not exercised here — it contains
	// non-ASCII chars that a non-UTF-8 test locale can mangle in tmux; the
	// "never touch the régisseur/conductor window" guard is unit-tested via
	// session.IsReservedWindow elsewhere. Here we only need an unrelated
	// window that must survive untouched.
	if err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "conductor", "-n", "archived-p").Run(); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}
	if err := exec.Command("tmux", "-L", socket, "new-window", "-t", "conductor", "-n", "active-p").Run(); err != nil {
		t.Fatalf("tmux new-window active-p: %v", err)
	}

	writeParcelle := func(name, status string) {
		p := filepath.Join(dir, "parcelles", name)
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "parcelle.yaml"), []byte("status: "+status+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeParcelle("archived-p", "archived")
	writeParcelle("active-p", "active")

	vb := &config.Vignoble{Name: vbName, Path: dir, Config: &config.VignobleConfig{}}
	result := &gcResult{}
	gcArchivedMaitreZombies(vb, false, result)

	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", "conductor", "-F", "#{window_name}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	windows := strings.Split(strings.TrimSpace(string(out)), "\n")

	for _, w := range windows {
		if w == "archived-p" {
			t.Errorf("expected the archived parcelle's ma\u00eetre window to be killed, still present: %v", windows)
		}
	}
	var foundActive bool
	for _, w := range windows {
		if w == "active-p" {
			foundActive = true
		}
	}
	if !foundActive {
		t.Error("expected the active parcelle's window to survive")
	}
	if len(result.reaped) != 1 {
		t.Errorf("expected exactly 1 reap entry, got %v", result.reaped)
	}
}

func TestGCArchivedMaitreZombies_DryRunLeavesWindow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	dir := t.TempDir()
	vbName := fmt.Sprintf("gctestdry%d", time.Now().UnixNano())
	socket := "pinard-" + vbName
	t.Cleanup(func() { exec.Command("tmux", "-L", socket, "kill-server").Run() })

	if err := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "conductor", "-n", "archived-p").Run(); err != nil {
		t.Fatalf("tmux new-session: %v", err)
	}

	p := filepath.Join(dir, "parcelles", "archived-p")
	if err := os.MkdirAll(p, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "parcelle.yaml"), []byte("status: archived\n"), 0644); err != nil {
		t.Fatal(err)
	}

	vb := &config.Vignoble{Name: vbName, Path: dir, Config: &config.VignobleConfig{}}
	result := &gcResult{}
	gcArchivedMaitreZombies(vb, true, result)

	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", "conductor", "-F", "#{window_name}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if !strings.Contains(string(out), "archived-p") {
		t.Error("dry-run must not kill the window")
	}
	if len(result.reaped) != 1 {
		t.Errorf("dry-run should still record the would-reap entry, got %v", result.reaped)
	}
}
