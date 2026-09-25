export const ACK_REQUIRED_TYPES = new Set([
  "schedule_spawned",
  "schedule_skipped",
  "schedule_failed",
  "needs_approval",
  "circuit_breaker",
]);

// The dashboard's general lane: destination for events that resolve to no
// parcelle (vignoble-level / untriaged). Reserved; never a real wire parcelle.
export const GENERAL_LANE = "_general";

// resolveEventParcelle implements the 3-step routing resolution used by the
// dashboard to route an event to a maître or the general lane:
//   1. explicit parcelle (KV `parcelle` field or a `parcelle:<name>` label)
//   2. default bucket = the event's project (KindRepo parcelle == vigne name)
//   3. general lane (GENERAL_LANE) when neither yields a parcelle
export function resolveEventParcelle(opts: {
  explicitParcelle?: string;
  labels?: string[];
  project?: string;
}): string {
  if (opts.explicitParcelle) return opts.explicitParcelle;
  const label = (opts.labels || []).find((l) => l.startsWith("parcelle:"));
  if (label) {
    const p = label.slice("parcelle:".length).trim();
    if (p) return p;
  }
  if (opts.project) return opts.project;
  return GENERAL_LANE;
}

export function buildDedupeKey(
  sessionId: string,
  type: string,
  data: Record<string, any>
): string {
  let dedupeExtra: any = "";
  if (data.attempt) dedupeExtra = data.attempt;
  else if (data.pipeline_id) dedupeExtra = data.pipeline_id;
  else if (data.iid) dedupeExtra = data.iid;
  else if (data.notes && Array.isArray(data.notes) && data.notes.length > 0) {
    dedupeExtra = data.notes[data.notes.length - 1].note_id || data.notes.length;
  }
  return `${sessionId}:${type}:${dedupeExtra}`;
}

function prRef(data: Record<string, any>): string {
  if (data.mr == null) return "";
  return data.pressoir === "github" ? `PR #${data.mr}` : `MR !${data.mr}`;
}

export function formatEventMessage(
  type: string,
  sessionId: string,
  data: Record<string, any>
): string {
  const project = data._project || data.cwd?.split("/").pop() || sessionId;
  const sessionRef = data._agentSessionId || sessionId;
  const mr = prRef(data);

  if (type === "agent_idle") {
    return `[agent-event] Agent ${project} (${sessionRef}) is idle — finished working. Check its status and decide next steps.`;
  } else if (type === "session_ended") {
    return `[agent-event] Agent ${project} (${sessionRef}) session ended.`;
  } else if (type === "mr_merged") {
    return `[agent-event] ${mr} on ${project} was merged.`;
  } else if (type === "mr_closed") {
    return `[agent-event] ${mr} on ${project} was closed.`;
  } else if (type === "auto_merged") {
    return `[agent-event] ${mr} on ${project} was auto-merged.`;
  } else if (type === "pipeline_passed") {
    return `[agent-event] CI passed on ${mr} (${project}). Ready for review.`;
  } else if (type === "pipeline_failed") {
    return `[agent-event] Pipeline failed on ${mr} (${project}), attempt ${data.attempt || "?"}/${data.max || "?"}. ${data.url || ""}`;
  } else if (type === "needs_approval") {
    return `[notification] ${mr} on ${project} — CI passed, awaiting human approval. A reviewer agent will be spawned automatically. ${data.url || ""}`;
  } else if (type === "main_pipeline_passed") {
    return `[agent-event] Main pipeline passed after ${mr} on ${project}.`;
  } else if (type === "main_pipeline_failed") {
    return `[agent-event] Main pipeline FAILED after ${mr} on ${project}: ${data.url || ""}`;
  } else if (type === "tag_pipeline_passed") {
    return `[agent-event] Tag ${data.tag || ""} pipeline passed on ${project}.`;
  } else if (type === "tag_pipeline_failed") {
    return `[agent-event] Tag ${data.tag || ""} pipeline FAILED on ${project}: ${data.url || ""}`;
  } else if (type === "issues_new") {
    return `[agent-event] New issue #${data.iid} on ${project}: ${data.title}. URL: ${data.url || ""}`;
  } else if (type === "circuit_breaker") {
    return `[agent-event] Circuit breaker: ${mr} on ${project} failed ${data.fail_count || ""} times. Agent stopped.`;
  } else if (type === "schedule_spawned") {
    const sched = data._scheduleName || data.schedule || sessionId;
    return `[inbox] Schedule ${sched} fired — agent spawned on ${project}. Use /inbox to review.`;
  } else if (type === "schedule_skipped") {
    const sched = data._scheduleName || data.schedule || sessionId;
    return `[inbox] Schedule ${sched} — ${data.reason || "poll not met"}. Use /inbox to review.`;
  } else if (type === "schedule_failed") {
    const sched = data._scheduleName || data.schedule || sessionId;
    return `[inbox] Schedule ${sched} FAILED: ${(data.error || "unknown").slice(0, 80)}. Use /inbox to review.`;
  } else if (type === "orphan_exhausted") {
    return `[alert] Orphaned run ${data.runId || sessionId} failed to recover after ${data.retries || "?"} attempts (parcelle: ${data.parcelle || "?"}). Manual intervention needed.`;
  } else if (type === "review_comment") {
    return `[agent-event] Review comment on ${mr} (${project}): ${data.message || "new feedback"}`;
  } else if (type === "issues_comment") {
    return `[issue-comment] Issue #${data.iid || ""} on ${project}: ${data.message || ""}`;
  } else if (type === "btw_reply") {
    return `[btw-reply] ${project} (${sessionRef}): ${data.response || "(no response)"}`;
  }

  return "";
}

