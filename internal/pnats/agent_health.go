package pnats

import (
	"os"
	"strconv"
	"time"
)

// StallThreshold is how long an agent's state/tempo/step must remain unchanged
// (while its heartbeat stays fresh) before it is reported as "stalled" (#323).
// Overridable via PINARD_STALL_MINUTES for operators tuning sensitivity.
var StallThreshold = defaultStallThreshold()

func defaultStallThreshold() time.Duration {
	if v := os.Getenv("PINARD_STALL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 15 * time.Minute
}

// AgentHealth is the derived error/stall status of an agent, computed from its
// pinard-agents KV record. It never mutates the record; consumers (aoc status,
// webterm-doctor, the webterm gateway, list_workers) use it to decide what to
// print — see issue #323.
type AgentHealth struct {
	Errored     bool
	LastError   string
	Stalled     bool
	Compactions int
}

// DeriveAgentHealth inspects a pinard-agents KV record (as decoded by
// pnats.KV.Get) and reports whether the agent is in an error state or appears
// stalled.
//
// Errored reflects tempo == "errored", set by the worker on turn_end when
// pi's stopReason is "error" or the max-output-token "length" case (see
// pi-extension/worker/index.ts). Normal stop reasons ("stop", "toolUse") and
// the deliberate interrupt-channel "aborted" case are never flagged.
//
// Stalled means the agent claims to be actively working (tempo == "active"),
// its heartbeat is fresh (lastSeen within livenessThreshold), but its last
// real state/tempo/step transition (lastTransitionAt) is older than
// StallThreshold. The worker only advances lastTransitionAt when something
// actually changes, so periodic heartbeat republishing (identical state/tempo
// every 60s) does not reset it — this is what lets a wedged turn be told apart
// from one that is genuinely making progress.
func DeriveAgentHealth(rec map[string]any, now time.Time, livenessThreshold time.Duration) AgentHealth {
	var h AgentHealth

	tempo, _ := rec["tempo"].(string)
	if tempo == "errored" {
		h.Errored = true
	}
	if s, ok := rec["lastError"].(string); ok {
		h.LastError = s
	}
	if n, ok := rec["compactions"].(float64); ok {
		h.Compactions = int(n)
	}

	if tempo == "active" {
		lastSeenStr, _ := rec["lastSeen"].(string)
		lastSeenAt, err := time.Parse(time.RFC3339, lastSeenStr)
		fresh := err == nil && now.Sub(lastSeenAt) <= livenessThreshold
		if fresh {
			transStr, _ := rec["lastTransitionAt"].(string)
			if transAt, err := time.Parse(time.RFC3339, transStr); err == nil {
				if now.Sub(transAt) > StallThreshold {
					h.Stalled = true
				}
			}
		}
	}

	return h
}
