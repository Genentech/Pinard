import { describe, it, expect, beforeEach, afterEach } from "vitest";
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync, statSync } from "node:fs";
import { gunzipSync } from "node:zlib";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { appendRotating } from "@pinard/log-rotate";

describe("appendRotating", () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "log-rotate-test-"));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("appends without rotating when under the size cap", () => {
    const path = join(dir, "test.log");
    appendRotating(path, "line one\n", { maxSizeBytes: 1024 });
    appendRotating(path, "line two\n", { maxSizeBytes: 1024 });
    expect(readFileSync(path, "utf8")).toBe("line one\nline two\n");
    expect(existsSync(`${path}.1.gz`)).toBe(false);
  });

  it("rotates and gzip-compresses when a write would exceed the cap", () => {
    const path = join(dir, "test.log");
    writeFileSync(path, "x".repeat(2000));

    appendRotating(path, "new line\n", { maxSizeBytes: 1024 });

    expect(existsSync(`${path}.1.gz`)).toBe(true);
    const decompressed = gunzipSync(readFileSync(`${path}.1.gz`)).toString("utf8");
    expect(decompressed).toBe("x".repeat(2000));

    expect(readFileSync(path, "utf8")).toBe("new line\n");
  });

  it("shifts numbered backups and prunes beyond maxBackups", () => {
    const path = join(dir, "test.log");
    const opts = { maxSizeBytes: 100, maxBackups: 2 };

    // Trigger three rotations in sequence.
    writeFileSync(path, "a".repeat(200));
    appendRotating(path, "1\n", opts);
    writeFileSync(path, "b".repeat(200));
    appendRotating(path, "2\n", opts);
    writeFileSync(path, "c".repeat(200));
    appendRotating(path, "3\n", opts);

    // Only maxBackups (2) numbered backups should remain.
    expect(existsSync(`${path}.1.gz`)).toBe(true);
    expect(existsSync(`${path}.2.gz`)).toBe(true);
    expect(existsSync(`${path}.3.gz`)).toBe(false);

    // Most recent rotation (c's content) should be the newest backup (.1.gz).
    const newest = gunzipSync(readFileSync(`${path}.1.gz`)).toString("utf8");
    expect(newest).toBe("c".repeat(200));
  });

  it("prunes backups older than maxAgeDays", () => {
    const path = join(dir, "test.log");
    const opts = { maxSizeBytes: 100, maxBackups: 5, maxAgeDays: 30 };

    writeFileSync(path, "a".repeat(200));
    appendRotating(path, "1\n", opts);
    expect(existsSync(`${path}.1.gz`)).toBe(true);

    // Backdate the backup beyond maxAgeDays.
    const old = new Date(Date.now() - 31 * 24 * 60 * 60 * 1000);
    require("node:fs").utimesSync(`${path}.1.gz`, old, old);

    // Next rotation should prune the now-stale backup during its pass.
    writeFileSync(path, "b".repeat(200));
    appendRotating(path, "2\n", opts);

    // The stale backup was shifted to .2.gz by the shift step, then pruned
    // as too old; only the fresh .1.gz (from this rotation) should remain.
    expect(existsSync(`${path}.1.gz`)).toBe(true);
    expect(existsSync(`${path}.2.gz`)).toBe(false);
  });

  it("never throws even if the target directory is unwritable", () => {
    const path = join(dir, "missing-subdir", "test.log");
    expect(() => appendRotating(path, "line\n")).not.toThrow();
  });
});
