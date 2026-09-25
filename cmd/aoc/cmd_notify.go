package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Genentech/pinard/internal/config"
	"github.com/Genentech/pinard/internal/logrotate"
	"github.com/Genentech/pinard/internal/pnats"
	"github.com/spf13/cobra"
)

var notifyCmd = &cobra.Command{
	Use:   "notify <message>",
	Short: "Send a notification to the conductor",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		message := strings.Join(args, " ")

		creds, err := config.LoadCredentials()
		if err != nil {
			return fmt.Errorf("load credentials: %w", err)
		}

		vignoble := os.Getenv("NATS_VIGNOBLE")
		if vignoble == "" {
			vignoble = "default"
		}

		// Parcelle-scope the notification so it reaches the owning parcelle's
		// maître. Workers always run with BABYSITTER_PARCELLE set; an explicit
		// --parcelle wins. Empty parcelle → vignoble-level channel (régisseur).
		parcelle, _ := cmd.Flags().GetString("parcelle")
		if parcelle == "" {
			parcelle = os.Getenv("BABYSITTER_PARCELLE")
		}

		// Publish to NATS
		nc := pnats.NewClient(creds)
		defer nc.Close()

		subject := pnats.NotificationsSubject(vignoble, parcelle)
		payload := map[string]string{
			"message":   message,
			"timestamp": time.Now().Format(time.RFC3339),
		}

		if err := nc.Publish(subject, payload); err != nil {
			fmt.Fprintf(os.Stderr, "[nats] publish error: %v\n", err)
		}

		// Log to file — prefer per-vignoble state dir so notifications stay
		// scoped to the vignoble and get_notifications can read the right file.
		var logFile string
		if v, err := config.ResolveVignoble(); err == nil {
			logFile = filepath.Join(v.StateDir, "notifications.log")
		} else {
			// No vignoble context — fall back to global dir (standalone workers)
			stateDir := os.Getenv("AOC_STATE_DIR")
			if stateDir == "" {
				home, _ := os.UserHomeDir()
				stateDir = filepath.Join(home, ".config", "aoc")
			}
			logFile = filepath.Join(stateDir, "notifications.log")
		}
		os.MkdirAll(filepath.Dir(logFile), 0755)
		notifyLogCfg := logrotate.Config{MaxSizeMB: 20, MaxBackups: 3, MaxAgeDays: 30, Compress: true}
		lw := logrotate.NewWriter(logFile, notifyLogCfg)
		if _, err := logrotate.ReclaimIfOversized(lw, notifyLogCfg); err != nil {
			fmt.Fprintf(os.Stderr, "[notify] reclaim %s failed (non-fatal): %v\n", logFile, err)
		}
		fmt.Fprintf(lw, "%s %s\n", time.Now().Format(time.RFC3339), message)
		lw.Close()

		fmt.Printf("Notified: %s\n", message)
		return nil
	},
}

func init() {
	notifyCmd.Flags().String("parcelle", "", "Parcelle to scope the notification to (defaults to $BABYSITTER_PARCELLE)")
	rootCmd.AddCommand(notifyCmd)
}
