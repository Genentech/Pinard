package main

import (
	"strings"
	"testing"
)

func TestValidateMRTitle(t *testing.T) {
	cases := []struct {
		name    string
		title   string
		wantErr bool
	}{
		{"fix bare", "fix: correct off-by-one in cron parser", false},
		{"feat bare", "feat: add webterm steer mode", false},
		{"fix scoped", "fix(ci): pin node version in pipeline", false},
		{"feat scoped", "feat(daemon): log abnormal turn ends", false},
		{"docs", "docs(PINARD.md): clarify MR workflow", true},
		{"chore", "chore: bump deps", true},
		{"refactor", "refactor: extract helper", true},
		{"ops", "ops: rotate logs", true},
		{"observability", "observability: log daemon-side record of abnormal turn ends", true},
		{"no prefix", "rotate aoc-daemon.log and pi-extension logs", true},
		{"empty subject", "fix:", true},
		{"empty subject scoped", "feat(docs): ", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMRTitle(tc.title)
			if tc.wantErr && err == nil {
				t.Fatalf("validateMRTitle(%q) = nil, want error", tc.title)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateMRTitle(%q) = %v, want nil", tc.title, err)
			}
			if tc.wantErr {
				msg := err.Error()
				for _, want := range []string{"fix:", "feat:", "fix(scope):", "feat(scope):"} {
					if !strings.Contains(msg, want) {
						t.Errorf("error message %q does not mention allowed form %q", msg, want)
					}
				}
			}
		})
	}
}
