package main

import (
	"strings"
	"testing"
)

// TestRespawnCmd_ArgValidation exercises the parts of `aoc respawn` that run
// before any config/NATS I/O, so they're safe to unit test directly: exactly
// two positional args, and the issue number must parse as an int. The actual
// respawn logic (label clearing, reap, state reset, spawn) is covered by
// internal/watcher's RespawnIssue tests.
func TestRespawnCmd_ArgValidation(t *testing.T) {
	if err := respawnCmd.Args(respawnCmd, []string{"only-one-arg"}); err == nil {
		t.Error("expected an error for a single positional arg")
	}
	if err := respawnCmd.Args(respawnCmd, []string{"vigne", "42", "extra"}); err == nil {
		t.Error("expected an error for three positional args")
	}
	if err := respawnCmd.Args(respawnCmd, []string{"vigne", "42"}); err != nil {
		t.Errorf("expected no error for two positional args, got %v", err)
	}
}

func TestRespawnCmd_InvalidIssueNumber(t *testing.T) {
	err := respawnCmd.RunE(respawnCmd, []string{"my-vigne", "not-a-number"})
	if err == nil {
		t.Fatal("expected an error for a non-numeric issue argument")
	}
	if !strings.Contains(err.Error(), "invalid issue number") {
		t.Errorf("expected 'invalid issue number' error, got: %v", err)
	}
}

// TestRespawnCmd_ForceFlagRegistered guards against the --force flag (the
// open-MR guard's escape hatch, see internal/watcher's RespawnIssue) silently
// disappearing or being renamed.
func TestRespawnCmd_ForceFlagRegistered(t *testing.T) {
	flag := respawnCmd.Flags().Lookup("force")
	if flag == nil {
		t.Fatal("expected a --force flag on `aoc respawn`")
	}
	if flag.Value.Type() != "bool" {
		t.Errorf("expected --force to be a bool flag, got %s", flag.Value.Type())
	}
	if flag.DefValue != "false" {
		t.Errorf("expected --force to default to false, got %s", flag.DefValue)
	}
}
