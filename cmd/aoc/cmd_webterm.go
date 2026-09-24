package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/Genentech/pinard/internal/webterm"
	"github.com/creack/pty"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

// resolveVignobleName returns the vignoble name for webterm subjects, from
// --vignoble-name, NATS_VIGNOBLE, or a resolvable vignoble directory (in that
// order). Standalone/HPC hosts have no vignoble dir, so the flag/env wins.
func resolveVignobleName(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString("vignoble-name"); v != "" {
		return v
	}
	if v := os.Getenv("NATS_VIGNOBLE"); v != "" {
		return v
	}
	if vb, err := config.ResolveVignoble(); err == nil {
		return vb.Name
	}
	return ""
}

var webtermResponderCmd = &cobra.Command{
	Use:   "webterm-responder",
	Short: "Run the web-terminal responder (streams local tmux targets over NATS)",
	Long: "Serves read-only browser terminal views for local tmux sessions on this host.\n" +
		"The pinard host runs this in-process via the daemon; use this command on\n" +
		"standalone/HPC worker hosts. Requires webterm.grant_secret in credentials.",
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		if !creds.WebtermResponderEnabled() {
			return fmt.Errorf("webterm responder not configured: set webterm.grant_secret (or grant_secret_env)")
		}
		vignoble := resolveVignobleName(cmd)
		if vignoble == "" {
			return fmt.Errorf("could not resolve vignoble name (use --vignoble-name or set NATS_VIGNOBLE)")
		}

		nc := pnats.NewClient(creds)
		if err := nc.Connect(); err != nil {
			return err
		}
		defer nc.Close()

		// Publish this vignoble's owner for gateway operator authorization (D7).
		if err := webterm.PublishOwner(pnats.NewKV(nc), vignoble, creds.WebtermOwner()); err != nil {
			log.Printf("[webterm] publish owner failed: %v", err)
		}

		resp := &webterm.Responder{
			NC:          nc.Conn(),
			Vignoble:    vignoble,
			GrantSecret: creds.WebtermGrantSecret(),
			MaxViewers:  creds.WebtermMaxViewers(),
			IdleTimeout: creds.WebtermIdleTimeout(),
			KV:          pnats.NewKV(nc),
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		log.Printf("[webterm] responder starting (vignoble=%s)", vignoble)
		return resp.Run(ctx)
	},
}

var webtermLinkCmd = &cobra.Command{
	Use:   "webterm-link",
	Short: "Print a read-only terminal link for a tmux target (unsigned when SSO auth is on, else signed+expiring)",
	RunE: func(cmd *cobra.Command, args []string) error {
		target, _ := cmd.Flags().GetString("target")
		if target == "" {
			return fmt.Errorf("--target is required")
		}
		auto, _ := cmd.Flags().GetBool("auto")
		creds, err := config.LoadCredentials()
		// --auto: silently print nothing (exit 0) when link posting is not enabled,
		// so automated callers (e.g. the babysitter "Vendangeur attached" comment)
		// can invoke unconditionally and append a link only when one exists.
		if auto {
			if err != nil || !creds.WebtermEnabled() || !creds.WebtermPostLinks() {
				return nil
			}
		} else {
			if err != nil {
				return err
			}
			if !creds.WebtermEnabled() {
				return fmt.Errorf("webterm not configured: need webterm.base_url + link_secret + grant_secret")
			}
		}
		vignoble := resolveVignobleName(cmd)
		if vignoble == "" {
			if auto {
				return nil
			}
			return fmt.Errorf("could not resolve vignoble (use --vignoble-name or set NATS_VIGNOBLE)")
		}
		// Mirror `aoc track_mr`: with Cognito SSO enabled, emit an UNSIGNED link (no
		// bearer in the URL; the gateway grants only SSO-authenticated operators).
		// Without auth, fall back to a signed, expiring link.
		if creds.WebtermAuthEnabled() {
			fmt.Println(webterm.BuildUnsignedLink(creds.WebtermBaseURL(), vignoble, target))
			return nil
		}
		ttl := creds.WebtermLinkTTL()
		if v, _ := cmd.Flags().GetDuration("ttl"); v > 0 {
			ttl = v
		}
		exp := time.Now().Add(ttl)
		fmt.Println(webterm.BuildLink(creds.WebtermBaseURL(), vignoble, target, exp, creds.WebtermLinkSecret()))
		return nil
	},
}

