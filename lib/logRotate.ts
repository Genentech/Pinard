// Size-capped rotation for the pi-extension's append-only log files
// (logs/conductor.log, logs/system.log, logs/nats-events.log, …). Mirrors
// the Go daemon's internal/logrotate policy: rotate on overflow, gzip the
// rotated backup, prune by count and age. Best-effort — logging must never
// throw for the caller.
import { existsSync, statSync, readFileSync, writeFileSync, appendFileSync, unlinkSync, renameSync } from "node:fs";
import { gzipSync } from "node:zlib";

export interface RotateOptions {
  maxSizeBytes?: number;
  maxBackups?: number;
  maxAgeDays?: number;
}

interface ResolvedOptions {
  maxSizeBytes: number;
  maxBackups: number;
  maxAgeDays: number;
}

const DEFAULTS: ResolvedOptions = {
  maxSizeBytes: 50 * 1024 * 1024,
  maxBackups: 5,
  maxAgeDays: 30,
};

function backupPath(path: string, n: number): string {
  return `${path}.${n}.gz`;
}

function pruneOldBackups(path: string, opts: ResolvedOptions): void {
  const maxAgeMs = opts.maxAgeDays * 24 * 60 * 60 * 1000;
  const now = Date.now();
  let n = 1;
  while (existsSync(backupPath(path, n))) {
    const p = backupPath(path, n);
    if (n > opts.maxBackups) {
      try { unlinkSync(p); } catch {}
    } else {
      try {
        if (now - statSync(p).mtimeMs > maxAgeMs) unlinkSync(p);
      } catch {}
    }
    n++;
  }
}

function rotate(path: string, opts: ResolvedOptions): number {
  const size = existsSync(path) ? statSync(path).size : 0;
  if (size === 0) return 0;

  for (let n = opts.maxBackups; n >= 1; n--) {
    const src = backupPath(path, n);
    if (!existsSync(src)) continue;
    if (n === opts.maxBackups) {
      try { unlinkSync(src); } catch {}
    } else {
      try { renameSync(src, backupPath(path, n + 1)); } catch {}
    }
  }

  const data = readFileSync(path);
  writeFileSync(backupPath(path, 1), gzipSync(data));
  unlinkSync(path);
  pruneOldBackups(path, opts);
  return size;
}

// appendRotating appends content to path, rotating (compress + shift
// backups + prune) first if the write would push the file past
// maxSizeBytes. Handles both steady-state growth and the one-off reclaim
// of a pre-existing oversized file (the first over-cap append rotates it).
export function appendRotating(path: string, content: string, opts?: RotateOptions): void {
  const cfg: ResolvedOptions = { ...DEFAULTS, ...opts };
  try {
    const currentSize = existsSync(path) ? statSync(path).size : 0;
    if (currentSize + Buffer.byteLength(content) > cfg.maxSizeBytes) {
      const reclaimed = rotate(path, cfg);
      if (reclaimed > 0) {
        console.error(`[log-rotate] rotated ${path} (${reclaimed} bytes archived)`);
      }
    }
    appendFileSync(path, content);
  } catch {
    // best-effort: a logging failure must never crash the caller
  }
}
