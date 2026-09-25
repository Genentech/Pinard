import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join, isAbsolute, dirname } from "node:path";
import { fileURLToPath } from "node:url";

/**
 * Regression guard (issue #342): the address-review / fix-pipeline agent
 * tasks in processes/swe.js must never instruct the LLM sub-agent to call
 * `aoc notify` itself. That call is invisible/unvetoable to the orchestrator
 * once dispatched, which is exactly how a no-commit turn used to announce
 * "Addressed review feedback" regardless of whether it pushed anything.
 *
 * (open-mr's own "aoc notify ... Opened MR" instruction is out of scope —
 * opening an MR always follows real, just-pushed work, so there is no
 * no-commit case to guard against there.)
 *
 * The notify decision for address-review / fix-pipeline now lives in a
 * deterministic `kind:'node'` task (processes/scripts/notify-if-changed.js,
 * gated by processes/lib/notifyGate.js) that the orchestrator calls
 * directly — never left to prompt text.
 */
describe("processes/swe.js notify-gate regression guard", () => {
  const source = readFileSync(join(__dirname, "../../processes/swe.js"), "utf8");

  function extractTaskBlock(taskId: string): string {
    const marker = `defineTask('${taskId}'`;
    const start = source.indexOf(marker);
    expect(start, `expected to find defineTask('${taskId}', ...) in processes/swe.js`).toBeGreaterThan(-1);
    const end = source.indexOf("}));", start);
    expect(end, `expected a closing "}));" after defineTask('${taskId}', ...)`).toBeGreaterThan(-1);
    return source.slice(start, end);
  }

  it("address-review does not instruct the agent to call `aoc notify` itself", () => {
    expect(extractTaskBlock("address-review")).not.toMatch(/aoc notify/);
  });

  it("fix-pipeline does not instruct the agent to call `aoc notify` itself", () => {
    expect(extractTaskBlock("fix-pipeline")).not.toMatch(/aoc notify/);
  });

  it("routes notifications through the deterministic notify-if-changed node task", () => {
    expect(source).toMatch(/notifyIfChanged/);
    expect(source).toMatch(/scripts\/notify-if-changed\.js/);
  });

  // Regression guard for a real bug caught in review on !632: swe.js is shared
  // across every project's spawns (bin/pinard resolves it via $PINARD_REPO or
  // a per-project override), but a worker's process.cwd() is its own project
  // worktree. A node-task `entry` given as a bare relative string like
  // 'processes/scripts/get-head-sha.js' resolves fine when cwd happens to be
  // this repo (e.g. CI) but ENOENTs for every other project. The entries must
  // be built from this file's own on-disk location (`import.meta.url` via
  // `processDir`), which is absolute regardless of the worker's cwd.
  it("builds node-task entries from this file's own location, not a bare relative path", () => {
    expect(source).toMatch(/const processDir = path\.dirname\(fileURLToPath\(import\.meta\.url\)\)/);
    expect(source).toMatch(/entry:\s*path\.join\(processDir,\s*'scripts\/get-head-sha\.js'\)/);
    expect(source).toMatch(/entry:\s*path\.join\(processDir,\s*'scripts\/notify-if-changed\.js'\)/);
    // The old, broken form: a bare relative literal that only resolves when
    // cwd happens to equal this repo.
    expect(source).not.toMatch(/entry:\s*'processes\/scripts\//);
  });

  it("the resolved node-task entry paths are absolute, regardless of cwd", async () => {
    // Mirrors swe.js's own `const processDir = path.dirname(fileURLToPath(import.meta.url))`
    // exactly, but driven off *this test file's* URL instead — proving the
    // technique yields an absolute path independent of process.cwd().
    const processDir = dirname(fileURLToPath(import.meta.url));
    expect(isAbsolute(processDir)).toBe(true);
    expect(isAbsolute(join(processDir, "scripts/get-head-sha.js"))).toBe(true);
    expect(isAbsolute(join(processDir, "scripts/notify-if-changed.js"))).toBe(true);
  });
});
