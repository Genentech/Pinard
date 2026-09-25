package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/git"
	"github.com/Genentech/pinard/internal/gitlab"
	"github.com/Genentech/pinard/internal/liveness"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/pressoir"
	"github.com/Genentech/pinard/internal/session"
	"github.com/Genentech/pinard/internal/watcher"
	"github.com/spf13/cobra"
)

var spawnCmd = &cobra.Command{
	Use:   "spawn",
	Short: "Launch a Claude agent with its own worktree",
	RunE: func(cmd *cobra.Command, args []string) error {
		project, _ := cmd.Flags().GetString("project")
		prompt, _ := cmd.Flags().GetString("prompt")
		name, _ := cmd.Flags().GetString("name")
		targetBranch, _ := cmd.Flags().GetString("target-branch")
		targetBranchExplicit := targetBranch != ""
		processName, _ := cmd.Flags().GetString("process")
		processArgs, _ := cmd.Flags().GetString("args")
		parcelle, _ := cmd.Flags().GetString("parcelle")
		issueID, _ := cmd.Flags().GetString("issue")
		runID, _ := cmd.Flags().GetString("run-id")
		runtime, _ := cmd.Flags().GetString("runtime")
		sif, _ := cmd.Flags().GetString("sif")
		binds, _ := cmd.Flags().GetStringArray("bind")
		noWorktree, _ := cmd.Flags().GetBool("no-worktree")
		contractID, _ := cmd.Flags().GetString("contract-id")
		noCapsule, _ := cmd.Flags().GetBool("no-capsule")
		ephemeral, _ := cmd.Flags().GetBool("ephemeral")

		if project == "" {
			return fmt.Errorf("--project is required")
		}
		if prompt == "" && processName == "" {
			return fmt.Errorf("--prompt is required (or use --process)")
		}
		if prompt == "" {
			prompt = fmt.Sprintf("Process: %s", processName)
		}

		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}

		vigne, ok := vb.Config.Vignes[project]
		if !ok {
			return fmt.Errorf("project %q not found in vignes.yaml", project)
		}

		projectPath := vigne.ExpandedPath()
		if _, err := os.Stat(projectPath); err != nil {
			return fmt.Errorf("project path %q does not exist", projectPath)
		}

		// Capsule auto-detection: when --issue is given and no explicit --contract-id or
		// --no-capsule, scan the issue (description + comments) for a contract_id and
		// probe Mnemosyne for funding status.
		// - Funded + pubkey match → auto-wire PINARD_CAPSULE_CONTRACT.
		// - Unfunded contract present → fail-closed (refuse to spawn on operator token).
		// - Pubkey mismatch → refuse with a clear message.
		// - No contract → normal spawn.
		// --no-capsule overrides all of the above (operator escape hatch).
		if issueID != "" && contractID == "" && !noCapsule {
			issueIID, err := strconv.Atoi(issueID)
			if err != nil {
				return fmt.Errorf("--issue: invalid IID %q: %w", issueID, err)
			}
			// Build Pressoir for the issue fetch.
			pCfg := vb.ResolvePressoirConfig(vigne.Repo)
			pr, prErr := pressoir.NewPressoir(pCfg, creds)
			if prErr != nil {
				pr = pressoir.NewGitLabAdapter(creds.GitLab.Host, creds.Token())
			}
			// Fetch the issue to get its description.
			issue, err := pr.GetIssue(context.Background(), pressoir.RepoRefFromPath(vigne.Repo), issueIID)
			// ResolveIssueContract uses *gitlab.Client for its internal note scan;
			// construct it lazily only when we have an issue to check.
			gl := gitlab.NewClient(creds.GitLab.Host, creds.Token())
			if err != nil {
				log.Printf("[spawn] cannot fetch issue %s #%d for capsule detection: %v", vigne.Repo, issueIID, err)
				// Non-fatal: fall through without a contract.
			} else {
					// Retry up to 3 times on transient Mnemosyne errors (5xx/network).
				var cr watcher.ContractResult
				const maxRetries = 3
				for attempt := 1; attempt <= maxRetries; attempt++ {
					cr = watcher.ResolveIssueContract(gl, vigne.Repo, issueIID, issue.Body)
					if !cr.Transient {
						break
					}
					if attempt < maxRetries {
						log.Printf("[spawn] Mnemosyne transient error (attempt %d/%d), retrying in 1s…", attempt, maxRetries)
						time.Sleep(time.Second)
					}
				}
				if cr.ContractID != "" {
					if cr.Transient {
						// Still transient after retries — do not fail-closed, do not spawn on operator token.
						return fmt.Errorf("could not verify capsule contract funding for issue #%s (Mnemosyne unavailable) — retry, or pass --contract-id/--no-capsule to override", issueID)
					}
					if cr.Error != "" && !cr.Funded {
						// Permanent failure (contract gone, pubkey mismatch) — refuse.
						return fmt.Errorf("issue #%s capsule contract error: %s", issueID, cr.Error)
					}
					if !cr.Funded {
						// Unfunded contract present — fail-closed.
						return fmt.Errorf("issue #%s is capsule-gated but unfunded (contract %s) — fund it at mnemosyne, or pass --no-capsule to override", issueID, cr.ContractID)
					}
					if !cr.PubkeyMatch {
						// Funded but wrong agent.
						return fmt.Errorf("issue #%s capsule contract %s is funded but targets a different agent pubkey: %s", issueID, cr.ContractID, cr.Error)
					}
					// Auto-wire: treat as if --contract-id was passed.
					contractID = cr.ContractID
					log.Printf("[spawn] issue #%s: auto-wired capsule contract %s", issueID, contractID)
				}
			}
		}

		// Resolve target branch: explicit flag > vigne config > git default branch.
		// This prevents hardcoding "main" on repos whose default branch is "master".
		if !targetBranchExplicit {
			if vigne.DefaultBranch != "" {
				targetBranch = vigne.DefaultBranch
			} else {
				detected, err := git.DefaultBranch(projectPath)
				if err != nil {
					return fmt.Errorf("resolve default branch for %s: %w", project, err)
				}
				targetBranch = detected
			}
		}

		// Per-vigne runtime defaults (flags override config).
		if runtime == "" {
			runtime = vigne.Runtime
		}
		if sif == "" {
			sif = vigne.Sif
		}
		if len(binds) == 0 {
			binds = vigne.Binds
		}
		if !noWorktree {
			noWorktree = vigne.NoWorktree
		}
		if runtime == "singularity" && sif == "" {
			return fmt.Errorf("runtime=singularity requires --sif (or vigne.sif)")
		}

		// Parcelle (workstream) name — defaults to the project. Computed once here
		// and reused for the session name and the run directory.
		parcelleName := parcelle
		if parcelleName == "" {
			parcelleName = project
		}

		// Generate session name: `<parcelle>--<project>-<id><rand>`. The parcelle
		// leads so a raw `tmux ls` / prefix+f picker is self-describing and
		// filterable by parcelle; the vignoble is dropped (the socket
		// `pinard-<vignoble>` already scopes it). `<id>` is the issue IID when
		// spawning for an issue, otherwise an MMSS time token. A random suffix
		// makes the name collision-proof even when the time token repeats (two
		// schedules firing at the same wall-clock second, or a daily cron at a
		// fixed time colliding with yesterday's leftover branch).
		if name == "" {
			id := issueID
			if id == "" {
				id = time.Now().Format("0405")
			}
			name = workerSessionName(parcelleName, project, id+randSuffix())
		}
		// Ensure the name is safe as a tmux target (and as a branch/worktree dir
		// path) regardless of source (auto-generated, --name, or scheduler).
		name = session.SanitizeName(name)

		// Compute the agent/run ID up front so we can guard against duplicate
		// live workers on the same run (two agents on one run open two MRs).
		force, _ := cmd.Flags().GetBool("force")
		agentID := name
		computedRunID := ""
		if processName != "" {
			if runID != "" {
				computedRunID = runID
			} else if issueID != "" {
				computedRunID = fmt.Sprintf("%s-%s-%s", project, processName, issueID)
			} else {
				computedRunID = fmt.Sprintf("%s-%s-%s", project, processName, name)
			}
			agentID = computedRunID
		}

		// Refuse to spawn if a worker is already live for this run ID.
		if processName != "" && !force {
			if existing := liveness.WorkerForRun(vb.Name, agentID); existing != "" {
				return fmt.Errorf("run %q already has a live vendangeur (session %q); use --force to override", agentID, existing)
			}
		}

		// Persist the original prompt + target branch so orphan-recovery can
		// re-supply them if it has to respawn this run — otherwise a respawn loses
		// the task context and the worker does unrelated work. Written once at first
		// spawn; never overwritten (a respawn passes the original prompt back in).
		if processName != "" && computedRunID != "" {
			runDir := filepath.Join(vb.Path, "parcelles", parcelleName, "runs", computedRunID)
			spawnMetaPath := filepath.Join(runDir, "spawn.json")
			if _, err := os.Stat(spawnMetaPath); err != nil {
				os.MkdirAll(runDir, 0o755)
				spawnMeta := map[string]string{
					"prompt":       prompt,
					"targetBranch": targetBranch,
				}
				// Capture contractId so orphan-recovery can re-pass --contract-id
				// on respawn (design D3 — never spend the operator token on funded
				// work by accident after an OOM/SIGKILL).
				if contractID != "" {
					spawnMeta["contractId"] = contractID
				}
				meta, _ := json.Marshal(spawnMeta)
				os.WriteFile(spawnMetaPath, meta, 0o644)
			}
		}

		// Update main branch
		git.Fetch(projectPath)
		git.Pull(projectPath)

		// Build prompt with instructions
		repo := vigne.Repo
		encodedRepo := url.PathEscape(repo)
		host := creds.GitLab.Host

		// Resolve pressoir adapter for provider-aware prompt guidance.
		promptPCfg := vb.ResolvePressoirConfig(repo)
		promptPR, promptPRErr := pressoir.NewPressoir(promptPCfg, creds)
		if promptPRErr != nil {
			promptPR = pressoir.NewGitLabAdapter(host, creds.Token())
		}

		var fullPrompt string
		if processName != "" {
			// IMPORTANT: this initial message triggers one LLM turn BEFORE the
			// babysitter dispatches the first task. It must be a NO-OP — if it
			// contained the issue/task context, the worker would act on it (e.g.
			// claim/assign the issue to the wrong user, start implementing) before
			// the real first task runs. So: governance rules only, no actionable
			// context, and an explicit "do nothing yet". The babysitter drives every
			// real action via tasks (which carry their own context).
			// GovernancePrompt is the single source of truth — also used by
			// bin/pinard via `aoc governance-prompt` to keep both paths in sync.
			fullPrompt = GovernancePrompt(processName)
			// Issue-driven workers get the issue from the babysitter (fetch-issue
			// task), so the initial turn needs NO context — keeping it context-free
			// prevents the worker from acting on the issue before its first task.
			// Prompt-driven process workers, however, carry their task in `prompt`,
			// so include it as explicitly-non-actionable background.
			if issueID == "" && prompt != "" {
				fullPrompt += fmt.Sprintf("\n\nBackground for the overall job (do NOT act on this until a task tells you to):\n%s", prompt)
			}
		} else {
			workerGuidance := promptPR.WorkerGuidance(repo, host, encodedRepo, targetBranch, creds.GitLab.User, project, name)
			fullPrompt = fmt.Sprintf(`%s

Instructions:
%s
- CRITICAL: You MUST only work within your current working directory (worktree). NEVER access, search, or read files outside your project directory. Do not use find/ls/cat/cd with absolute paths like /home, /data, or ~ to search the filesystem. If you need files from other repos, use the pressoir API (aoc pressoir …).`,
				prompt, workerGuidance,
			)
		}

		// Worktree vs in-place. Code workers get an isolated git worktree (branch per
		// run, MR workflow). Data/orchestration workers (--no-worktree, e.g. a genomics
		// build mutating /data/...) run directly in the project path — there is no branch
		// to open and no MR; isolation comes from the container bind allow-list instead.
		branchName := fmt.Sprintf("pinard/%s", name)
		spawnDir := projectPath
		if !noWorktree {
			spawnDir = filepath.Join(projectPath, ".worktrees", name)
			os.MkdirAll(filepath.Dir(spawnDir), 0755)

			// Ensure the target branch exists on origin before creating the worktree.
			// cuvee/* branches are auto-created off the default branch when missing.
			// Non-cuvee missing branches produce a clear, actionable error.
			if !git.RemoteBranchExists(projectPath, "origin", targetBranch) {
				if strings.HasPrefix(targetBranch, "cuvee/") {
					log.Printf("[spawn] target branch %q missing on origin — creating from default branch and pushing", targetBranch)
					if err := git.EnsureRemoteBranch(projectPath, targetBranch); err != nil {
						return fmt.Errorf("auto-create target branch %q: %w", targetBranch, err)
					}
					log.Printf("[spawn] target branch %q created on origin", targetBranch)
				} else {
					return fmt.Errorf("target branch %q does not exist on origin; only cuvee/* branches are auto-created — push the branch first or use a cuvee/* name", targetBranch)
				}
			}

			startPoint := "origin/" + targetBranch
			if err := git.WorktreeAdd(projectPath, spawnDir, branchName, startPoint); err != nil {
				// Try fallback start points
				if err2 := git.WorktreeAdd(projectPath, spawnDir, branchName, targetBranch); err2 != nil {
					defaultBranch, dbErr := git.DefaultBranch(projectPath)
					if dbErr != nil {
						return fmt.Errorf("failed to create worktree (target %q): %v; default-branch fallback unavailable: %w", targetBranch, err2, dbErr)
					}
					if err3 := git.WorktreeAdd(projectPath, spawnDir, branchName, "origin/"+defaultBranch); err3 != nil {
						// Self-heal: a leftover worktree dir or branch from an
						// abandoned run blocks reuse of this name. Prune it and
						// retry once before giving up.
						if strings.Contains(err3.Error(), "already exists") {
							git.WorktreeRemove(projectPath, spawnDir)
							os.RemoveAll(spawnDir)
							exec.Command("git", "-C", projectPath, "worktree", "prune").Run()
							exec.Command("git", "-C", projectPath, "branch", "-D", branchName).Run()
							if err4 := git.WorktreeAdd(projectPath, spawnDir, branchName, startPoint); err4 != nil {
								return fmt.Errorf("failed to create worktree: %v", err4)
							}
						} else {
							return fmt.Errorf("failed to create worktree: %v", err)
						}
					}
				}
			}
		}

		// Worker permission policy (project-level, defense-in-depth). In-place runs
		// (--no-worktree) must NOT clobber an existing policy in the real repo — and
		// for singularity the bind allow-list is the real sandbox, so skip if present.
		workerPolicyDir := filepath.Join(spawnDir, ".pi", "agent")
		os.MkdirAll(workerPolicyDir, 0755)
		workerPolicyLink := filepath.Join(workerPolicyDir, "pi-permissions.jsonc")
		vignePolicy := filepath.Join(vb.Path, "vignes", project, ".pi", "agent", "pi-permissions.jsonc")
		_, policyExists := os.Stat(workerPolicyLink)
		switch {
		case noWorktree && policyExists == nil:
			// Keep the repo's existing policy untouched.
		case func() bool { _, e := os.Stat(vignePolicy); return e == nil }():
			os.Symlink(vignePolicy, workerPolicyLink)
		default:
			defaultPolicy := `{
  "defaultPolicy": {
    "tools": "allow",
    "bash": "allow",
    "mcp": "deny",
    "skills": "allow",
    "special": "allow"
  },
  "special": {
    "external_directory": "deny"
  }
}
`
			os.WriteFile(workerPolicyLink, []byte(defaultPolicy), 0644)
		}

		// Ensure trusted worker policy exists (overrides global external_directory: allow)
		home, _ := os.UserHomeDir()
		trustedWorkerPolicy := filepath.Join(home, ".pi", "agent", "worker-policy", "pi-permissions.jsonc")
		if _, err := os.Stat(trustedWorkerPolicy); err != nil {
			os.MkdirAll(filepath.Dir(trustedWorkerPolicy), 0755)
			os.WriteFile(trustedWorkerPolicy, []byte(`{
  "defaultPolicy": {
    "tools": "allow",
    "bash": "allow",
    "mcp": "deny",
    "skills": "allow",
    "special": "allow"
  },
  "special": {
    "external_directory": "deny"
  }
}
`), 0644)
		}

		// Build worker env — explicit allow-list only. The worker command uses
		// `env -i` so the tmux shell inherits NOTHING from the daemon; only the
		// vars listed here reach the worker process. This prevents operator PATs
		// (e.g. EXOHUB_GITLAB_TOKEN) and any PINARD_OWNER_* vars that happen to
		// be in the daemon's environment from leaking into an LLM-driven worker.
		envParts := []string{
			fmt.Sprintf("HOME='%s'", os.Getenv("HOME")),
			fmt.Sprintf("PATH='%s'", os.Getenv("PATH")),
			fmt.Sprintf("GITLAB_HOST='%s'", host),
			fmt.Sprintf("GLAB_HOST='%s'", host),
		}
		// Pass through TERM so tmux/readline work correctly inside the worker shell.
		if term := os.Getenv("TERM"); term != "" {
			envParts = append(envParts, fmt.Sprintf("TERM='%s'", term))
		}
		// USER is expected by various tools (git, ssh, engram).
		if user := os.Getenv("USER"); user != "" {
			envParts = append(envParts, fmt.Sprintf("USER='%s'", user))
		}
		// TMPDIR / TMP for temporary file storage.
		if tmpdir := os.Getenv("TMPDIR"); tmpdir != "" {
			envParts = append(envParts, fmt.Sprintf("TMPDIR='%s'", tmpdir))
		} else if tmp := os.Getenv("TMP"); tmp != "" {
			envParts = append(envParts, fmt.Sprintf("TMP='%s'", tmp))
		}
		if token := creds.Token(); token != "" {
			envParts = append(envParts, fmt.Sprintf("GITLAB_TOKEN='%s'", token))
			envParts = append(envParts, fmt.Sprintf("GLAB_TOKEN='%s'", token))
		}
		if sshKey := creds.SSHKeyPath(); sshKey != "" {
			envParts = append(envParts, fmt.Sprintf("GIT_SSH_COMMAND='ssh -i %s -o IdentitiesOnly=yes'", sshKey))
		}
		// GitHub repos: wire HTTPS PAT for git clone/push instead of SSH.
		// GIT_CONFIG_COUNT/KEY/VALUE inject a per-process credential helper that
		// supplies the PAT without writing it to any file on disk.
		if promptPCfg.Provider() == "github" {
			if ghToken := creds.GitHubToken(); ghToken != "" {
				ghHost := creds.GitHubHost() // user-facing host, e.g. "github.com"
				envParts = append(envParts, "PINARD_GITHUB_TOKEN='" + ghToken + "'")
				envParts = append(envParts, "GIT_CONFIG_COUNT='1'")
				envParts = append(envParts, fmt.Sprintf("GIT_CONFIG_KEY_0='url.https://x-access-token:%s@%s/.insteadOf'", ghToken, ghHost))
				envParts = append(envParts, fmt.Sprintf("GIT_CONFIG_VALUE_0='https://%s/'", ghHost))
			}
		}
		if creds.GitLab.User != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_GITLAB_USER='%s'", creds.GitLab.User))
		}
		reviewer := creds.GitLab.Reviewer
		if reviewer == "" {
			// Extract username from git email (before @)
			email := gitConfigValue("user.email")
			if idx := strings.Index(email, "@"); idx > 0 {
				reviewer = email[:idx]
			}
		}
		if reviewer != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_REVIEWER='%s'", reviewer))
		}
		// Author = user (they directed the work)
		// Committer = pinard (pushed via pinard SSH key)
		authorName := gitConfigValue("user.name")
		authorEmail := gitConfigValue("user.email")
		if authorName != "" {
			envParts = append(envParts, fmt.Sprintf("GIT_AUTHOR_NAME='%s'", authorName))
		}
		if authorEmail != "" {
			envParts = append(envParts, fmt.Sprintf("GIT_AUTHOR_EMAIL='%s'", authorEmail))
		}
		// Committer identity: GitHub provider uses github.git_name/git_email;
		// GitLab uses gitlab.git_name/git_email. Fall back to the other provider's
		// values so mixed vignobles don't silently omit committer attribution.
		committerName := creds.GitLab.GitName
		committerEmail := creds.GitLab.GitEmail
		if promptPCfg.Provider() == "github" {
			if creds.GitHub.GitName != "" {
				committerName = creds.GitHub.GitName
			}
			if creds.GitHub.GitEmail != "" {
				committerEmail = creds.GitHub.GitEmail
			}
		}
		if committerName != "" {
			envParts = append(envParts, fmt.Sprintf("GIT_COMMITTER_NAME='%s'", committerName))
		}
		if committerEmail != "" {
			envParts = append(envParts, fmt.Sprintf("GIT_COMMITTER_EMAIL='%s'", committerEmail))
		}
		if pass := creds.NATSPassword(); pass != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_NATS_PASS='%s'", pass))
		}
		if creds.NATS.User != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_NATS_USER='%s'", creds.NATS.User))
		}
		if creds.NATS.URL != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_NATS_URL='%s'", creds.NATS.URL))
		}

		escapedPrompt := strings.ReplaceAll(fullPrompt, "'", "'\\''")
		// workerEnv is built after the launcher block so that any URL injections
		// appended to envParts by the singularity branch are included.
		workerEnv := "" // set below after the launcher block

		vignobleWorkerDefault := vb.Config.Models.Worker.Tier
		if vignobleWorkerDefault == "" {
			vignobleWorkerDefault = vb.Config.Models.Worker.ID
		}
		workerModel := vigne.WorkerModel(vignobleWorkerDefault)
		modelFlag := fmt.Sprintf("--model '%s' ", workerModel)

		processFlag := ""
		if processName != "" {
			processFlag = fmt.Sprintf("--process '%s' ", processName)
		}
		argsFlag := ""
		if processArgs != "" {
			escapedArgs := strings.ReplaceAll(processArgs, "'", "'\\''")
			argsFlag = fmt.Sprintf("--args '%s' ", escapedArgs)
		}
		// Always pass the effective parcelle (defaults to project) so the worker
		// publishes under `parcelles.<parcelle>` — agent subjects are always
		// parcelle-scoped, even for default-bucket workers.
		parcelleFlag := fmt.Sprintf("--parcelle '%s' ", parcelleName)
		issueFlag := ""
		if issueID != "" {
			issueFlag = fmt.Sprintf("--issue '%s' ", issueID)
		}
		runIDFlag := ""
		if runID != "" {
			runIDFlag = fmt.Sprintf("--run-id '%s' ", runID)
		}

		// Launcher: bare `pinard` locally, or a sandboxed `singularity run` for
		// runtime=singularity. The worker flags are identical either way — only the
		// launcher binary changes. --containall hides everything not explicitly bound,
		// so the agent's blast radius is exactly the bind allow-list.
		launcher := "pinard"
		if runtime == "singularity" {
			// Expand a leading ~ in sif/binds (the shell won't, since we quote them).
			home, _ := os.UserHomeDir()
			expandTilde := func(p string) string {
				if strings.HasPrefix(p, "~/") {
					return filepath.Join(home, p[2:])
				}
				return p
			}
			sif = expandTilde(sif)
			expandedBinds := make([]string, 0, len(binds))
			for _, b := range binds {
				// A bind may be "host:container[:ro]"; expand ~ in each path segment.
				segs := strings.Split(b, ":")
				for i := range segs {
					segs[i] = expandTilde(segs[i])
				}
				expandedBinds = append(expandedBinds, strings.Join(segs, ":"))
			}
			binds = expandedBinds
			bindArgs := ""
			for _, b := range binds {
				// Skip bind entries whose host source is absent — warn but don't abort.
				// This lets optional binds (e.g. cert dirs that only exist on some nodes)
				// be declared without failing the launch on hosts that lack them.
				hostSrc := strings.SplitN(b, ":", 2)[0]
				if _, statErr := os.Stat(hostSrc); statErr != nil {
					log.Printf("[spawn] skipping bind %q: host source does not exist", b)
					continue
				}
				bindArgs += fmt.Sprintf("--bind '%s' ", b)
			}
			// Pass bootstrap URLs into the container env when set on the host.
			if v := os.Getenv("PINARD_UNCORK_URL"); v != "" {
				envParts = append(envParts, fmt.Sprintf("PINARD_UNCORK_URL='%s'", strings.ReplaceAll(v, "'", "'\\''"))) 
			}
			if v := os.Getenv("PINARD_POUR_URL"); v != "" {
				envParts = append(envParts, fmt.Sprintf("PINARD_POUR_URL='%s'", strings.ReplaceAll(v, "'", "'\\''"))) 
			}
			launcher = fmt.Sprintf("singularity run --containall --writable-tmpfs %s'%s'", bindArgs, sif)
		}

		// Capsule contract: inject PINARD_CAPSULE_CONTRACT when --contract-id is given.
		if contractID != "" {
			envParts = append(envParts, fmt.Sprintf("PINARD_CAPSULE_CONTRACT='%s'", strings.ReplaceAll(contractID, "'", "'\\''"))) 
			// Isolation: for non-sandboxed (bare `pinard`) capsule workers, give the worker its
			// own PI_CODING_AGENT_DIR so ensure-proxy-provider writes the capsule token to a
			// private auth.json, not the shared ~/.pi/agent/auth.json that holds the operator
			// token. Without this, ensure-proxy-provider finds the operator token in the shared
			// file, returns early, and capsule-redeem never runs — the worker silently bills
			// the operator instead of the funded contract (issue #110).
			// Singularity --containall workers already get an isolated $HOME from the sandbox;
			// this guard is only needed for bare (non-sandboxed) spawns.
			if runtime != "singularity" {
				isolatedAgentDir := filepath.Join(spawnDir, ".pi", "pi-agent")
				os.MkdirAll(isolatedAgentDir, 0o755)
				envParts = append(envParts, fmt.Sprintf("PI_CODING_AGENT_DIR='%s'", strings.ReplaceAll(isolatedAgentDir, "'", "'\\''"))) 
			}
		}

		// Issue URL: let the worker embed it in its KV state so the webterm gateway
		// can surface a clickable link in the terminal header.
		if issueID != "" && repo != "" {
			issueURL := fmt.Sprintf("https://%s/%s/-/issues/%s", host, repo, issueID)
			envParts = append(envParts, fmt.Sprintf("PINARD_ISSUE_URL='%s'", issueURL))
		}

		// Build the final env string now that all envParts additions are done.
		workerEnv = strings.Join(envParts, " ")

		workerFlags := fmt.Sprintf("--worker --vignoble '%s' --project '%s' --session-name '%s' --target-branch '%s' %s%s%s%s%s%s--prompt '%s'",
			vb.Path, project, name, targetBranch, modelFlag, processFlag, argsFlag, parcelleFlag, issueFlag, runIDFlag, escapedPrompt)

		// `env -i` clears the inherited environment entirely before applying the
		// allow-list, so no daemon env var (operator PAT, PINARD_OWNER_*, etc.)
		// can reach the worker even if it was present in the daemon's process env.
		workerCmd := fmt.Sprintf("cd '%s' && env -i %s %s %s\n", spawnDir, workerEnv, launcher, workerFlags)

		sm := session.New()
		defer sm.Close()

		if err := sm.SpawnWorker(vb.Name, name, workerCmd); err != nil {
			return fmt.Errorf("spawn vendangeur failed: %w", err)
		}

		// Publish initial state to KV
		nc := pnats.NewClient(creds)
		if err := nc.Connect(); err == nil {
			kv := pnats.NewKV(nc)
			kvState := map[string]any{
				"project":  project,
				"repo":     repo,
				"state":    "running",
				"tempo":    "active",
				"cwd":      spawnDir,
				"name":     name,
				"agentId":  agentID,
				"vignoble": vb.Name,
			}
			if processName != "" {
				kvState["process"] = processName
			}
			if computedRunID != "" {
				kvState["runId"] = computedRunID
			}
			// Always record the effective parcelle so watchers can resolve
			// parcelle-scoped subjects and the dashboard can group by parcelle.
			kvState["parcelle"] = parcelleName
			if issueID != "" {
				kvState["issue"] = issueID
			}
			if ephemeral {
				kvState["ephemeral"] = true
			}
			kv.Put("pinard-agents", agentID, kvState)
			nc.Close()
		}

		log.Printf("Spawned: %s on %s in %s", name, project, spawnDir)
		fmt.Printf("Spawned: %s on %s in %s\n", name, project, spawnDir)
		fmt.Printf("  tmux -L pinard-%s attach -t %s\n", vb.Name, name)

		return nil
	},
}

