package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/spf13/cobra"
)

// daemonPIDFile returns the path to the per-vignoble daemon PID file.
func daemonPIDFile(vb *config.Vignoble) string {
	return filepath.Join(vb.StateDir, "daemon.pid")
}

// readPIDFile returns the PID stored in path, or 0 if absent/unparseable.
func readPIDFile(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// processAlive reports whether a process with the given PID exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// On Linux, signal 0 probes existence without delivering a signal.
	return syscall.Kill(pid, 0) == nil
}

// loadEnvFile parses a simple KEY=VALUE env file (the one systemd used as
// EnvironmentFile) and returns the entries as "KEY=VALUE" strings. Supports
// optional `export ` prefixes, surrounding quotes, comments, and blank lines.
func loadEnvFile(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		out = append(out, key+"="+val)
	}
	return out
}

// nodeVersionOK reports whether the node binary at binDir/node meets the minimum
// version requirement (major.minor.patch). Returns false if the binary cannot be
// executed or the version cannot be parsed.
func nodeVersionOK(nodebin string, minMaj, minMin, minPatch int) bool {
	out, err := exec.Command(nodebin, "-e", "process.stdout.write(process.versions.node)").Output()
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), ".", 3)
	if len(parts) != 3 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	patch, err3 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	if maj != minMaj {
		return maj > minMaj
	}
	if min != minMin {
		return min > minMin
	}
	return patch >= minPatch
}

// buildDaemonPATH constructs the PATH the daemon needs to spawn workers:
// ~/.local/bin, the nvm node bin that holds `pi`, plus standard system paths.
// Respects PINARD_NODE (absolute path to node binary) if set. Otherwise scans
// nvm versions and prefers the first that has `pi` AND meets >=22.19.0; falls
// back to the first with `pi` when no qualifying version is found.
func buildDaemonPATH() string {
	home, _ := os.UserHomeDir()
	parts := []string{filepath.Join(home, ".local", "bin")}

	// PINARD_NODE wins: prepend its parent dir so the daemon and all children
	// (bin/pinard --worker) resolve the correct node.
	if pinardNode := os.Getenv("PINARD_NODE"); pinardNode != "" {
		if info, err := os.Stat(pinardNode); err == nil && !info.IsDir() {
			parts = append(parts, filepath.Dir(pinardNode))
		}
	} else {
		// Scan nvm versions: prefer first that has `pi` AND meets >=22.19.0.
		nvmDir := filepath.Join(home, ".nvm", "versions", "node")
		if entries, err := os.ReadDir(nvmDir); err == nil {
			var fallback string
			for _, e := range entries {
				binDir := filepath.Join(nvmDir, e.Name(), "bin")
				if _, err := os.Stat(filepath.Join(binDir, "pi")); err != nil {
					continue
				}
				if fallback == "" {
					fallback = binDir
				}
				if nodeVersionOK(filepath.Join(binDir, "node"), 22, 19, 0) {
					parts = append(parts, binDir)
					fallback = ""
					break
				}
			}
			if fallback != "" {
				// No qualifying version found; warn and use whatever has pi.
				fmt.Fprintf(os.Stderr, "aoc daemon: warning: no nvm node >=22.19.0 with pi found — workers may crash. Set PINARD_NODE or run: nvm install 22\n")
				parts = append(parts, fallback)
			}
		}
	}

	parts = append(parts,
		"/home/linuxbrew/.linuxbrew/bin",
		"/usr/local/go/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
	)
	return strings.Join(parts, ":")
}

// daemonChildEnv builds the environment for a detached daemon child: the current
// environment, overlaid with ~/.config/pinard/env secrets, an augmented PATH,
// and the vignoble config pointers so the child resolves the right vignoble
// regardless of cwd.
func daemonChildEnv(vb *config.Vignoble) []string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			env[kv[:eq]] = kv[eq+1:]
		}
	}

	home, _ := os.UserHomeDir()
	for _, kv := range loadEnvFile(filepath.Join(home, ".config", "pinard", "env")) {
		if eq := strings.IndexByte(kv, '='); eq > 0 {
			env[kv[:eq]] = kv[eq+1:]
		}
	}

	env["PATH"] = buildDaemonPATH()
	env["AOC_CONFIG"] = vb.ConfigPath
	env["AOC_SCHEDULES"] = filepath.Join(vb.Path, "schedules.yaml")

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the daemon as a detached background process",
	RunE: func(cmd *cobra.Command, args []string) error {
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}

		pidFile := daemonPIDFile(vb)
		if pid := readPIDFile(pidFile); processAlive(pid) {
			fmt.Printf("Daemon already running (pid %d)\n", pid)
			return nil
		}

		os.MkdirAll(vb.StateDir, 0755)
		os.MkdirAll(vb.LogDir, 0755)
		logPath := filepath.Join(vb.LogDir, "aoc-daemon.log")
		// The daemon child manages its own rotating log file internally (see
		// daemon.go — log.SetOutput onto a logrotate writer for this same path).
		// Redirect fd-level stdout/stderr to /dev/null here rather than the raw
		// log file: keeping a second, never-rotated fd open on that path would
		// defeat the point, and every daemon log line already goes through the
		// stdlib log package. Only output emitted before the child installs its
		// own log.SetOutput (i.e. before daemonCmd.RunE runs) is lost — a rare,
		// tiny edge case (misconfiguration crashes before the vignoble resolves).
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", os.DevNull, err)
		}
		defer devNull.Close()

		child := exec.Command(selfPath(), "daemon")
		child.Dir = vb.Path
		child.Env = daemonChildEnv(vb)
		child.Stdout = devNull
		child.Stderr = devNull
		child.Stdin = nil
		// Detach into its own session so it survives the launching shell.
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		if err := child.Start(); err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}
		// Capture the PID before Release() — Release() resets it to -1.
		childPID := child.Process.Pid
		// The foreground daemon also writes this file on startup; write it here
		// too so `start` reports a valid PID immediately.
		os.WriteFile(pidFile, []byte(strconv.Itoa(childPID)), 0644)
		// Release the child so it keeps running after we exit.
		child.Process.Release()

		fmt.Printf("Daemon started for vignoble %s (pid %d)\n", vb.Name, childPID)
		fmt.Printf("  logs: %s\n", logPath)
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pidFile := daemonPIDFile(vb)
		pid := readPIDFile(pidFile)
		if !processAlive(pid) {
			fmt.Println("Daemon not running")
			os.Remove(pidFile)
			return nil
		}

		syscall.Kill(pid, syscall.SIGTERM)
		// Wait up to 5s for graceful shutdown.
		for i := 0; i < 50; i++ {
			if !processAlive(pid) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if processAlive(pid) {
			fmt.Printf("Daemon (pid %d) did not stop after SIGTERM\n", pid)
			return fmt.Errorf("daemon still running")
		}
		os.Remove(pidFile)
		fmt.Printf("Daemon stopped (pid %d)\n", pid)
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := daemonStopCmd.RunE(cmd, args); err != nil {
			return err
		}
		return daemonStartCmd.RunE(cmd, args)
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report whether the daemon is running",
	RunE: func(cmd *cobra.Command, args []string) error {
		vb, err := config.ResolveVignoble()
		if err != nil {
			return err
		}
		pid := readPIDFile(daemonPIDFile(vb))
		if processAlive(pid) {
			fmt.Printf("Daemon: active (pid %d)\n", pid)
		} else {
			fmt.Println("Daemon: inactive")
		}
		return nil
	},
}

func init() {
	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonRestartCmd, daemonStatusCmd)
}
