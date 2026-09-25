package watcher

// respawn_test.go — tests for RespawnIssue, the atomic alternative to the
// two-step pinard:discarded label ritual (issue #336).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/gitlab"
	"github.com/Genentech/pinard/internal/state"
)

func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// makeRespawnTestRepo creates a git repo (the "project") with a worktree that
// simulates a leftover worker session, and returns (projectPath, worktreeBranch).
func makeRespawnTestRepo(t *testing.T, sessionName string) (projectPath string) {
	t.Helper()
	tmp := t.TempDir()
	projectPath = filepath.Join(tmp, "proj")
	mustRunGit(t, "", "init", projectPath)
	mustRunGit(t, projectPath, "config", "user.email", "test@example.com")
	mustRunGit(t, projectPath, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(projectPath, "README"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, projectPath, "add", "README")
	mustRunGit(t, projectPath, "commit", "-m", "init")

	wtPath := filepath.Join(projectPath, ".worktrees", sessionName)
	mustRunGit(t, projectPath, "worktree", "add", wtPath, "-b", sessionName)
	return projectPath
}

// mockSession implements session.Manager, recording StopWorker calls.
type respawnMockSession struct {
	mu      sync.Mutex
	stopped []string
}

func (m *respawnMockSession) SpawnWorker(workspace, name, command string) error { return nil }
func (m *respawnMockSession) StopWorker(workspace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, name)
	return nil
}
func (m *respawnMockSession) GetWorkerCwd(workspace, name string) (string, error) { return "", nil }
func (m *respawnMockSession) Close() error                                        { return nil }

// respawnStubGitLab is an httptest-backed GitLab stub: GET issue returns a
// fixed Issue payload (mutable via setIssue), everything else (PUT/POST) is
// recorded and answered with 200 {}.
type respawnStubGitLab struct {
	mu      sync.Mutex
	issue   gitlab.Issue
	putForm []url.Values
	notes   []gitlab.Note
}

func (s *respawnStubGitLab) setIssue(i gitlab.Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issue = i
}

func (s *respawnStubGitLab) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/notes"):
		s.mu.Lock()
		notes := s.notes
		s.mu.Unlock()
		json.NewEncoder(w).Encode(notes)
		return
	case r.Method == http.MethodGet:
		s.mu.Lock()
		issue := s.issue
		s.mu.Unlock()
		json.NewEncoder(w).Encode(issue)
		return
	case r.Method == http.MethodPut:
		r.ParseForm()
		s.mu.Lock()
		s.putForm = append(s.putForm, r.Form)
		s.mu.Unlock()
		w.Write([]byte(`{}`))
		return
	default:
		w.Write([]byte(`{}`))
	}
}

func newRespawnStub(t *testing.T, issue gitlab.Issue) (*respawnStubGitLab, *gitlab.Client) {
	t.Helper()
	stub := &respawnStubGitLab{issue: issue}
	// gitlab.Client always dials https:// (apiURL), so the stub must be TLS —
	// srv.Client() trusts this server's self-signed cert.
	srv := httptest.NewTLSServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)
	gl := gitlab.NewClient(srv.Listener.Addr().String(), "test-token")
	gl.HTTP = srv.Client()
	return stub, gl
}

