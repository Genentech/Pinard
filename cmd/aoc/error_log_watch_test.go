package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/Genentech/pinard/internal/config"
)

func newTestErrorLogWatcher(t *testing.T) *errorLogWatcher {
	t.Helper()
	return &errorLogWatcher{
		Vignoble: &config.Vignoble{Name: "testv", Config: &config.VignobleConfig{}},
		logged:   make(map[string]string),
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(orig)
		log.SetFlags(origFlags)
	})
	return &buf
}

func TestErrorLogWatcher_LogsOnceForNewErroredAt(t *testing.T) {
	buf := captureLog(t)
	w := newTestErrorLogWatcher(t)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"length: max output tokens reached"}`))

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %q", len(lines), out)
	}
	if !strings.Contains(out, "agent=w1") || !strings.Contains(out, "length: max output tokens reached") {
		t.Fatalf("log line missing agent identity or lastError: %q", out)
	}
}

func TestErrorLogWatcher_SameErroredAtDoesNotRelog(t *testing.T) {
	buf := captureLog(t)
	w := newTestErrorLogWatcher(t)

	entry := `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"error: 401 Unauthorized"}`
	w.handle(putEntry("w1", entry))
	w.handle(putEntry("w1", entry))
	w.handle(putEntry("w1", entry))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line for a persistently errored agent, got %d: %q", len(lines), buf.String())
	}
}

func TestErrorLogWatcher_RecoveryThenNewErrorLogsAgain(t *testing.T) {
	buf := captureLog(t)
	w := newTestErrorLogWatcher(t)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"error: 401 Unauthorized"}`))
	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"active"}`))
	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T11:00:00Z","lastError":"length: max output tokens reached"}`))

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines (initial error + distinct error after recovery), got %d: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], "401 Unauthorized") {
		t.Fatalf("expected first line to contain the error-stopReason message: %q", lines[0])
	}
	if !strings.Contains(lines[1], "length: max output tokens reached") {
		t.Fatalf("expected second line to contain the length-stopReason message: %q", lines[1])
	}
}

func TestErrorLogWatcher_IgnoresOtherVignoble(t *testing.T) {
	buf := captureLog(t)
	w := newTestErrorLogWatcher(t)

	w.handle(putEntry("w1", `{"vignoble":"other","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"error: boom"}`))

	if buf.Len() != 0 {
		t.Fatalf("expected no log output for an agent in a different vignoble, got %q", buf.String())
	}
}

func TestErrorLogWatcher_DeleteForgetsState(t *testing.T) {
	buf := captureLog(t)
	w := newTestErrorLogWatcher(t)

	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"error: boom"}`))
	w.handle(delEntry("w1"))
	w.handle(putEntry("w1", `{"vignoble":"testv","name":"w1","tempo":"errored","erroredAt":"2026-09-25T10:00:00Z","lastError":"error: boom"}`))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected a KV delete to forget tracked state, allowing a re-observed erroredAt to log again; got %d lines: %q", len(lines), buf.String())
	}
}
