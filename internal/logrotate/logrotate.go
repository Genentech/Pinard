// Package logrotate provides a size-capped, in-process rotating log writer
// for daemon-side log files. It wraps lumberjack so that a long-running
// process (or a fresh process image after syscall.Exec re-exec) never grows
// a log file unbounded: it appends to the existing file if under the size
// cap, rotates+compresses on overflow, and prunes old backups by count/age.
package logrotate

import (
	"fmt"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config controls rotation policy for a single log file.
type Config struct {
	MaxSizeMB  int  // rotate once the file reaches this size
	MaxBackups int  // number of old, compressed files to retain
	MaxAgeDays int  // prune backups older than this many days
	Compress   bool // gzip rotated backups
}

// DefaultConfig returns the standard policy: 50MB per file, 5 backups,
// pruned after 30 days, compressed.
func DefaultConfig() Config {
	return Config{
		MaxSizeMB:  50,
		MaxBackups: 5,
		MaxAgeDays: 30,
		Compress:   true,
	}
}

// NewWriter returns a rotating writer targeting path. Writes append to the
// existing file (no truncation) when it exists and is under the size cap —
// safe to call repeatedly against the same path (e.g. across a daemon
// re-exec), since lumberjack opens in append mode and only rotates on
// overflow.
func NewWriter(path string, cfg Config) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxBackups,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
	}
}

// ReclaimIfOversized forces an immediate rotation of an existing, already
// oversized log file: on first use against a file that predates this
// policy (e.g. a multi-hundred-MB log accumulated before rotation existed),
// this archives+compresses the current content and starts a fresh file,
// applying MaxBackups/MaxAge pruning in the same pass. Returns the size (in
// bytes) of the file that was rotated away, or 0 if no rotation was needed.
func ReclaimIfOversized(lj *lumberjack.Logger, cfg Config) (int64, error) {
	info, err := os.Stat(lj.Filename)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("stat %s: %w", lj.Filename, err)
	}
	maxBytes := int64(cfg.MaxSizeMB) * 1024 * 1024
	if info.Size() < maxBytes {
		return 0, nil
	}
	reclaimed := info.Size()
	if err := lj.Rotate(); err != nil {
		return 0, fmt.Errorf("rotate %s: %w", lj.Filename, err)
	}
	return reclaimed, nil
}