// workerSessionName builds a parcelle-leading, tmux-safe worker session name:
// `<parcelle>--<project>-<id>`. The parcelle leads so `tmux ls` / prefix+f is
// self-describing and filterable by parcelle; the vignoble is omitted (the tmux
// socket already scopes it). Callers pass the collision suffix as part of id.
// GovernancePrompt returns the no-op bootstrap prompt for a process worker.
// This is the single source of truth used by both `aoc spawn` and `bin/pinard`
// via `aoc governance-prompt`, so both paths always stay in sync.
func GovernancePrompt(processName string) string {
	return fmt.Sprintf(`You are a worker governed by a babysitter process (%s). Work is dispatched as tasks, ONE AT A TIME.

You have NO task yet. Do NOT take any action now — do not read or change code, do not
touch the git host, do not claim/assign/label anything, do not open a pull request. Reply only with
"ready" and wait. Your first task will arrive momentarily.

STRICT step discipline (every task):
- Do EXACTLY what the current task says — nothing more, nothing less.
- Never anticipate or start later steps. claim, analyze, implement, test, open pull request are
  each a SEPARATE task you'll be given in turn.
- Never read/edit code, commit, push, or open a pull request unless the CURRENT task says so.
  Claiming an issue means ONLY updating its labels/assignee, exactly as the task states.
- When the current task is done, call task:post and STOP. Wait for the next task.
- Do not use openspec, skills, or project-specific workflows — work only from the task.

Git host CLI: use aoc pressoir … subcommands for all git host operations (pull requests,
issues, comments). The $GITLAB_HOST environment variable is set when running against GitLab.
Do not run "git remote" to choose a host.`, processName)
}

