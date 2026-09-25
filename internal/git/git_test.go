package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// defaultBranchForGit returns the default branch name used by git init on this
// system — "master" for git < 2.28, "main" if init.defaultBranch is configured.
func defaultBranchForGit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "config", "--global", "init.defaultBranch").Output()
	if err == nil {
		if b := strings.TrimSpace(string(out)); b != "" {
			return b
		}
	}
	// git < 2.28 has no init.defaultBranch setting and defaults to master.
	return "master"
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("command %v failed: %v\n%s", append([]string{name}, args...), err, out)
	}
}

// makeTestRepo creates a temporary bare "origin" repo and a local clone,
// initialises them with a commit on the system default branch, and returns the paths.
func makeTestRepo(t *testing.T) (bareDir, cloneDir string) {
	t.Helper()
	tmp := t.TempDir()

	bareDir = filepath.Join(tmp, "origin.git")
	cloneDir = filepath.Join(tmp, "clone")
	wdDir := filepath.Join(tmp, "init-wd")

	// Init bare repo.
	mustRun(t, "", "git", "init", "--bare", bareDir)

	// Create a working repo, commit, and push to bare.
	mustRun(t, "", "git", "init", wdDir)
	mustRun(t, wdDir, "git", "config", "user.email", "test@example.com")
	mustRun(t, wdDir, "git", "config", "user.name", "Test")

	readmePath := filepath.Join(wdDir, "README")
	if err := os.WriteFile(readmePath, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, wdDir, "git", "add", "README")
	mustRun(t, wdDir, "git", "commit", "-m", "init")

	defBranch := defaultBranchForGit(t)
	mustRun(t, wdDir, "git", "remote", "add", "origin", bareDir)
	mustRun(t, wdDir, "git", "push", "-u", "origin", defBranch)

	// Set origin/HEAD on the bare repo so DefaultBranch() resolves correctly.
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/"+defBranch)

	// Clone into the working copy.
	mustRun(t, "", "git", "clone", bareDir, cloneDir)
	mustRun(t, cloneDir, "git", "config", "user.email", "test@example.com")
	mustRun(t, cloneDir, "git", "config", "user.name", "Test")

	return bareDir, cloneDir
}

// TestRemoteBranchExists verifies that the helper correctly reports presence/absence.
func TestRemoteBranchExists(t *testing.T) {
	_, cloneDir := makeTestRepo(t)
	defBranch := defaultBranchForGit(t)

	// The default branch was pushed during setup — must exist.
	if !RemoteBranchExists(cloneDir, "origin", defBranch) {
		t.Errorf("expected %s to exist on origin", defBranch)
	}

	// "cuvee/does-not-exist" was never pushed.
	if RemoteBranchExists(cloneDir, "origin", "cuvee/does-not-exist") {
		t.Error("expected cuvee/does-not-exist to be absent on origin")
	}
}

