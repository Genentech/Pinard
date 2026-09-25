import { describe, it, expect } from "vitest";
import { deriveAgentHealth, classifyTurnStopReason } from "@pinard/logic";

const LIVENESS_MS = 5 * 60 * 1000;
const STALL_MS = 15 * 60 * 1000;
const NOW = Date.parse("2026-01-01T12:00:00Z");

function iso(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString();
}

describe("deriveAgentHealth", () => {
  it("healthy active agent is neither errored nor stalled", () => {
    const h = deriveAgentHealth(
      { tempo: "active", lastSeen: iso(-60_000), lastTransitionAt: iso(-60_000) },
      NOW,
      LIVENESS_MS,
      STALL_MS
    );
    expect(h.errored).toBe(false);
    expect(h.stalled).toBe(false);
  });

  it("blocked/idle agent is never reported stalled, even with an old transition", () => {
    const h = deriveAgentHealth(
      { tempo: "blocked", lastSeen: iso(-60_000), lastTransitionAt: iso(-2 * STALL_MS) },
      NOW,
      LIVENESS_MS,
      STALL_MS
    );
    expect(h.stalled).toBe(false);
  });

  it("errored tempo surfaces lastError and compactions", () => {
    const h = deriveAgentHealth(
      { tempo: "errored", lastError: "401: proxy token expired", compactions: 2 },
      NOW,
      LIVENESS_MS,
      STALL_MS
    );
    expect(h.errored).toBe(true);
    expect(h.lastError).toBe("401: proxy token expired");
    expect(h.compactions).toBe(2);
    expect(h.stalled).toBe(false);
  });

  it("active agent wedged past the stall threshold with a fresh heartbeat is stalled", () => {
    const h = deriveAgentHealth(
      { tempo: "active", lastSeen: iso(-30_000), lastTransitionAt: iso(-STALL_MS - 60_000) },
      NOW,
      LIVENESS_MS,
      STALL_MS
    );
    expect(h.stalled).toBe(true);
  });

  it("active agent with a stale heartbeat is not stalled — it's dead, not wedged", () => {
    const h = deriveAgentHealth(
      { tempo: "active", lastSeen: iso(-2 * LIVENESS_MS), lastTransitionAt: iso(-STALL_MS - 60_000) },
      NOW,
      LIVENESS_MS,
      STALL_MS
    );
    expect(h.stalled).toBe(false);
  });

  it("missing lastTransitionAt does not throw and is not stalled", () => {
    const h = deriveAgentHealth({ tempo: "active", lastSeen: iso(0) }, NOW, LIVENESS_MS, STALL_MS);
    expect(h.stalled).toBe(false);
  });

  it("defaults compactions to 0 when absent", () => {
    const h = deriveAgentHealth({ tempo: "blocked" }, NOW, LIVENESS_MS, STALL_MS);
    expect(h.compactions).toBe(0);
  });
});

describe("classifyTurnStopReason", () => {
  it("maps stopReason=error to errored with a truncated lastError", () => {
    const c = classifyTurnStopReason("error", "401: proxy token expired");
    expect(c.tempo).toBe("errored");
    expect(c.lastError).toBe("401: proxy token expired");
  });

  it("truncates a long errorMessage to 300 chars", () => {
    const c = classifyTurnStopReason("error", "x".repeat(500));
    expect(c.tempo).toBe("errored");
    expect(c.lastError?.length).toBe(300);
  });

  it("synthesizes a lastError when errorMessage is empty on stopReason=error", () => {
    const c = classifyTurnStopReason("error", undefined);
    expect(c.tempo).toBe("errored");
    expect(c.lastError).toBe("error: turn failed");
  });

  it("maps stopReason=length to errored with a synthesized lastError", () => {
    const c = classifyTurnStopReason("length", undefined);
    expect(c.tempo).toBe("errored");
    expect(c.lastError).toBe("length: max output tokens reached");
  });

  it("maps normal completions (stop/toolUse) to blocked", () => {
    expect(classifyTurnStopReason("stop", undefined).tempo).toBe("blocked");
    expect(classifyTurnStopReason("toolUse", undefined).tempo).toBe("blocked");
  });

  it("does NOT treat a deliberate abort as an error", () => {
    const c = classifyTurnStopReason("aborted", undefined);
    expect(c.tempo).toBe("blocked");
    expect(c.lastError).toBeUndefined();
  });

  it("recentAbort suppresses error classification even when stopReason is error", () => {
    const c = classifyTurnStopReason("error", "Operation aborted", true);
    expect(c.tempo).toBe("blocked");
    expect(c.lastError).toBeUndefined();
  });

  it("recentAbort suppresses the length case too", () => {
    const c = classifyTurnStopReason("length", undefined, true);
    expect(c.tempo).toBe("blocked");
  });

  it("recentAbort=false (default) behaves exactly as before", () => {
    const c = classifyTurnStopReason("error", "401: proxy token expired");
    expect(c.tempo).toBe("errored");
  });
});
