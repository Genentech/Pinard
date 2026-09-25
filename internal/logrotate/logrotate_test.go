package logrotate

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNewWriterRotatesOnOverflow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	cfg := Config{MaxSizeMB: 1, MaxBackups: 2, MaxAgeDays: 30, Compress: false}
	lj := NewWriter(path, cfg)
	defer lj.Close()

	line := bytes.Repeat([]byte("x"), 1024) // 1KB per write
	for i := 0; i < 1100; i++ {             // > 1MB total, should trigger rotation
		if _, err := lj.Write(append(line, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected a rotated backup alongside the live file, got %d entries: %v", len(entries), entries)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat current log: %v", err)
	}
	maxBytes := int64(cfg.MaxSizeMB) * 1024 * 1024
	if info.Size() >= maxBytes {
		t.Fatalf("current log file size %d did not reset below cap %d after rotation", info.Size(), maxBytes)
	}
}

func TestReclaimIfOversizedRotatesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.log")

	// Simulate a pre-existing oversized file (as if accumulated before
	// rotation was introduced).
	big := bytes.Repeat([]byte("y"), 2*1024*1024)
	if err := os.WriteFile(path, big, 0644); err != nil {
		t.Fatalf("seed oversized file: %v", err)
	}

	cfg := Config{MaxSizeMB: 1, MaxBackups: 2, MaxAgeDays: 30, Compress: true}
	lj := NewWriter(path, cfg)
	defer lj.Close()

	reclaimed, err := ReclaimIfOversized(lj, cfg)
	if err != nil {
		t.Fatalf("ReclaimIfOversized: %v", err)
	}
	if reclaimed != int64(len(big)) {
		t.Fatalf("expected reclaimed=%d, got %d", len(big), reclaimed)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat post-rotate: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected a fresh empty file after forced rotation, got size %d", info.Size())
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) < 2 {
		t.Fatalf("expected a compressed backup alongside the fresh file, got %d entries: %v", len(entries), entries)
	}
}

func TestReclaimIfOversizedNoopWhenUnderCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.log")
	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	cfg := DefaultConfig()
	lj := NewWriter(path, cfg)
	defer lj.Close()

	reclaimed, err := ReclaimIfOversized(lj, cfg)
	if err != nil {
		t.Fatalf("ReclaimIfOversized: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("expected no reclamation for a small file, got %d", reclaimed)
	}
}

func TestReclaimIfOversizedMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.log")

	cfg := DefaultConfig()
	lj := NewWriter(path, cfg)
	defer lj.Close()

	reclaimed, err := ReclaimIfOversized(lj, cfg)
	if err != nil {
		t.Fatalf("ReclaimIfOversized on missing file should not error: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("expected 0 reclaimed for missing file, got %d", reclaimed)
	}
}

func TestNewWriterAppendsAcrossReexecSimulation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reexec.log")
	cfg := Config{MaxSizeMB: 50, MaxBackups: 5, MaxAgeDays: 30, Compress: true}

	lj1 := NewWriter(path, cfg)
	if _, err := lj1.Write([]byte("first\n")); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	lj1.Close()

	// Simulate a fresh process image (syscall.Exec) constructing a brand new
	// writer against the same path.
	lj2 := NewWriter(path, cfg)
	defer lj2.Close()
	if _, err := lj2.Write([]byte("second\n")); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "first\nsecond\n" {
		t.Fatalf("expected appended content with no truncation/duplication, got %q", string(data))
	}
}
