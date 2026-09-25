import { describe, it, expect } from "vitest";
import { shouldNotify } from "../../processes/lib/notifyGate.js";

/**
 * Deterministic notify gate (issue #342): a vendangeur turn should only
 * announce "Addressed review feedback" / "Fixed pipeline" when HEAD actually
 * moved. This is a pure function — no model behaviour involved, per the
 * issue's acceptance criteria.
 */
describe("notifyGate.shouldNotify", () => {
  it("does not notify when the sha is unchanged", () => {
    expect(shouldNotify("abc123", "abc123")).toBe(false);
  });

  it("notifies when the sha changed", () => {
    expect(shouldNotify("abc123", "def456")).toBe(true);
  });

  it("notifies on the first check when there is no prior sha", () => {
    expect(shouldNotify(undefined, "abc123")).toBe(true);
    expect(shouldNotify(null, "abc123")).toBe(true);
    expect(shouldNotify("", "abc123")).toBe(true);
  });

  it("never notifies when there is no current sha to report", () => {
    expect(shouldNotify("abc123", undefined)).toBe(false);
    expect(shouldNotify("abc123", "")).toBe(false);
    expect(shouldNotify(undefined, undefined)).toBe(false);
  });
});