// webtermWorkerResponderCmd is the worker-self-served responder: it bridges the
// worker's own pi PTY directly over NATS using the same grant-gated protocol as
// the host Responder (TmuxBackend), without requiring tmux. This is the
// daemon-less / HPC / Singularity --containall path.
//
// The command reads from the PTY master fd number passed via --pty-fd (the
// caller — bin/pinard --worker — opens a PTY pair and passes the master fd).
// It subscribes to ReqSubject, verifies grants, and serves each viewer via
// per-viewer OutSubject/InSubject/CtlSubject/EvtSubject.
var webtermWorkerResponderCmd = &cobra.Command{
	Use:   "webterm-worker-responder",
	Short: "Run the grant-gated worker PTY responder (daemon-less / HPC path)",
	Long: "Serves the worker's own PTY over NATS using the same grant-gated protocol as\n" +
		"webterm-responder, but without tmux. Called by bin/pinard --worker before\n" +
		"exec'ing pi on daemon-less / HPC / Singularity hosts.\n\n" +
		"Requires --session-name and --pty-fd; webterm.grant_secret in credentials.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ptyFD, _ := cmd.Flags().GetInt("pty-fd")
		sessionName, _ := cmd.Flags().GetString("session-name")
		if ptyFD < 0 {
			return fmt.Errorf("--pty-fd is required")
		}
		if sessionName == "" {
			return fmt.Errorf("--session-name is required")
		}

		creds, err := config.LoadCredentials()
		if err != nil {
			return err
		}
		if !creds.WebtermResponderEnabled() {
			// Not configured — silently exit. The worker still runs without a
			// responder; the feature is opt-in via grant_secret.
			return nil
		}
		vignoble := resolveVignobleName(cmd)
		if vignoble == "" {
			return fmt.Errorf("could not resolve vignoble name (use --vignoble-name or set NATS_VIGNOBLE)")
		}

		ptyFile := os.NewFile(uintptr(ptyFD), "pty-master")
		if ptyFile == nil {
			return fmt.Errorf("could not open pty fd %d", ptyFD)
		}

		nc := pnats.NewClient(creds)
		if err := nc.Connect(); err != nil {
			return err
		}
		defer nc.Close()

		backend := &webterm.ProcessBackend{
			Target: sessionName,
			PTY:    ptyFile,
			Setsize: func(cols, rows int) {
				_ = pty.Setsize(ptyFile, &pty.Winsize{
					Cols: uint16(cols),
					Rows: uint16(rows),
				})
			},
		}

		resp := &webterm.Responder{
			NC:          nc.Conn(),
			Vignoble:    vignoble,
			GrantSecret: creds.WebtermGrantSecret(),
			MaxViewers:  creds.WebtermMaxViewers(),
			IdleTimeout: creds.WebtermIdleTimeout(),
			Backend:     backend,
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		log.Printf("[webterm] worker responder starting (vignoble=%s session=%s)", vignoble, sessionName)
		return resp.Run(ctx)
	},
}

// agentLivenessThreshold mirrors the value in internal/webterm/gateway.go.
const agentLivenessThreshold = 5 * time.Minute

// localTmuxSessions returns the set of tmux session names on the local tmux
// server, using the standard pinard socket (pinard-<vignoble>). Falls back to
// the default server if the socket-based query fails.
func localTmuxSessions(vignoble string) map[string]bool {
	socket := "pinard-" + vignoble
	out, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		// Try default server as fallback (non-pinard or test environments).
		out, err = exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
		if err != nil {
			return map[string]bool{}
		}
	}
	result := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			result[line] = true
		}
	}
	return result
}

