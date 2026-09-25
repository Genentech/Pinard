import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";

/**
 * Static analysis of tool registration in extensions.
 * Verifies tool ownership spec by parsing source files —
 * no Pi SDK dependency needed.
 */

const ROOT = join(__dirname, "../..");
const CONDUCTOR_SRC = readFileSync(
  join(ROOT, "pi-extension/pinard/index.ts"),
  "utf8"
);
const WORKER_SRC = readFileSync(
  join(ROOT, "pi-extension/worker/index.ts"),
  "utf8"
);

function extractRegisteredTools(source: string): string[] {
  // Match both pi.registerTool(varName) and pi.registerTool(fn(...))
  const matches = source.matchAll(/pi\.registerTool\((\w+)/g);
  return Array.from(matches, (m) => m[1]);
}

function extractToolNames(source: string): Map<string, string> {
  const map = new Map<string, string>();
  const toolDefs = source.matchAll(
    /const\s+(\w+)\s*=[^;]*?defineTool\(\{[^}]*name:\s*"([^"]+)"/gs
  );
  for (const m of toolDefs) {
    map.set(m[1], m[2]);
  }
  return map;
}

const SHARED_SRC = readFileSync(
  join(ROOT, "pi-extension/shared/tools.ts"),
  "utf8"
);
const sharedToolDefs = extractToolNames(SHARED_SRC);

const conductorToolDefs = new Map([
  ...extractToolNames(CONDUCTOR_SRC),
  ...sharedToolDefs,
]);
const conductorRegistered = extractRegisteredTools(CONDUCTOR_SRC);
const conductorToolNames = conductorRegistered.map(
  (varName) => conductorToolDefs.get(varName) || varName
);

const BIN_PINARD_SRC = readFileSync(join(ROOT, "bin/pinard"), "utf8");

function extractPinardToolsAllowlist(source: string): string[] {
  const match = source.match(/_PINARD_TOOLS="([^"]+)"/);
  if (!match) return [];
  return match[1]
    .split(",")
    .map((tool) => tool.trim())
    .filter((tool) => tool && !tool.startsWith("${"));
}

const pinardToolsAllowlist = extractPinardToolsAllowlist(BIN_PINARD_SRC);

const workerToolDefs = new Map([
  ...extractToolNames(WORKER_SRC),
  ...sharedToolDefs,
  ["trackMrTool", "track_mr"],  // factory function — not caught by regex
]);
const workerRegistered = extractRegisteredTools(WORKER_SRC);
const workerToolNames = workerRegistered.map(
  (varName) => workerToolDefs.get(varName) || varName
);

const CONDUCTOR_ONLY_TOOLS = [
  "spawn_agent",
  "respawn_issue",
  "list_workers",
  "kill_worker",
  "interrupt_worker",
  "send_message",
  "get_notifications",
  "get_schedules",
  "create_schedule",
  "get_watcher_logs",
  "get_agent_events",
  "ack_event",
  "update_config",
];

const WORKER_ONLY_TOOLS = ["aoc_notify", "recall"];

// Tools registered by BOTH conductor and worker
const SHARED_TOOLS = [
  "read_issue",
  "track_mr",
];

// Tools only in worker (not in conductor)
const WORKER_SHARED_TOOLS = [
  "update_issue",
];

// Tools only in conductor (not shared with worker)
const CONDUCTOR_SHARED_TOOLS = [
  "create_issue",
  "link_issues",
  "create_cuvee",
  "open_cuvee_mr",
];

describe("Tool ownership: worker extension", () => {
  it("registers aoc_notify", () => {
    expect(workerToolNames).toContain("aoc_notify");
  });

  it("does NOT register any conductor-only tools", () => {
    for (const tool of CONDUCTOR_ONLY_TOOLS) {
      expect(workerToolNames, `worker should not have ${tool}`).not.toContain(
        tool
      );
    }
  });

  it("does NOT register notify_user", () => {
    expect(workerToolNames).not.toContain("notify_user");
  });

  it("registers only expected tools (no extras)", () => {
    const allowed = new Set([...WORKER_ONLY_TOOLS, ...SHARED_TOOLS, ...WORKER_SHARED_TOOLS]);
    for (const tool of workerToolNames) {
      expect(
        allowed.has(tool),
        `unexpected tool '${tool}' registered by worker`
      ).toBe(true);
    }
  });
});

describe("Tool ownership: conductor extension", () => {
  it("registers all conductor-only tools", () => {
    for (const tool of CONDUCTOR_ONLY_TOOLS) {
      expect(
        conductorToolNames,
        `conductor should have ${tool}`
      ).toContain(tool);
    }
  });

  it("does NOT register notify_user (removed)", () => {
    expect(conductorToolNames).not.toContain("notify_user");
  });

  it("registers shared tools", () => {
    for (const tool of [...SHARED_TOOLS, ...CONDUCTOR_SHARED_TOOLS]) {
      expect(
        conductorToolNames,
        `conductor should have shared tool ${tool}`
      ).toContain(tool);
    }
  });

  it("does NOT register worker-only tools", () => {
    for (const tool of WORKER_ONLY_TOOLS) {
      expect(
        conductorToolNames,
        `conductor should not have ${tool}`
      ).not.toContain(tool);
    }
  });
});

describe("Tool ownership: bin/pinard allowlist drift", () => {
  it("lists every tool pi-extension/pinard/index.ts registers via pi.registerTool", () => {
    for (const tool of new Set(conductorToolNames)) {
      expect(
        pinardToolsAllowlist,
        `bin/pinard's _PINARD_TOOLS is missing '${tool}', which is registered via pi.registerTool() in pi-extension/pinard/index.ts — a tool registered but absent from the allowlist is silently hidden from the LLM`
      ).toContain(tool);
    }
  });
});