// installFakeAOC puts a fake `aoc` binary on PATH that always exits 0 for any
// `spawn` invocation, and appends the invocation args to a log file so tests
// can assert on what it was called with.
func installFakeAOC(t *testing.T) (logPath string) {
	t.Helper()
	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "aoc.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logPath + "\nexit 0\n"
	fakeAoc := filepath.Join(binDir, "aoc")
	if err := os.WriteFile(fakeAoc, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake aoc: %v", err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	return logPath
}

func newRespawnVignoble(t *testing.T, project, projectPath, repo string) *config.Vignoble {
	t.Helper()
	vigDir := t.TempDir()
	return &config.Vignoble{
		Path:       vigDir,
		Name:       "testv",
		ConfigPath: filepath.Join(vigDir, "vignes.yaml"),
		Config: &config.VignobleConfig{
			Vignes: map[string]config.Vigne{
				project: {Repo: repo, Path: projectPath},
			},
		},
	}
}

const (
	respawnOwner = "lelongs"
	respawnBot   = "pinard-bot"
)

// TestRespawnIssue_ClearsLabelsReapsAndSpawns is the happy path: an
// already-spawned, owner-approved issue with a stale worker/worktree is
// respawned in one call — labels cleared, stale worker reaped, state reset,
// new spawn triggered immediately.
func TestRespawnIssue_ClearsLabelsReapsAndSpawns(t *testing.T) {
	const project = "my-proj"
	const sessionName = "my-proj--my-proj-42abc"

	projectPath := makeRespawnTestRepo(t, sessionName)
	vig := newRespawnVignoble(t, project, projectPath, "mygroup/my-proj")

	issue := gitlab.Issue{IID: 42, Title: "Do the thing", Labels: []string{"pinard:discarded", "in-progress", "bug"}}
	issue.Author.Username = respawnOwner
	stub, gl := newRespawnStub(t, issue)
	_ = stub

	aocLog := installFakeAOC(t)

	kv := newMockKV()
	kv.setAgent("my-proj-swe-42", map[string]any{
		"project": project,
		"issue":   "42",
		"name":    sessionName,
	})

	sess := &respawnMockSession{}

	stateDir := t.TempDir()
	st, err := state.Load[state.IssueWatcherState](filepath.Join(stateDir, "issue-watcher.yaml"))
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	st.Update(func(s *state.IssueWatcherState) {
		s.Seen = map[string]map[string]*state.SeenIssue{
			project: {"42": {Status: "spawned", Title: "Do the thing"}},
		}
	})

	w := &IssueWatcher{
		State:    st,
		KV:       kv,
		GitLab:   gl,
		Vignoble: vig,
		User:     respawnBot,
		Owner:    respawnOwner,
		Session:  sess,
	}

	summary, err := w.RespawnIssue(project, 42, false)
	if err != nil {
		t.Fatalf("RespawnIssue failed: %v (summary: %s)", err, summary)
	}

	if !strings.Contains(summary, "cleared labels") {
		t.Errorf("expected summary to mention cleared labels, got: %s", summary)
	}
	if !strings.Contains(summary, "reaped stale worker") {
		t.Errorf("expected summary to mention reaped worker, got: %s", summary)
	}
	if !strings.Contains(summary, "spawned worker") {
		t.Errorf("expected summary to mention spawn, got: %s", summary)
	}

	// Stale tmux session was stopped.
	if len(sess.stopped) != 1 || sess.stopped[0] != sessionName {
		t.Errorf("expected StopWorker(%q), got %v", sessionName, sess.stopped)
	}

	// KV entry removed.
	if data, _ := kv.Get("pinard-agents", "my-proj-swe-42"); data != nil {
		t.Errorf("expected KV entry deleted, still present: %v", data)
	}

	// Worktree removed.
	if _, err := os.Stat(filepath.Join(projectPath, ".worktrees", sessionName)); err == nil {
		t.Error("expected stale worktree to be removed")
	}

	// Labels cleared: remove_labels included pinard:discarded and in-progress.
	stub.mu.Lock()
	var sawRemoveLabels bool
	for _, form := range stub.putForm {
		rl := form.Get("remove_labels")
		if strings.Contains(rl, "pinard:discarded") && strings.Contains(rl, "in-progress") {
			sawRemoveLabels = true
		}
	}
	stub.mu.Unlock()
	if !sawRemoveLabels {
		t.Error("expected a PUT with remove_labels containing pinard:discarded and in-progress")
	}

	// New spawn was triggered via `aoc spawn ... --issue 42`.
	logBytes, _ := os.ReadFile(aocLog)
	logStr := string(logBytes)
	if !strings.Contains(logStr, "spawn") || !strings.Contains(logStr, "--issue 42") {
		t.Errorf("expected aoc spawn --issue 42 invocation, got log: %q", logStr)
	}

	// State ends up "spawned" again after the new spawn succeeds.
	st.Read(func(s *state.IssueWatcherState) {
		entry := s.Seen[project]["42"]
		if entry == nil {
			t.Fatal("state entry missing after respawn")
		}
		if entry.Status != "spawned" {
			t.Errorf("expected status=spawned after respawn, got %q", entry.Status)
		}
	})
}

// TestRespawnIssue_BlockedLabelRefusesToRespawn verifies that a `blocked`
// issue is never force-spawned by RespawnIssue.
func TestRespawnIssue_BlockedLabelRefusesToRespawn(t *testing.T) {
	const project = "my-proj"
	projectPath := makeRespawnTestRepo(t, "unused-session")
	vig := newRespawnVignoble(t, project, projectPath, "mygroup/my-proj")

	issue := gitlab.Issue{IID: 7, Title: "Blocked work", Labels: []string{"blocked"}}
	issue.Author.Username = respawnOwner
	_, gl := newRespawnStub(t, issue)
	aocLog := installFakeAOC(t)

	stateDir := t.TempDir()
	st, _ := state.Load[state.IssueWatcherState](filepath.Join(stateDir, "issue-watcher.yaml"))

	w := &IssueWatcher{
		State:    st,
		KV:       newMockKV(),
		GitLab:   gl,
		Vignoble: vig,
		User:     respawnBot,
		Owner:    respawnOwner,
	}

	_, err := w.RespawnIssue(project, 7, false)
	if err == nil {
		t.Fatal("expected error for blocked issue, got nil")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("expected error to mention 'blocked', got: %v", err)
	}

	// No spawn should have been attempted.
	if data, err := os.ReadFile(aocLog); err == nil && len(data) > 0 {
		t.Errorf("expected no aoc invocation for a blocked issue, got: %q", data)
	}
}

// TestRespawnIssue_HoldsWhenOwnerNotApproved verifies that RespawnIssue never
// bypasses the owner-approval gate: an unapproved issue is held, not spawned,
// and the returned error names the remedy.
func TestRespawnIssue_HoldsWhenOwnerNotApproved(t *testing.T) {
	const project = "my-proj"
	projectPath := makeRespawnTestRepo(t, "unused-session")
	vig := newRespawnVignoble(t, project, projectPath, "mygroup/my-proj")

	issue := gitlab.Issue{IID: 9, Title: "Needs approval"}
	issue.Author.Username = "someone-else" // not the owner
	_, gl := newRespawnStub(t, issue)
	aocLog := installFakeAOC(t)

	stateDir := t.TempDir()
	st, _ := state.Load[state.IssueWatcherState](filepath.Join(stateDir, "issue-watcher.yaml"))

	w := &IssueWatcher{
		State:    st,
		KV:       newMockKV(),
		GitLab:   gl,
		Vignoble: vig,
		User:     respawnBot,
		Owner:    respawnOwner,
	}

	summary, err := w.RespawnIssue(project, 9, false)
	if err == nil {
		t.Fatal("expected error when owner has not approved")
	}
	if !strings.Contains(err.Error(), "owner approval") {
		t.Errorf("expected error to name the remedy (owner approval), got: %v (%s)", err, summary)
	}

	st.Read(func(s *state.IssueWatcherState) {
		entry := s.Seen[project]["9"]
		if entry == nil || entry.Status != "awaiting-approval" {
			t.Errorf("expected status=awaiting-approval, got %+v", entry)
		}
	})

	if data, err := os.ReadFile(aocLog); err == nil && len(data) > 0 {
		t.Errorf("expected no aoc invocation while awaiting approval, got: %q", data)
	}
}

// TestRespawnIssue_OpenMRRefusesWithoutForce verifies the reviewer-flagged gap
// on MR !626: RespawnIssue must not reap a worker with an open, unmerged MR
// unless force is set — otherwise the fresh worker would open a second MR for
// the same issue while the first is left orphaned (local branch deleted,
// remote branch + MR surviving with no worker attached).
func TestRespawnIssue_OpenMRRefusesWithoutForce(t *testing.T) {
	const project = "my-proj"
	const sessionName = "my-proj--my-proj-42abc"
	const agentKey = "my-proj-swe-42"

	projectPath := makeRespawnTestRepo(t, sessionName)
	vig := newRespawnVignoble(t, project, projectPath, "mygroup/my-proj")

	issue := gitlab.Issue{IID: 42, Title: "Do the thing", Labels: []string{"pinard:discarded", "in-progress", "bug"}}
	issue.Author.Username = respawnOwner
	_, gl := newRespawnStub(t, issue)
	aocLog := installFakeAOC(t)

	kv := newMockKV()
	kv.setAgent(agentKey, map[string]any{
		"project": project,
		"issue":   "42",
		"name":    sessionName,
	})

	sess := &respawnMockSession{}

	stateDir := t.TempDir()
	st, _ := state.Load[state.IssueWatcherState](filepath.Join(stateDir, "issue-watcher.yaml"))
	st.Update(func(s *state.IssueWatcherState) {
		s.Seen = map[string]map[string]*state.SeenIssue{
			project: {"42": {Status: "spawned", Title: "Do the thing"}},
		}
	})

	mrSt, _ := state.Load[state.MRWatcherState](filepath.Join(stateDir, "mr-watcher.yaml"))
	mrSt.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			agentKey: {Name: sessionName, Project: project, MR: 123, State: ""},
		}
	})

	w := &IssueWatcher{
		State:    st,
		KV:       kv,
		GitLab:   gl,
		Vignoble: vig,
		User:     respawnBot,
		Owner:    respawnOwner,
		Session:  sess,
		MRState:  mrSt,
	}

	summary, err := w.RespawnIssue(project, 42, false)
	if err == nil {
		t.Fatalf("expected refusal for an open MR, got success: %s", summary)
	}
	if !strings.Contains(err.Error(), "open MR") || !strings.Contains(err.Error(), "123") {
		t.Errorf("expected error to name the open MR (123), got: %v", err)
	}

	// Nothing was torn down: session untouched, KV entry intact, worktree intact.
	if len(sess.stopped) != 0 {
		t.Errorf("expected no StopWorker call, got %v", sess.stopped)
	}
	if data, _ := kv.Get("pinard-agents", agentKey); data == nil {
		t.Error("expected KV entry to survive the refusal")
	}
	if _, err := os.Stat(filepath.Join(projectPath, ".worktrees", sessionName)); err != nil {
		t.Error("expected worktree to survive the refusal")
	}

	// No second spawn was triggered.
	if data, err := os.ReadFile(aocLog); err == nil && len(data) > 0 {
		t.Errorf("expected no aoc invocation when refusing an open-MR reap, got: %q", data)
	}

	// Watcher state was NOT reset to seen — the refusal must not clear the way
	// for a normal poll to spawn a second worker either.
	st.Read(func(s *state.IssueWatcherState) {
		entry := s.Seen[project]["42"]
		if entry == nil || entry.Status != "spawned" {
			t.Errorf("expected status to remain spawned after refusal, got %+v", entry)
		}
	})
}