var webtermDoctorCmd = &cobra.Command{
	Use:   "webterm-doctor [vignoble]",
	Short: "Diagnose /sessions visibility: show per-agent include/exclude reasoning from pinard-agents KV",
	Long: "Connects to NATS, reads all records from the pinard-agents KV bucket, and prints\n" +
		"per-agent include/exclude reasoning matching the logic buildIndex uses in the gateway.\n" +
		"Useful for diagnosing why a worker does not appear in /sessions.",
	RunE: func(cmd *cobra.Command, args []string) error {
		creds, err := config.LoadCredentials()
		if err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}

		vignoble, _ := cmd.Flags().GetString("vignoble-name")
		if vignoble == "" && len(args) > 0 {
			vignoble = args[0]
		}
		if vignoble == "" {
			vignoble = resolveVignobleName(cmd)
		}
		if vignoble == "" {
			return fmt.Errorf("vignoble name required: pass as argument or use --vignoble-name / NATS_VIGNOBLE")
		}

		nc := pnats.NewClient(creds)
		if err := nc.Connect(); err != nil {
			return fmt.Errorf("NATS connect: %w", err)
		}
		defer nc.Close()

		kv := pnats.NewKV(nc)
		keys, err := kv.Keys("pinard-agents")
		if err != nil {
			return fmt.Errorf("KV list pinard-agents: %w", err)
		}

		if len(keys) == 0 {
			fmt.Printf("pinard-agents KV bucket is empty — no workers have registered for any vignoble.\n")
			return nil
		}

		localSessions := localTmuxSessions(vignoble)
		now := time.Now()

		fmt.Printf("Vignoble: %s\n", vignoble)
		fmt.Printf("Local tmux sessions (%d): %s\n\n", len(localSessions), func() string {
			if len(localSessions) == 0 {
				return "(none)"
			}
			names := make([]string, 0, len(localSessions))
			for n := range localSessions {
				names = append(names, n)
			}
			return strings.Join(names, ", ")
		}())
		fmt.Printf("%-9s  %-40s  %s\n", "VERDICT", "KEY", "REASON")
		fmt.Println(strings.Repeat("-", 90))

		var skippedTombstones int
		for _, k := range keys {
			rec, err := kv.Get("pinard-agents", k)
			if err != nil {
				if errors.Is(err, nats.ErrKeyNotFound) {
					skippedTombstones++
					continue
				}
				fmt.Printf("%-9s  %-40s  %s\n", "ERROR", k, fmt.Sprintf("KV get failed: %v", err))
				continue
			}
			if rec == nil {
				fmt.Printf("%-9s  %-40s  %s\n", "ERROR", k, "KV get returned nil")
				continue
			}

			name, _ := rec["name"].(string)
			recVig, _ := rec["vignoble"].(string)
			lastSeenStr, _ := rec["lastSeen"].(string)
			isStandalone, _ := rec["standalone"].(bool)
			state, _ := rec["state"].(string)
			tempo, _ := rec["tempo"].(string)
			host, _ := rec["host"].(string)
			build, _ := rec["build"].(string)
			if build == "" {
				build = "unknown"
			}

			// Check: vignoble match
			if recVig != vignoble {
				fmt.Printf("%-9s  %-40s  vignoble mismatch: record=%q want=%q\n", "EXCLUDE", k, recVig, vignoble)
				continue
			}

			// Check: name present
			if name == "" {
				fmt.Printf("%-9s  %-40s  name field is empty\n", "EXCLUDE", k)
				continue
			}

			// Check: local session (would appear via tmux listing, not KV scan)
			if localSessions[name] {
				fmt.Printf("%-9s  %-40s  local tmux session (listed via tmux, not KV) build=%s\n", "LOCAL", k, build)
				continue
			}

			// Check: lastSeen present
			if lastSeenStr == "" {
				fmt.Printf("%-9s  %-40s  lastSeen absent — no heartbeat received\n", "EXCLUDE", k)
				continue
			}

			// Check: freshness
			lastSeenAt, parseErr := time.Parse(time.RFC3339, lastSeenStr)
			if parseErr != nil {
				fmt.Printf("%-9s  %-40s  lastSeen unparseable: %q\n", "EXCLUDE", k, lastSeenStr)
				continue
			}
			age := now.Sub(lastSeenAt).Round(time.Second)
			if age > agentLivenessThreshold {
				fmt.Printf("%-9s  %-40s  stale: lastSeen=%s ago (threshold=%s)\n", "EXCLUDE", k, age, agentLivenessThreshold)
				continue
			}

			// INCLUDE: determine display role
			role := "remote"
			if !isStandalone {
				role = "unreachable-local"
			}
			fmt.Printf("%-9s  %-40s  name=%s role=%s state=%s/%s host=%s build=%s age=%s\n",
				"INCLUDE", k, name, role, state, tempo, host, build, age)
		}
		if skippedTombstones > 0 {
			fmt.Printf("(skipped %d deleted/tombstoned keys)\n", skippedTombstones)
		}
		return nil
	},
}

func init() {
	webtermResponderCmd.Flags().String("vignoble-name", "", "Vignoble name (NATS namespace); defaults to NATS_VIGNOBLE or the resolved vignoble")
	rootCmd.AddCommand(webtermResponderCmd)

	webtermWorkerResponderCmd.Flags().String("vignoble-name", "", "Vignoble name (NATS namespace); defaults to NATS_VIGNOBLE or the resolved vignoble")
	webtermWorkerResponderCmd.Flags().Int("pty-fd", -1, "PTY master file descriptor (opened by the caller)")
	webtermWorkerResponderCmd.Flags().String("session-name", "", "Session name this responder answers for")
	rootCmd.AddCommand(webtermWorkerResponderCmd)

	webtermLinkCmd.Flags().String("target", "", "tmux target (session name)")
	webtermLinkCmd.Flags().String("vignoble-name", "", "Vignoble name; defaults to NATS_VIGNOBLE or the resolved vignoble")
	webtermLinkCmd.Flags().Duration("ttl", 0, "link lifetime (overrides webterm.link_ttl)")
	webtermLinkCmd.Flags().Bool("auto", false, "Silently print nothing (exit 0) when webterm/post_links is disabled; for automated callers")
	rootCmd.AddCommand(webtermLinkCmd)

	webtermDoctorCmd.Flags().String("vignoble-name", "", "Vignoble name; defaults to first positional arg, NATS_VIGNOBLE, or the resolved vignoble")
	rootCmd.AddCommand(webtermDoctorCmd)
}