// TestDefaultBranch checks that the default branch is resolved from origin/HEAD.
func TestDefaultBranch(t *testing.T) {
	_, cloneDir := makeTestRepo(t)
	defBranch := defaultBranchForGit(t)

	got, err := DefaultBranch(cloneDir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != defBranch {
		t.Errorf("expected %q, got %q", defBranch, got)
	}
}

// TestDefaultBranch_NoOriginHead verifies that DefaultBranch falls back to
// `git ls-remote --symref` (instead of a hardcoded guess) when the checkout has
// no local origin/HEAD ref — e.g. a checkout built via init+remote+fetch rather
// than clone, or one re-pointed at a new remote. It also verifies the self-heal:
// origin/HEAD is set afterwards so the fast path resolves on the next call.
func TestDefaultBranch_NoOriginHead(t *testing.T) {
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "origin.git")
	wdDir := filepath.Join(tmp, "init-wd")
	checkoutDir := filepath.Join(tmp, "checkout")

	mustRun(t, "", "git", "init", "--bare", bareDir)

	mustRun(t, "", "git", "init", wdDir)
	mustRun(t, wdDir, "git", "config", "user.email", "test@example.com")
	mustRun(t, wdDir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wdDir, "README"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, wdDir, "git", "add", "README")
	mustRun(t, wdDir, "git", "commit", "-m", "init")

	// Use a branch name that is deliberately not "main", to catch any
	// hardcoded fallback.
	const realDefault = "trunk"
	mustRun(t, wdDir, "git", "branch", "-m", realDefault)
	mustRun(t, wdDir, "git", "remote", "add", "origin", bareDir)
	mustRun(t, wdDir, "git", "push", "-u", "origin", realDefault)
	mustRun(t, bareDir, "git", "symbolic-ref", "HEAD", "refs/heads/"+realDefault)

	// Build the checkout via init+remote+fetch, which never creates
	// refs/remotes/origin/HEAD (unlike `git clone`).
	mustRun(t, "", "git", "init", checkoutDir)
	mustRun(t, checkoutDir, "git", "remote", "add", "origin", bareDir)
	mustRun(t, checkoutDir, "git", "fetch", "origin")
	mustRun(t, checkoutDir, "git", "config", "user.email", "test@example.com")
	mustRun(t, checkoutDir, "git", "config", "user.name", "Test")

	if _, err := run(checkoutDir, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		t.Fatal("expected origin/HEAD to be unset before DefaultBranch is called")
	}

	got, err := DefaultBranch(checkoutDir)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != realDefault {
		t.Errorf("expected %q, got %q", realDefault, got)
	}

	// Self-heal: origin/HEAD should now resolve locally.
	healed, err := run(checkoutDir, "rev-parse", "--abbrev-ref", "origin/HEAD")
	if err != nil {
		t.Fatalf("expected origin/HEAD to be set after DefaultBranch self-heal, got error: %v", err)
	}
	if strings.TrimPrefix(healed, "origin/") != realDefault {
		t.Errorf("expected self-healed origin/HEAD to point at %q, got %q", realDefault, healed)
	}
}

// TestDefaultBranch_UnreachableRemote verifies that DefaultBranch returns an
// error — never a hardcoded guess — when the remote's default branch cannot be
// determined.
func TestDefaultBranch_UnreachableRemote(t *testing.T) {
	tmp := t.TempDir()
	checkoutDir := filepath.Join(tmp, "checkout")
	mustRun(t, "", "git", "init", checkoutDir)
	mustRun(t, checkoutDir, "git", "remote", "add", "origin", filepath.Join(tmp, "does-not-exist.git"))

	got, err := DefaultBranch(checkoutDir)
	if err == nil {
		t.Fatalf("expected error for unreachable remote, got branch %q", got)
	}
	if got == "main" || got == "master" {
		t.Errorf("must not fall back to a hardcoded branch name, got %q", got)
	}
}

// TestEnsureRemoteBranch_MissingBranch verifies that EnsureRemoteBranch creates
// the branch on origin when it is absent, and that the remote-tracking ref exists
// locally afterwards.
func TestEnsureRemoteBranch_MissingBranch(t *testing.T) {
	_, cloneDir := makeTestRepo(t)

	branch := "cuvee/test-feature"
	if err := EnsureRemoteBranch(cloneDir, branch); err != nil {
		t.Fatalf("EnsureRemoteBranch: %v", err)
	}

	if !RemoteBranchExists(cloneDir, "origin", branch) {
		t.Error("branch not found on origin after EnsureRemoteBranch")
	}
}

// TestEnsureRemoteBranch_ExistingBranch verifies that calling EnsureRemoteBranch
// on a branch that already exists is a no-op (idempotent).
func TestEnsureRemoteBranch_ExistingBranch(t *testing.T) {
	_, cloneDir := makeTestRepo(t)
	defBranch := defaultBranchForGit(t)

	branch := "cuvee/already-exists"
	// Create it directly first.
	mustRun(t, cloneDir, "git", "push", "origin", defBranch+":refs/heads/"+branch)

	// Should succeed without error.
	if err := EnsureRemoteBranch(cloneDir, branch); err != nil {
		t.Fatalf("EnsureRemoteBranch on existing branch: %v", err)
	}
}

// TestEnsureRemoteBranch_Concurrent verifies race-safety: two goroutines calling
// EnsureRemoteBranch on the same missing branch at the same time must both succeed.
func TestEnsureRemoteBranch_Concurrent(t *testing.T) {
	_, cloneDir := makeTestRepo(t)

	branch := "cuvee/concurrent-create"

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = EnsureRemoteBranch(cloneDir, branch)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	if !RemoteBranchExists(cloneDir, "origin", branch) {
		t.Error("branch not found on origin after concurrent EnsureRemoteBranch")
	}
}