var governancePromptCmd = &cobra.Command{
	Use:   "governance-prompt",
	Short: "Print the no-op bootstrap governance prompt for a process worker",
	RunE: func(cmd *cobra.Command, args []string) error {
		processName, _ := cmd.Flags().GetString("process")
		if processName == "" {
			return fmt.Errorf("--process is required")
		}
		fmt.Print(GovernancePrompt(processName))
		return nil
	},
}

func workerSessionName(parcelle, project, id string) string {
	return session.SanitizeName(fmt.Sprintf("%s--%s-%s", parcelle, project, id))
}

// randSuffix returns a short random hex string used to make worker/session
// names collision-proof even when the timestamp component repeats.
func randSuffix() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

func gitConfigValue(key string) string {
	out, err := exec.Command("git", "config", "--global", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func init() {
	spawnCmd.Flags().String("project", "", "Vigne/project name")
	spawnCmd.Flags().String("prompt", "", "Task prompt for the agent")
	spawnCmd.Flags().String("name", "", "Session name (auto-generated if omitted)")
	spawnCmd.Flags().String("target-branch", "", "MR target branch (default: auto-detected from repo)")
	spawnCmd.Flags().String("process", "", "Babysitter process definition name")
	spawnCmd.Flags().String("parcelle", "", "Parcelle (workstream) name — defaults to project name")
	spawnCmd.Flags().String("issue", "", "GitLab issue IID driving this work")
	spawnCmd.Flags().String("run-id", "", "Resume an existing babysitter run by ID")
	spawnCmd.Flags().String("args", "", "Process inputs as JSON")
	spawnCmd.Flags().Bool("force", false, "Spawn even if a live worker already exists for this run ID")
	spawnCmd.Flags().String("runtime", "", "Worker runtime: local (default) or singularity")
	spawnCmd.Flags().String("sif", "", "Singularity image path (runtime=singularity)")
	spawnCmd.Flags().StringArray("bind", nil, "Singularity --bind entry (repeatable; host[:container[:ro]])")
	spawnCmd.Flags().Bool("no-worktree", false, "Run in the project path without creating a git worktree (data/orchestration jobs)")
	spawnCmd.Flags().String("contract-id", "", "Mnemosyne contract ID — injects PINARD_CAPSULE_CONTRACT into the worker env")
	spawnCmd.Flags().Bool("no-capsule", false, "Skip capsule auto-detection; spawn on operator token even if the issue has a funded contract")
	spawnCmd.Flags().Bool("ephemeral", false, "Mark this worker as ephemeral (e.g. scheduled/one-shot); allows aoc gc to reap it when done")
	rootCmd.AddCommand(spawnCmd)

	governancePromptCmd.Flags().String("process", "", "Babysitter process name (required)")
	rootCmd.AddCommand(governancePromptCmd)
}
