import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

/**
 * Guard test (issue #333): a babysitter process definition must never shell
 * out to a provider-specific git-host CLI (`glab`, `gh`) — all forge actions
 * go through the provider-neutral `aoc pressoir` / `aoc comment-mr` commands,
 * so the same process works unmodified on GitLab and GitHub vignobles.
 *
 * Same shape as tests/unit/tool-ownership.test.ts: static text scan, no AST.
 */

const PROCESSES_DIR = join(__dirname, "../../processes");

const processFiles = readdirSync(PROCESSES_DIR).filter((f) =>
  f.endsWith(".js")
);

const FORBIDDEN_PATTERNS: RegExp[] = [/\bglab\b/, /\bgh\s+api\b/, /\bgh\s+pr\b/];

describe("Process forge-neutrality guard", () => {
  it("found at least one process file to check", () => {
    expect(processFiles.length).toBeGreaterThan(0);
  });

  for (const file of processFiles) {
    it(`processes/${file} contains no raw glab/gh invocation`, () => {
      const source = readFileSync(join(PROCESSES_DIR, file), "utf8");
      for (const pattern of FORBIDDEN_PATTERNS) {
        expect(
          pattern.test(source),
          `processes/${file} must not invoke a provider-specific git-host CLI (matched ${pattern}) — use "aoc pressoir" / "aoc comment-mr" instead`
        ).toBe(false);
      }
    });
  }
});