export interface StopReasonClassification {
  tempo: "errored" | "blocked";
  lastError?: string;
}

// classifyTurnStopReason maps pi's AssistantMessage.stopReason to the tempo the
// worker should publish (#323). "error" and "length" (the max-output-token
// case) are the only abnormal stops that matter operationally; "stop"/"toolUse"
// are normal completions and "aborted" is the deliberate interrupt-channel path
// (see the `.interrupt` subject handler) — neither is an error.
//
// `recentAbort` — true when the worker itself requested ctx.abort() (conductor
// interrupt_worker / webterm CtlInterrupt) within a short grace window before
// this turn ended. A deliberate abort can surface here as stopReason "error"
// with an errorMessage rather than pi's own "aborted" stopReason, so when set
// it suppresses error classification regardless of stopReason — an operator
// interrupt should never paint an errored/stalled badge.
export function classifyTurnStopReason(
  stopReason: string | undefined,
  errorMessage: string | undefined,
  recentAbort = false
): StopReasonClassification {
  if (recentAbort) return { tempo: "blocked" };
  if (stopReason === "error") {
    const trimmed = (errorMessage || "").trim();
    return { tempo: "errored", lastError: (trimmed || "error: turn failed").slice(0, 300) };
  }
  if (stopReason === "length") {
    return { tempo: "errored", lastError: "length: max output tokens reached" };
  }
  return { tempo: "blocked" };
}

export interface AgentHealth {
  errored: boolean;
  lastError?: string;
  stalled: boolean;
  compactions: number;
}

// Mirrors internal/pnats/agent_health.go's DeriveAgentHealth: derives error/stall
// status from a pinard-agents KV record (#323). `errored` reflects tempo ===
// "errored", set by the worker on turn_end when pi's stopReason is "error" or
// the max-output-token "length" case. `stalled` means the agent claims to be
// actively working (tempo === "active"), its heartbeat is fresh, but its last
// real state/tempo/step transition (lastTransitionAt) is older than
// stallThresholdMs — the worker only advances lastTransitionAt on an actual
// change, so periodic heartbeat republishing doesn't reset it.
export function deriveAgentHealth(
  rec: Record<string, any>,
  now: number,
  livenessThresholdMs: number,
  stallThresholdMs = 15 * 60 * 1000
): AgentHealth {
  const tempo = rec.tempo;
  const health: AgentHealth = {
    errored: tempo === "errored",
    lastError: typeof rec.lastError === "string" ? rec.lastError : undefined,
    stalled: false,
    compactions: typeof rec.compactions === "number" ? rec.compactions : 0,
  };
  if (tempo === "active") {
    const lastSeenMs = Date.parse(rec.lastSeen || "");
    const fresh = Number.isFinite(lastSeenMs) && now - lastSeenMs <= livenessThresholdMs;
    if (fresh) {
      const transMs = Date.parse(rec.lastTransitionAt || "");
      if (Number.isFinite(transMs) && now - transMs > stallThresholdMs) {
        health.stalled = true;
      }
    }
  }
  return health;
}

export type WorkerStatus = "working" | "idle" | "completed" | "stopped";

export function getWorkerStatus(state: {
  state?: string;
  tempo?: string;
  output?: any;
}): WorkerStatus {
  if (
    state.state === "stopped" ||
    state.state === "failed" ||
    state.state === "done"
  )
    return "stopped";
  if (state.tempo === "active") return "working";
  if (state.tempo === "blocked") return "idle";
  if (state.tempo === "idle" && state.output) return "completed";
  if (state.tempo === "idle") return "idle";
  return "working";
}

// buildLessonPayload returns the Engram observation POST body for a /lesson pin,
// or null if text is empty. Extracted for unit-testability without HTTP mocking.
export function buildLessonPayload(
  text: string,
  sessionId: string,
  project: string
): { session_id: string; title: string; content: string; type: string; project: string; confidence: number } | null {
  const trimmed = text.trim();
  if (!trimmed) return null;
  return {
    session_id: sessionId,
    title: trimmed.slice(0, 60),
    content: trimmed,
    type: "rule",
    project,
    confidence: 0.95,
  };
}

export class EventDeduplicator {
  private delivered = new Set<string>();
  private maxSize: number;

  constructor(maxSize = 500) {
    this.maxSize = maxSize;
  }

  shouldDeliver(key: string): boolean {
    return !this.delivered.has(key);
  }

  add(key: string): void {
    this.delivered.add(key);
    if (this.delivered.size > this.maxSize) {
      const first = this.delivered.values().next().value;
      if (first) this.delivered.delete(first);
    }
  }

  get size(): number {
    return this.delivered.size;
  }

  has(key: string): boolean {
    return this.delivered.has(key);
  }

  clear(): void {
    this.delivered.clear();
  }
}
