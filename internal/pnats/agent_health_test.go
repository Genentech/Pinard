package pnats

import (
	"testing"
	"time"
)

func TestDeriveAgentHealth(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	liveness := 5 * time.Minute

	t.Run("healthy active agent is neither errored nor stalled", func(t *testing.T) {
		rec := map[string]any{
			"tempo":            "active",
			"lastSeen":         now.Add(-1 * time.Minute).Format(time.RFC3339),
			"lastTransitionAt": now.Add(-1 * time.Minute).Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if h.Errored || h.Stalled {
			t.Fatalf("expected healthy, got %+v", h)
		}
	})

	t.Run("idle/blocked agent is not stalled even with an old transition", func(t *testing.T) {
		rec := map[string]any{
			"tempo":            "blocked",
			"lastSeen":         now.Add(-1 * time.Minute).Format(time.RFC3339),
			"lastTransitionAt": now.Add(-1 * time.Hour).Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if h.Stalled {
			t.Fatalf("blocked agent should never be stalled, got %+v", h)
		}
	})

	t.Run("errored tempo surfaces lastError", func(t *testing.T) {
		rec := map[string]any{
			"tempo":     "errored",
			"lastError": "401: proxy token expired",
			"lastSeen":  now.Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if !h.Errored || h.LastError != "401: proxy token expired" {
			t.Fatalf("expected errored with text, got %+v", h)
		}
		if h.Stalled {
			t.Fatalf("errored agent should not also be reported stalled: %+v", h)
		}
	})

	t.Run("active agent wedged past StallThreshold with fresh heartbeat is stalled", func(t *testing.T) {
		rec := map[string]any{
			"tempo":            "active",
			"lastSeen":         now.Add(-30 * time.Second).Format(time.RFC3339),
			"lastTransitionAt": now.Add(-StallThreshold - time.Minute).Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if !h.Stalled {
			t.Fatalf("expected stalled, got %+v", h)
		}
	})

	t.Run("active agent with stale heartbeat is not stalled (it's dead, not wedged)", func(t *testing.T) {
		rec := map[string]any{
			"tempo":            "active",
			"lastSeen":         now.Add(-1 * time.Hour).Format(time.RFC3339),
			"lastTransitionAt": now.Add(-StallThreshold - time.Minute).Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if h.Stalled {
			t.Fatalf("agent with stale heartbeat should not be flagged stalled: %+v", h)
		}
	})

	t.Run("missing lastTransitionAt does not crash and is not stalled", func(t *testing.T) {
		rec := map[string]any{
			"tempo":    "active",
			"lastSeen": now.Format(time.RFC3339),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if h.Stalled {
			t.Fatalf("no lastTransitionAt should not be flagged stalled: %+v", h)
		}
	})

	t.Run("compactions count is surfaced", func(t *testing.T) {
		rec := map[string]any{
			"tempo":       "blocked",
			"compactions": float64(3),
		}
		h := DeriveAgentHealth(rec, now, liveness)
		if h.Compactions != 3 {
			t.Fatalf("expected compactions=3, got %+v", h)
		}
	})
}