// TestRespawnIssue_ForceRespawnsOverOpenMRAndDropsWatchedEntry verifies that
// force=true is the deliberate escape hatch: it reaps the worker despite the
// open MR and drops the now-stale WatchedMR entry so the MR watcher doesn't
// keep polling a session that no longer exists.
func TestRespawnIssue_ForceRespawnsOverOpenMRAndDropsWatchedEntry(t *testing.T) {
	const project = "my-proj"
	const sessionName = "my-proj--my-proj-42abc"
	const agentKey = "my-proj-swe-42"

	projectPath := makeRespawnTestRepo(t, sessionName)
	vig := newRespawnVignoble(t, project, projectPath, "mygroup/my-proj")

	issue := gitlab.Issue{IID: 42, Title: "Do the thing", Labels: []string{"pinard:discarded", "in-progress", "bug"}}
	issue.Author.Username = respawnOwner
	_, gl := newRespawnStub(t, issue)
	aocLog := installFakeAOC(t)

	kv := newMockKV()
	kv.setAgent(agentKey, map[string]any{
		"project": project,
		"issue":   "42",
		"name":    sessionName,
	})

	sess := &respawnMockSession{}

	stateDir := t.TempDir()
	st, _ := state.Load[state.IssueWatcherState](filepath.Join(stateDir, "issue-watcher.yaml"))
	st.Update(func(s *state.IssueWatcherState) {
		s.Seen = map[string]map[string]*state.SeenIssue{
			project: {"42": {Status: "spawned", Title: "Do the thing"}},
		}
	})

	mrSt, _ := state.Load[state.MRWatcherState](filepath.Join(stateDir, "mr-watcher.yaml"))
	mrSt.Update(func(s *state.MRWatcherState) {
		s.Watched = map[string]*state.WatchedMR{
			agentKey: {Name: sessionName, Project: project, MR: 123, State: ""},
		}
	})

	w := &IssueWatcher{
		State:    st,
		KV:       kv,
		GitLab:   gl,
		Vignoble: vig,
		User:     respawnBot,
		Owner:    respawnOwner,
		Session:  sess,
		MRState:  mrSt,
	}

	summary, err := w.RespawnIssue(project, 42, true)
	if err != nil {
		t.Fatalf("RespawnIssue with force failed: %v (summary: %s)", err, summary)
	}
	if !strings.Contains(summary, "dropped its stale MR-watcher entry") {
		t.Errorf("expected summary to mention dropping the stale MR-watcher entry, got: %s", summary)
	}

	// Worker was torn down despite the open MR.
	if len(sess.stopped) != 1 || sess.stopped[0] != sessionName {
		t.Errorf("expected StopWorker(%q), got %v", sessionName, sess.stopped)
	}
	if data, _ := kv.Get("pinard-agents", agentKey); data != nil {
		t.Errorf("expected KV entry deleted, still present: %v", data)
	}

	// The stale WatchedMR entry was dropped.
	mrSt.Read(func(s *state.MRWatcherState) {
		if _, ok := s.Watched[agentKey]; ok {
			t.Errorf("expected WatchedMR entry %q to be dropped, still present", agentKey)
		}
	})

	// New spawn was still triggered.
	logBytes, _ := os.ReadFile(aocLog)
	if !strings.Contains(string(logBytes), "--issue 42") {
		t.Errorf("expected aoc spawn --issue 42 invocation, got log: %q", logBytes)
	}
}
