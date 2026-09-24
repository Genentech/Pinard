package main

import (
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/watcher"
)

// BUG REGRESSION (#305): `aoc comment-mr` must always append the conductor
// marker so the mr-watcher forwards the note to the vendangeur despite the
// shared git-host identity.
func TestAppendConductorMarker(t *testing.T) {
	body := appendConductorMarker("LGTM")
	if !strings.HasPrefix(body, "LGTM") {
		t.Errorf("appendConductorMarker(%q) = %q, want it to start with the original body", "LGTM", body)
	}
	if !strings.Contains(body, watcher.ConductorMarker) {
		t.Errorf("appendConductorMarker(%q) = %q, want it to contain %q", "LGTM", body, watcher.ConductorMarker)
	}
}
