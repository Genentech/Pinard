package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var createUserCmd = &cobra.Command{
	Use:   "create-user",
	Short: "Create a NATS account/user scoped to a vignoble (admin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		name, _ := cmd.Flags().GetString("name")
		vignoble, _ := cmd.Flags().GetString("vignoble")
		natsURL, _ := cmd.Flags().GetString("nats-url")

		if name == "" || vignoble == "" {
			return fmt.Errorf("--name and --vignoble are required")
		}

		if _, err := exec.LookPath("nsc"); err != nil {
			return fmt.Errorf("`aoc create-user` only applies to self-hosted nsc/JWT NATS deployments.\n" +
				"Config-account deployments (Helm/Vault, e.g. production pinard-nats) use a single\n" +
				"pre-configured account with unrestricted permissions — this command is a no-op for them.\n" +
				"If you are running a self-hosted nsc/JWT NATS, install nsc first:\n" +
				"  curl -L https://raw.githubusercontent.com/nats-io/nsc/master/install.py | python")
		}

		fmt.Printf("Creating NATS account '%s' scoped to vignoble '%s'...\n", name, vignoble)

		// Create account
		if err := nscRun("describe", "account", "-n", name); err != nil {
			nscRun("add", "account", "-n", name)
			fmt.Printf("  Account '%s' created\n", name)
		} else {
			fmt.Printf("  Account '%s' already exists\n", name)
		}

		// Full allow-list applied on both create and edit, so the grant is
		// idempotent: re-running against an already-provisioned account updates
		// the permissions (e.g. to add $KV.pinard-agents.> to live remote workers).
		allowList := []string{
			"--allow-pub", fmt.Sprintf("pinard.%s.>", vignoble),
			"--allow-sub", fmt.Sprintf("pinard.%s.>", vignoble),
			"--allow-pubsub", "_INBOX.>",
			"--allow-pubsub", "$JS.>",
			"--allow-pub", "$KV.pinard-agents.>",
			"--allow-sub", "$KV.pinard-agents.>",
		}
		permDesc := fmt.Sprintf("pinard.%s.>, _INBOX.>, $JS.>, $KV.pinard-agents.>", vignoble)

		// Create user
		if err := nscRun("describe", "user", "-a", name, "-n", "pinard"); err != nil {
			nscRun(append([]string{"add", "user", "-a", name, "-n", "pinard"}, allowList...)...)
			fmt.Printf("  User 'pinard' created with permissions: %s\n", permDesc)
		} else {
			// User exists — edit in place to ensure all permissions are current.
			nscRun(append([]string{"edit", "user", "-a", name, "-n", "pinard"}, allowList...)...)
			fmt.Printf("  User 'pinard' already exists — permissions updated: %s\n", permDesc)
		}

		// Push
		fmt.Printf("  Pushing account to %s...\n", natsURL)
		if err := nscRun("push", "-a", name, "-u", natsURL); err != nil {
			fmt.Printf("  Warning: push failed\n")
		} else {
			fmt.Printf("  Account pushed\n")
		}

		// Find creds
		env := nscOutput("env")
		operator := ""
		for _, line := range strings.Split(env, "\n") {
			if strings.Contains(line, "operator") {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					operator = fields[len(fields)-1]
				}
			}
		}

		home, _ := os.UserHomeDir()
		credsPath := fmt.Sprintf("%s/.nkeys/creds/%s/%s/pinard.creds", home, operator, name)
		if _, err := os.Stat(credsPath); err == nil {
			fmt.Printf("\nCredentials: %s\n", credsPath)
			fmt.Printf("\nUser config (~/.config/pinard/credentials.yaml):\n\n")
			fmt.Printf("  nats:\n    url: %s\n    credentials: %s\n", natsURL, credsPath)
		}

		return nil
	},
}

func nscRun(args ...string) error {
	cmd := exec.Command("nsc", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func nscOutput(args ...string) string {
	out, _ := exec.Command("nsc", args...).Output()
	return string(out)
}

func init() {
	createUserCmd.Flags().String("name", "", "Username / account name")
	createUserCmd.Flags().String("vignoble", "", "Vignoble name for permissions")
	createUserCmd.Flags().String("nats-url", "nats://localhost:4222", "NATS server URL for push")
	rootCmd.AddCommand(createUserCmd)
}
