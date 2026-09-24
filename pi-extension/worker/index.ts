import { defineTool, type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "@earendil-works/pi-ai";
import { execSync } from "node:child_process";
import { hostname } from "node:os";
import { readIssueTool, updateIssueTool, trackMrTool } from "../shared/tools.js";
import { connect, wsconnect, type NatsConnection, type Subscription } from "@nats-io/transport-node";
import WebSocket from "ws";
if (!globalThis.WebSocket) (globalThis as any).WebSocket = WebSocket;
import { jetstream, type JetStreamClient } from "@nats-io/jetstream";
import { Kvm, type KV } from "@nats-io/kv";
import { registerProxyProvider, seedProxyAuth } from "../shared/provider.js";

const NATS_URL = process.env.PINARD_NATS_URL || "";
const NATS_CREDS = process.env.PINARD_NATS_CREDS || "";
const NATS_USER = process.env.PINARD_NATS_USER || "";
const NATS_PASS = process.env.PINARD_NATS_PASSWORD || process.env.PINARD_NATS_PASS || "";
const VIGNOBLE = process.env.NATS_VIGNOBLE || "default";
const SESSION = process.env.WORKER_SESSION || "unknown";
const PROJECT = process.env.WORKER_PROJECT || "unknown";
const PROCESS_NAME = process.env.BABYSITTER_PROCESS || "";
const RUN_ID = process.env.BABYSITTER_RUN_ID || "";
// Parcelle defaults to the project (the default-bucket parcelle). Agent subjects
// are ALWAYS parcelle-scoped, so PARCELLE must never be empty.
const PARCELLE = process.env.BABYSITTER_PARCELLE || PROJECT;
const ISSUE_URL = process.env.PINARD_ISSUE_URL || "";
const IS_STANDALONE = process.env.PINARD_STANDALONE === "1";
const HOST = hostname();
const AOC = "aoc";

// Build provenance (TAG/COMMIT_SHORT), e.g. "v0.77.0/a1b2c3d" — see #302.
// PINARD_BUILD is exported by bin/pinard when $PINARD_HOME/BUILD_INFO exists;
// fall back to reading the file directly for callers that skip the launcher.
function resolveBuild(): string | undefined {
  if (process.env.PINARD_BUILD) return process.env.PINARD_BUILD;
  const home = process.env.PINARD_HOME;
  if (!home) return undefined;
  try {
    const { readFileSync } = require("node:fs");
    const { join } = require("node:path");
    const raw = readFileSync(join(home, "BUILD_INFO"), "utf8") as string;
    const vals: Record<string, string> = {};
    for (const line of raw.split("\n")) {
      const idx = line.indexOf("=");
      if (idx === -1) continue;
      vals[line.slice(0, idx)] = line.slice(idx + 1).trim();
    }
    if (!vals.TAG || !vals.COMMIT_SHORT) return undefined;
    return `${vals.TAG}/${vals.COMMIT_SHORT}`;
  } catch {
    return undefined;
  }
}
const BUILD = resolveBuild();

// For process-governed workers, use run ID as the stable NATS agent identifier.
// Run ID survives respawns; session name does not.
const AGENT_ID = (PROCESS_NAME && RUN_ID) ? RUN_ID : SESSION;

// Agent-scoped subject base: pinard.<vignoble>.parcelles.<parcelle>.agents.<id>
function agentBase(id: string): string {
  return `pinard.${VIGNOBLE}.parcelles.${PARCELLE}.agents.${id}`;
}

function inboxSubject(): string {
  if (PROCESS_NAME) {
    return `${agentBase(AGENT_ID)}.process.${PROCESS_NAME}.inbox`;
  }
  return `${agentBase(AGENT_ID)}.inbox`;
}

function eventsSubject(type: string): string {
  if (PROCESS_NAME) {
    return `${agentBase(AGENT_ID)}.process.${PROCESS_NAME}.events.${type}`;
  }
  return `${agentBase(AGENT_ID)}.events.${type}`;
}

let nc: NatsConnection | null = null;
let js: JetStreamClient | null = null;
let kvAgents: KV | null = null;
let piRef: ExtensionAPI | null = null;
let currentCtx: any = null;
let inboxSub: Subscription | null = null;
let btwSub: Subscription | null = null;
let interruptSub: Subscription | null = null;
let jsInboxConsumer: any = null;
let pendingBtw: { btw_id: string; inject: boolean } | null = null;
let pendingEventEffect: { effectId: string; runDir: string } | null = null;

// ── Capsule usage reporting ───────────────────────────────────

const CAPSULE_CONTRACT = process.env.PINARD_CAPSULE_CONTRACT || "";
const CAPSULE_STATS_EVERY = Math.max(1, parseInt(process.env.PINARD_CAPSULE_STATS_EVERY || "", 10) || 10);

let capsuleStatsURL = "";
let capsuleInputTokens = 0;
let capsuleOutputTokens = 0;
let capsuleCacheReadTokens = 0;
let capsuleModel = "";
let capsuleTurnCount = 0;
let capsuleToolCalls = 0;
let capsuleCompactions = 0;

function capsuleJSONPath(): string {
  const runsDir = process.env.BABYSITTER_RUNS_DIR || "";
  const runId = RUN_ID || "";
  if (!runsDir || !runId) return "";
  return require("node:path").join(runsDir, runId, "capsule.json");
}

function loadCapsuleStatsURL(): void {
  if (!CAPSULE_CONTRACT) return;
  const p = capsuleJSONPath();
  if (!p) return;
  try {
    const data = JSON.parse(require("node:fs").readFileSync(p, "utf8"));
    if (data?.contract_stats_url) capsuleStatsURL = data.contract_stats_url;
  } catch {}
}

async function patchCapsuleStats(): Promise<void> {
  if (!capsuleStatsURL) return;
  try {
    const body = JSON.stringify([{
      op: "replace",
      path: "/stats",
      value: {
        input_tokens: capsuleInputTokens,
        output_tokens: capsuleOutputTokens,
        cache_read_tokens: capsuleCacheReadTokens,
        tool_calls: capsuleToolCalls,
        compactions: capsuleCompactions,
        model: capsuleModel,
      },
    }]);
    const res = await fetch(capsuleStatsURL, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body,
    });
    if (!res.ok) {
      console.error(`[worker] capsule stats PATCH failed: ${res.status} ${res.statusText}`);
    }
  } catch (e: any) {
    console.error(`[worker] capsule stats PATCH error: ${e?.message || e}`);
  }
}

// ── NATS ──────────────────────────────────────────────────────

// Initial-connect retry count. Once connected, the client reconnects forever
// (maxReconnectAttempts: -1), so this only governs the cold start. HPC nodes can be
// slow to reach NATS, so default higher and allow override via PINARD_NATS_CONNECT_RETRIES.
const CONNECT_RETRIES = Math.max(1, parseInt(process.env.PINARD_NATS_CONNECT_RETRIES || "", 10) || 10);

async function connectNats(retries = CONNECT_RETRIES): Promise<void> {
  for (let i = 0; i < retries; i++) {
    try {
      const opts: any = {
        servers: NATS_URL,
        reconnect: true,
        maxReconnectAttempts: -1,
        reconnectTimeWait: 5000,
        timeout: 10000,
      };
      if (NATS_CREDS) {
        const { readFileSync } = require("node:fs");
        opts.authenticator = require("@nats-io/transport-node").credsAuthenticator(
          readFileSync(NATS_CREDS)
        );
      } else if (NATS_USER && NATS_PASS) {
        opts.user = NATS_USER;
        opts.pass = NATS_PASS;
        const { usernamePasswordAuthenticator } = require("@nats-io/nats-core");
        opts.authenticator = usernamePasswordAuthenticator(NATS_USER, NATS_PASS);
      }
      const isWs = NATS_URL.startsWith("ws://") || NATS_URL.startsWith("wss://");
      nc = isWs ? await wsconnect(opts) : await connect(opts);
      js = jetstream(nc);
      const kvm = new Kvm(nc);
      kvAgents = await kvm.open("pinard-agents");
      return;
    } catch (e: any) {
      const msg = String(e);
      if (msg.includes("authorization") || msg.includes("authentication")) {
        throw new Error(`[worker] NATS auth failed: ${msg}. Check PINARD_NATS_CREDS or credentials.yaml.`);
      }
      console.error(`[worker] NATS connect attempt ${i + 1}/${retries} failed: ${msg}`);
      if (i < retries - 1) {
        const backoff = Math.min(30000, 1000 * 2 ** i); // capped exponential
        await new Promise((r) => setTimeout(r, backoff));
      }
    }
  }
  throw new Error(`[worker] NATS connection failed after ${retries} attempts (${NATS_URL}). NATS is required.`);
}

function natsPublish(subject: string, data: Record<string, any>): void {
  if (!nc) return;
  const payload = new TextEncoder().encode(JSON.stringify(data));
  if (js) {
    js.publish(subject, payload).catch((err: any) => {
      console.error(`[worker] JetStream publish failed for ${subject}: ${err.message || err}`);
    });
  } else {
    nc.publish(subject, payload);
  }
}

// Fine-grained babysitter state (e.g. "awaiting merged_me|pipeline_success",
// "open-mr", "completed"). Persisted so turn_start/turn_end don't clear it.
let lastStep = "";
let lastStateTempo = { state: "running", tempo: "active" };
let heartbeatTimer: ReturnType<typeof setInterval> | null = null;
const HEARTBEAT_INTERVAL_MS = 60_000; // 1 minute — keeps lastSeen fresh for the /sessions index
async function publishState(state: string, tempo: string, step?: string): Promise<void> {
  if (step !== undefined) lastStep = step;
  lastStateTempo = { state, tempo };
  if (!kvAgents) return;
  try {
    // Read-merge-write: preserve daemon-stamped fields (e.g. `mr`, `repo`) that
    // this worker does not manage. Without this, a periodic publishState call
    // would clobber the `mr` stamp set by `aoc track-mr`, breaking MR routing.
    let preserved: Record<string, unknown> = {};
    try {
      const existing = await kvAgents.get(AGENT_ID);
      if (existing && existing.value.length > 0) {
        const raw = new TextDecoder().decode(existing.value);
        const parsed = JSON.parse(raw) as Record<string, unknown>;
        // Carry forward any fields not written by this worker.
        const workerFields = new Set([
          "project", "name", "agentId", "runId", "process", "parcelle",
          "state", "tempo", "step", "cwd", "vignoble", "issueUrl", "host", "standalone", "build", "lastSeen",
        ]);
        for (const [k, v] of Object.entries(parsed)) {
          if (!workerFields.has(k)) preserved[k] = v;
        }
      }
    } catch (readErr: any) {
      const msg = readErr?.message ?? String(readErr);
      if (!msg.includes("draining") && !msg.includes("closed")) {
        console.error(`[worker] KV read failed (bucket=pinard-agents, key=${AGENT_ID}):`, msg);
      }
    }
    await kvAgents.put(AGENT_ID, JSON.stringify({
      ...preserved,
      project: PROJECT,
      name: SESSION,
      agentId: AGENT_ID,
      runId: RUN_ID || undefined,
      process: PROCESS_NAME || undefined,
      parcelle: PARCELLE || undefined,
      state,
      tempo,
      step: lastStep || undefined,
      cwd: process.cwd(),
      vignoble: VIGNOBLE,
      issueUrl: ISSUE_URL || undefined,
      host: HOST,
      standalone: IS_STANDALONE || undefined,
      build: BUILD || undefined,
      lastSeen: new Date().toISOString(),
    }));
  } catch (e: any) {
    const msg = e?.message ?? String(e);
    if (!msg.includes("draining") && !msg.includes("closed")) {
      console.error(`[worker] KV publishState failed (bucket=pinard-agents, key=${AGENT_ID}):`, msg);
    }
  }
}

// ── Inbox (main channel) ─────────────────────────────────────

async function subscribeInbox(): Promise<void> {
  if (!nc || !piRef) return;
  const subject = inboxSubject();
  const consumerName = `worker-${AGENT_ID}`;

  if (js) {
    const streamName = PROCESS_NAME ? "pinard-processes" : "pinard-inboxes";
    try {
      const jsm = await js.jetstreamManager();
      // Ensure stream exists (daemon creates it, but worker might start before daemon)
      try {
        await jsm.streams.info(streamName);
      } catch {
        console.error(`[worker] Stream ${streamName} not found — creating`);
        const subjects = PROCESS_NAME ? ["pinard.*.parcelles.*.agents.*.process.>"] : ["pinard.*.parcelles.*.agents.*.inbox"];
        await jsm.streams.add({ name: streamName, subjects, retention: "limits", storage: "file", num_replicas: 1 });
      }
      // Create or reuse durable consumer — "all" ensures missed messages are delivered on reconnect
      try {
        await jsm.consumers.add(streamName, {
          durable_name: consumerName,
          filter_subject: subject,
          ack_policy: "explicit",
          deliver_policy: "all",
          max_deliver: 12,
        });
      } catch (e: any) {
        console.error(`[worker] Consumer ${consumerName} add: ${e.message || e}`);
      }
      const stream = await js.streams.get(streamName);
      const consumer = await stream.getConsumer(consumerName);
      jsInboxConsumer = consumer;
      const messages = await consumer.consume();
      (async () => {
        for await (const msg of messages) {
          try {
            const data = msg.json<Record<string, any>>();
            // Typed messages are events the daemon delivers at the babysitter event
            // step — if the worker hasn't reached event-wait yet, NAK so JetStream
            // redelivers later. Typeless messages are conductor/human chat
            // (send_message) and are delivered immediately regardless of process state.
            if (PROCESS_NAME && data.type) {
              const { existsSync } = require("node:fs");
              const signalFile = require("node:path").join(process.cwd(), ".babysitter-event-wait.json");
              if (!existsSync(signalFile) && !pendingEventEffect) {
                // Not in event-wait mode yet. NAK with delay so JetStream redelivers
                // once babysitter reaches its event-wait step. Give up after max_deliver (12).
                const delivered = msg.info.deliveryCount;
                if (delivered >= 11) {
                  console.error(`[worker] Dropping event after ${delivered} deliveries (never entered event-wait): type=${data.type}`);
                  msg.ack();
                } else {
                  msg.nak(5000);
                }
                continue;
              }
            }
            handleInboxMessage(data);
            msg.ack();
          } catch { msg.ack(); }
        }
      })();
      console.error(`[worker] Subscribed to ${subject} via JetStream (${streamName}/${consumerName})`);
      return;
    } catch (e: any) {
      console.error(`[worker] JetStream subscription failed for ${streamName}: ${e.message || e} — falling back to core NATS`);
    }
  }

  // Fallback: core NATS (freeform workers only)
  if (PROCESS_NAME) {
    console.error(`[worker] CRITICAL: Process worker could not subscribe to JetStream inbox. Events will be lost. Worker should be restarted.`);
    return;
  }
  inboxSub = nc.subscribe(subject);
  (async () => {
    for await (const msg of inboxSub!) {
      try {
        const data = JSON.parse(new TextDecoder().decode(msg.data));
        handleInboxMessage(data);
      } catch {}
    }
  })();
}

function handleInboxMessage(data: Record<string, any>): void {
  if (!piRef) return;
  const message = data.message || "";

  // Check for pending event effect from file signal (cross-extension communication)
  let expectedEventTypes: string[] = [];
  if (PROCESS_NAME && !pendingEventEffect) {
    try {
      const { readFileSync, existsSync } = require("node:fs");
      const signalFile = require("node:path").join(process.cwd(), ".babysitter-event-wait.json");
      if (existsSync(signalFile)) {
        const signal = JSON.parse(readFileSync(signalFile, "utf8"));
        if (signal.effectId) {
          pendingEventEffect = { effectId: signal.effectId, runDir: signal.runDir || "" };
          expectedEventTypes = signal.eventTypes || [];
        }
      }
    } catch {}
  }

  // When babysitter is waiting for an event, resolve the effect instead of prompting the LLM
  if (PROCESS_NAME && pendingEventEffect) {
    // Typeless messages are conductor/human chat (send_message), not event
    // resolutions — only typed events (from the daemon) resolve the effect. Deliver
    // chat to the LLM without resolving the effect or removing the signal file, so
    // the process stays parked: babysitter's turn_end sees the effect still pending
    // and does not advance.
    if (!data.type) {
      if (message) {
        console.error(`[worker] Delivering chat message while in event-wait (effect left pending): ${message.slice(0, 50)}`);
        piRef.sendUserMessage(message, { deliverAs: "followUp" });
      }
      return;
    }
    // Only resolve if the event type matches what the process is waiting for
    if (expectedEventTypes.length > 0 && !expectedEventTypes.includes(data.type)) {
      console.error(`[worker] Ignoring event type=${data.type} — waiting for: ${expectedEventTypes.join(", ")}`);
      return;
    }

    const { effectId, runDir } = pendingEventEffect;
    const PINARD_REPO_PATH = process.env.PINARD_REPO || "";
    const bsCli = `node ${PINARD_REPO_PATH}/deps/babysitter/packages/sdk/dist/cli/main.js`;
    try {
      const valueJson = JSON.stringify(data).replace(/'/g, "'\\''");
      execSync(`${bsCli} task:post "${runDir}" ${effectId} --status ok --value-inline '${valueJson}'`, {
        encoding: "utf8",
        timeout: 10_000,
      });
      pendingEventEffect = null;
      try { require("node:fs").unlinkSync(require("node:path").join(process.cwd(), ".babysitter-event-wait.json")); } catch {}
      console.error(`[worker] Resolved event effect ${effectId} with type=${data.type}`);
      // Trigger a turn so babysitter extension's turn_end fires and drives next iteration
      piRef.sendUserMessage(`Event received: ${data.type}`, { deliverAs: "followUp" });
    } catch (e: any) {
      console.error(`[worker] Failed to resolve event effect: ${e.message}`);
      // Don't fallback to LLM — leave signal file intact, daemon will re-dispatch
    }
    return;
  }

  if (message) {
    piRef.sendUserMessage(message, { deliverAs: "followUp" });
  }
}

// ── BTW channel (parallel questions) ─────────────────────────

function subscribeBtw(): void {
  if (!nc || !piRef) return;
  const subject = `${agentBase(SESSION)}.btw`;

  btwSub = nc.subscribe(subject);
  (async () => {
    for await (const msg of btwSub!) {
      try {
        const data = JSON.parse(new TextDecoder().decode(msg.data));
        const message = data.message || "";
        const btw_id = data.btw_id || "";
        const inject = data.inject || false;

        if (message && piRef) {
          pendingBtw = { btw_id, inject };
          piRef.sendUserMessage(
            `[conductor question — reply directly, your response will be sent back automatically]\n${message}`,
            { deliverAs: "steer" }
          );
        }
      } catch {}
    }
  })();
}

function publishBtwReply(response: string): void {
  if (!pendingBtw) return;
  natsPublish(eventsSubject("btw_reply"), {
    btw_id: pendingBtw.btw_id,
    response,
    _project: PROJECT,
    _agentSessionId: SESSION,
    timestamp: new Date().toISOString(),
  });
  pendingBtw = null;
}

// ── Interrupt channel ────────────────────────────────────────

function subscribeInterrupt(): void {
  if (!nc || !piRef) return;
  const subject = `${agentBase(SESSION)}.interrupt`;

  interruptSub = nc.subscribe(subject);
  (async () => {
    for await (const msg of interruptSub!) {
      try {
        const data = JSON.parse(new TextDecoder().decode(msg.data));
        const reason = data.reason || "Interrupted by conductor";

        if (currentCtx && !currentCtx.isIdle?.()) {
          currentCtx.abort?.();
        }
        if (piRef) {
          piRef.sendUserMessage(
            `[interrupted] ${reason}`,
            { deliverAs: "steer" }
          );
        }
      } catch {}
    }
  })();
}

// ── Tools ─────────────────────────────────────────────────────

// Engram HTTP API. ENGRAM_URL is injected by the launcher (aoc) as the authoritative
// single source of truth for the per-vignoble Engram port. Do NOT provide a fallback
// port — every vignoble gets a deterministic but different port (PortForVignoble), so
// any hardcoded default would silently hit the wrong server. If unset, the recall tool
// skips the Engram leg with a one-time warning.
const ENGRAM_URL = process.env.ENGRAM_URL || "";
let _engramUrlWarnedOnce = false;

const recallTool = defineTool({
  name: "recall",
  label: "Recall Memory",
  description: "Recall relevant knowledge from memory. Two modes: (1) query mode — fans out to the curated pinard knowledge base (SurrealDB wiki + typed entities) and local Engram observations, returning source-labeled raw hits; call when stuck, on failure, or starting a new subtask. (2) fetch mode — exact drill-down by ref, returning the full body of a specific wiki page or entity; use when you have a ref from a boot manifest or recall hit. query and fetch are mutually exclusive.",
  parameters: Type.Object({
    query: Type.Optional(Type.String({ description: "Query mode: what you want to recall — a question, symptom, or topic. Mutually exclusive with fetch." })),
    fetch: Type.Optional(Type.String({ description: "Fetch mode: exact ref to expand — wiki:<path> for a wiki page, entity:<id> for an entity. Mutually exclusive with query." })),
    k: Type.Optional(Type.Number({ description: "Max hits per source in query mode (default: 8)" })),
  }),
  async execute(_toolCallId, params, _signal, _onUpdate, _ctx) {
    const fetchRef = (params.fetch || "").trim();

    // ── Fetch mode: exact drill-down by ref ───────────────────────────────
    if (fetchRef) {
      if (nc) {
        try {
          const recallSubject = `pinard.${VIGNOBLE}.recall`;
          const payload = JSON.stringify({
            session_id: SESSION,
            group_id: PROJECT,
            vignoble: VIGNOBLE,
            fetch: fetchRef,
          });
          const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
          const resp = JSON.parse(new TextDecoder().decode(msg.data));
          const result = resp.result;
          if (result) {
            let text: string;
            if (result.type === "wiki") {
              text = `[wiki · ${result.scope}] ${result.title || ""}\n\n${result.body || ""}`;
            } else {
              text = `[entity:${result.role || "entity"} · ${result.scope}] ${result.name || ""}\n\n${result.description || ""}`;
            }
            return {
              content: [{ type: "text" as const, text: text.trim() }],
              details: { summary: `fetched ${result.type} ref=${fetchRef} from scope=${result.scope}` },
            };
          }
        } catch {
          // Fail-open: NATS unavailable or service not running
        }
      }
      return {
        content: [{ type: "text" as const, text: `(no result for ref: ${fetchRef})` }],
        details: { summary: `fetch not found: ${fetchRef}` },
      };
    }

    // ── Query mode: semantic + FTS fan-out ────────────────────────────────
    const query = params.query || "";
    const k = params.k ?? 8;

    const curated: Array<{ text: string; type: string; confidence: number; ref: string }> = [];
    const engram: Array<{ text: string; obs_type: string; when: string; scope: string }> = [];

    // Fan-out 1: pinard SurrealDB recall via NATS request-reply
    // Pass vignoble-wide scope list so all vigne scopes are searched.
    if (nc) {
      try {
        const recallSubject = `pinard.${VIGNOBLE}.recall`;
        const workerScopes = [PROJECT, `vignoble-${VIGNOBLE}`, VIGNOBLE, "__global__"];
        const payload = JSON.stringify({
          session_id: SESSION,
          group_id: PROJECT,
          vignoble: VIGNOBLE,
          scopes: workerScopes,
          query: { user_message: query },
          constraints: { max_context_tokens: 400 },
          raw: true,
        });
        const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 5_000 });
        const resp = JSON.parse(new TextDecoder().decode(msg.data));
        const hits: any[] = resp.hits || [];
        for (const h of hits.slice(0, k)) {
          const scope = h._scope || h.scope || "";
          const ref = h._wiki ? `wiki:${h.path || ""}` : (h.id ? `entity:${h.id}` : "");
          const text = h._wiki
            ? `[wiki · ${scope}] ${h.title || ""}: ${h.body || ""}`
            : `[entity:${h.role || "entity"} · ${scope}] ${h.name || ""}: ${h.description || ""}`;
          curated.push({
            text: text.trim(),
            type: h._wiki ? "wiki" : (h.role || "entity"),
            confidence: h.confidence ?? (h.dist != null ? Math.max(0, 1 - h.dist) : 1),
            ref,
          });
        }
      } catch {
        // Fail-open: NATS unavailable or service not running
      }
    }

    // Fan-out 2: local Engram search via HTTP
    // ENGRAM_URL is authoritative (injected by launcher); no fallback port — each
    // vignoble gets a unique port and guessing would silently hit the wrong server.
    if (!ENGRAM_URL) {
      if (!_engramUrlWarnedOnce) {
        _engramUrlWarnedOnce = true;
        console.error("[recall] ENGRAM_URL not set — skipping Engram leg");
      }
    } else {
      try {
        const url = `${ENGRAM_URL}/search?q=${encodeURIComponent(query)}&limit=${k}&project=${encodeURIComponent(PROJECT)}`;
        const res = await fetch(url, { signal: AbortSignal.timeout(5_000) });
        if (res.ok) {
          const raw = await res.json();
          const hits: any[] = Array.isArray(raw) ? raw : [];
          for (const h of hits) {
            const text = `${h.title || ""}${h.content ? ": " + h.content.slice(0, 400) : ""}`;
            engram.push({
              text: text.trim(),
              obs_type: h.type || "",
              when: h.created_at || "",
              scope: h.scope || "project",
            });
          }
        }
      } catch {
        // Fail-open: Engram temporarily unavailable
      }
    }

    const result = { curated, engram };
    const summary = `curated: ${curated.length} hit(s), engram: ${engram.length} hit(s)`;
    return {
      content: [{ type: "text" as const, text: JSON.stringify(result, null, 2) }],
      details: { summary },
    };
  },
});

const notifyTool = defineTool({
  name: "aoc_notify",
  label: "Notify Conductor",
  description: "Send a notification to the conductor (pinard). Use when you finish a task, open an MR, or need to report status.",
  parameters: Type.Object({
    message: Type.String({ description: "Notification message (will be prefixed with session name)" }),
  }),
  async execute(_toolCallId, params, _signal, _onUpdate, _ctx) {
    const msg = `[${SESSION}] ${params.message}`;

    // If there's a pending btw question, this reply also goes to btw_reply
    if (pendingBtw) {
      publishBtwReply(params.message);
    }

    try {
      execSync(`${AOC} notify "${msg.replace(/"/g, '\\"')}"`, { encoding: "utf8", timeout: 5_000 });
      return { content: [{ type: "text" as const, text: `Notified: ${msg}` }], details: undefined };
    } catch (e: any) {
      return { content: [{ type: "text" as const, text: `Failed: ${e.message}` }], details: undefined };
    }
  },
});

// ── Extension Entry Point ─────────────────────────────────────

export default function worker(pi: ExtensionAPI) {
  piRef = pi;
  registerProxyProvider(pi);

  pi.registerTool(notifyTool);
  pi.registerTool(recallTool);
  pi.registerTool(trackMrTool(AGENT_ID, PROJECT, AOC));
  pi.registerTool(readIssueTool);
  pi.registerTool(updateIssueTool);

  // /lesson <text> — pin a high-confidence rule/fact to shared pinard memory.
  // /lesson [--edit [--entity=<id>] | --replace --entity=<id> <text> | <text>]
  pi.registerCommand("lesson", {
    description: "Pin/edit a lesson: /lesson <text>  |  /lesson --edit [--entity=<id>]  |  /lesson --replace --entity=<id> <text>",
    handler: async (args, ctx) => {
      if (!nc) {
        ctx.ui.notify("Cannot access lessons: NATS not connected", "error");
        return;
      }
      const sessionId = `worker-lesson-${PROJECT}`;
      const recallSubject = `pinard.${VIGNOBLE}.recall`;
      const rulesSubject = `pinard.${VIGNOBLE}.memory.rules`;

      // ── --replace --entity=<id> <text> ─────────────────────────────────────────────
      const replaceMatch = args.match(/^--replace\s+--entity=([^\s]+)\s+(.+)$/s);
      if (replaceMatch) {
        const entityId = replaceMatch[1].trim();
        const text = replaceMatch[2].trim();
        if (!text) {
          ctx.ui.notify("Usage: /lesson --replace --entity=<id> <new text>", "warning");
          return;
        }
        natsPublish(rulesSubject, {
          op: "replace",
          replaces: entityId,
          session_id: sessionId,
          title: text.slice(0, 60),
          content: text,
          type: "rule",
          project: PROJECT,
          confidence: 0.95,
        });
        ctx.ui.notify(`Lesson replaced (id: ${entityId.slice(0, 20)}): ${text.slice(0, 50)}`, "info");
        piRef?.sendMessage({ customType: "pinard-lesson", content: text, display: true }, { triggerTurn: true });
        return;
      }

      // ── --edit [--entity=<id>] ─────────────────────────────────────────────────────
      const editFlag = /^--edit\b/.test(args);
      if (editFlag) {
        const entityMatch = args.match(/--entity=([^\s]+)/);
        let entityId = entityMatch ? entityMatch[1].trim() : null;
        let existingText = "";

        if (entityId) {
          try {
            const payload = JSON.stringify({ session_id: sessionId, group_id: PROJECT, vignoble: VIGNOBLE, fetch: entityId });
            const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
            const resp = JSON.parse(new TextDecoder().decode(msg.data));
            const result = resp.result;
            if (!result) {
              ctx.ui.notify(`Entity not found: ${entityId}`, "error");
              return;
            }
            existingText = result.description || result.body || "";
          } catch {
            ctx.ui.notify("Could not fetch entity — NATS timeout", "error");
            return;
          }
        } else {
          ctx.ui.notify("Fetching lessons…", "info");
          let hits: any[] = [];
          try {
            const workerQueryScopes = [PROJECT, `vignoble-${VIGNOBLE}`, VIGNOBLE, "__global__"];
            const payload = JSON.stringify({
              session_id: sessionId,
              group_id: PROJECT,
              vignoble: VIGNOBLE,
              query: { user_message: "lesson rule decision fact" },
              raw: true,
              k: 20,
              scopes: workerQueryScopes,
            });
            const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
            const resp = JSON.parse(new TextDecoder().decode(msg.data));
            hits = (resp.hits || []).filter((h: any) => h.provenance === "lesson" && h.id);
          } catch {
            ctx.ui.notify("Could not query lessons — NATS timeout", "error");
            return;
          }
          if (hits.length === 0) {
            ctx.ui.notify("No editable lessons found", "info");
            return;
          }
          const choices = hits.map((h: any) => `${h.name || h.description?.slice(0, 60) || h.id}`);
          const pick = await ctx.ui.select("Edit which lesson?", choices);
          if (!pick) return;
          const idx = choices.indexOf(pick);
          if (idx === -1) return;
          const hit = hits[idx];
          entityId = String(hit.id);
          existingText = hit.description || "";
        }

        // Open editor for all lengths — publish replace directly to avoid input-bar truncation
        const edited = await ctx.ui.editor("Edit lesson", existingText);
        if (!edited || !edited.trim()) return;
        natsPublish(rulesSubject, {
          op: "replace",
          replaces: entityId,
          session_id: sessionId,
          title: edited.trim().slice(0, 60),
          content: edited.trim(),
          type: "rule",
          project: VIGNOBLE,
          confidence: 0.95,
        });
        ctx.ui.notify(`Lesson updated (${entityId.slice(0, 20)})`, "info");
        return;
      }

      // ── plain /lesson <text> (upsert) ──────────────────────────────────────────────
      const text = args.trim();
      if (!text) {
        ctx.ui.notify("Usage: /lesson <text>  |  /lesson --edit [--entity=<id>]  |  /lesson --replace --entity=<id> <new text>", "warning");
        return;
      }
      try {
        // Publish to the pinard NATS memory pipeline: the ingester upserts a
        // `decision` entity (provenance=lesson) scoped to PROJECT (the vigne the
        // worker is on), so the lesson reaches shared memory (boot + /recall).
        //
        // We deliberately do NOT write to Engram directly. A raw POST /sessions
        // creates a legacy-format session mutation with no payload directory,
        // which permanently jams Engram cloud sync for the whole project
        // (unrepairable by engram's own doctor/repair/bootstrap). The NATS path
        // is the reliable, side-effect-free channel for pinned rules.
        natsPublish(rulesSubject, {
          session_id: sessionId,
          title: text.slice(0, 60),
          content: text,
          type: "rule",
          project: PROJECT,
          confidence: 0.95,
        });
        ctx.ui.notify(`Lesson pinned: ${text.slice(0, 60)}`, "info");
        piRef?.sendMessage({ customType: "pinard-lesson", content: text, display: true }, { triggerTurn: true });
      } catch (e: any) {
        ctx.ui.notify(`Failed to pin lesson: ${e?.message ?? String(e)}`, "error");
      }
    },
  });

  // /forget --entity=<id> — delete a pinned lesson from shared memory.
  pi.registerCommand("forget", {
    description: "Delete a pinned lesson: /forget --entity=<id>",
    handler: async (args, ctx) => {
      if (!nc) {
        ctx.ui.notify("Cannot delete lesson: NATS not connected", "error");
        return;
      }
      const entityMatch = args.match(/--entity=([^\s]+)/);
      if (!entityMatch) {
        ctx.ui.notify("Usage: /forget --entity=<id>", "warning");
        return;
      }
      const entityId = entityMatch[1].trim();
      const sessionId = `worker-lesson-${PROJECT}`;
      const confirmed = await ctx.ui.confirm(
        "Delete lesson",
        `Delete entity ${entityId}? This is irreversible.`,
      );
      if (!confirmed) return;
      try {
        natsPublish(`pinard.${VIGNOBLE}.memory.rules`, {
          op: "delete",
          entity_id: entityId,
          session_id: sessionId,
          project: PROJECT,
        });
        ctx.ui.notify(`Lesson deleted: ${entityId}`, "info");
      } catch (e: any) {
        ctx.ui.notify(`Failed to delete lesson: ${e?.message ?? String(e)}`, "error");
      }
    },
  });

  // /recall [--global|--scope <name>] <query> | fetch <ref>
  // Human-facing memory recall: query or fetch from the pinard knowledge base,
  // pick hits interactively, and inject selected memories into the session.
  pi.registerCommand("recall", {
    description: "Query memory and inject hits into context: /recall [--scope <name>] <query>  or  /recall fetch <ref>",
    handler: async (args, ctx) => {
      if (!nc) {
        ctx.ui.notify("NATS not connected — cannot recall", "error");
        return;
      }

      // Parse args: strip optional --global / --scope <name> / --type <t> flags
      let rest = (args || "").trim();
      let scopeOverride: string | null = null;
      let typeFilter: string | null = null;
      const globalFlag = /^--global\b/.test(rest);
      if (globalFlag) {
        scopeOverride = "__global__";
        rest = rest.replace(/^--global\s*/, "");
      }
      const scopeMatch = rest.match(/^--scope[= ](\S+)\s*/);
      if (scopeMatch) {
        scopeOverride = scopeMatch[1];
        rest = rest.slice(scopeMatch[0].length);
      }
      const typeMatch = rest.match(/^--type[= ](\S+)\s*/);
      if (typeMatch) {
        typeFilter = typeMatch[1].toLowerCase();
        rest = rest.slice(typeMatch[0].length);
      }

      const recallSubject = `pinard.${VIGNOBLE}.recall`;

      // ── Fetch mode ──────────────────────────────────────────────────────
      if (rest.startsWith("fetch ")) {
        const fetchRef = rest.slice(6).trim();
        if (!fetchRef) {
          ctx.ui.notify("Usage: /recall fetch <wiki:path|entity:id>", "warning");
          return;
        }
        let body = "(no result)";
        try {
          const payload = JSON.stringify({
            session_id: SESSION,
            group_id: PROJECT,
            vignoble: VIGNOBLE,
            fetch: fetchRef,
            ...(scopeOverride ? { scope: scopeOverride } : {}),
          });
          const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
          const resp = JSON.parse(new TextDecoder().decode(msg.data));
          const result = resp.result;
          if (result) {
            if (result.type === "wiki") {
              body = `[wiki · ${result.scope || ""}] ${result.title || ""}\n\n${result.body || ""}`;
            } else {
              body = `[entity:${result.role || "entity"} · ${result.scope || ""}] ${result.name || ""}\n\n${result.description || ""}`;
            }
          }
        } catch {
          ctx.ui.notify("Recall fetch timed out or NATS unavailable", "warning");
          return;
        }
        const fetchContent = body.trim();
        if (!fetchContent || fetchContent === "(no result)") {
          ctx.ui.notify("No result", "info");
          return;
        }
        piRef?.sendMessage({ customType: "pinard-recall", content: fetchContent, display: true }, { triggerTurn: true });
        return;
      }

      // ── Query mode ──────────────────────────────────────────────────────
      let query = rest;
      if (!query) {
        const input = await ctx.ui.input?.("Recall query:");
        if (!input) return;
        query = input.trim();
        if (!query) return;
      }

      ctx.ui.notify(`Querying memory: ${query.slice(0, 60)}…`, "info");

      // Build scope list: worker scopes for vignoble-wide fan-out.
      const workerQueryScopes = scopeOverride
        ? [scopeOverride]
        : [PROJECT, `vignoble-${VIGNOBLE}`, VIGNOBLE, "__global__"];

      // Collect hits from NATS recall service
      const curatedHits: Array<{ label: string; ref: string; body?: string; provenance?: string; id?: string }> = [];
      const engramHits: Array<{ label: string; content: string }> = [];

      if (typeFilter !== "engram") try {
        const payload = JSON.stringify({
          session_id: SESSION,
          group_id: PROJECT,
          vignoble: VIGNOBLE,
          scopes: workerQueryScopes,
          query: { user_message: query },
          raw: true,
          k: 8,
          ...(scopeOverride ? { scope: scopeOverride } : {}),
          ...(typeFilter ? { type_filter: typeFilter } : {}),
        });
        const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
        const resp = JSON.parse(new TextDecoder().decode(msg.data));
        const hits: any[] = resp.hits || [];
        for (const h of hits) {
          const score = h.confidence != null ? ` (${(h.confidence * 100).toFixed(0)}%)` : h.dist != null ? ` (dist:${h.dist.toFixed(2)})` : "";
          const scope = h._scope || h.scope || "";
          if (h._wiki) {
            const ref = `wiki:${h.path || ""}`;
            curatedHits.push({ label: `[wiki · ${scope}] ${h.title || ""}${score} (ref: ${ref})`, ref });
          } else {
            const ref = h.id ? String(h.id) : "";
            const provenance = h.provenance || "";
            const roleLabel = provenance === "lesson" ? "lesson" : provenance === "episode_extraction" ? "teaching" : `entity:${h.role || "entity"}`;
            const summary = (h.description || "").slice(0, 120);
            curatedHits.push({
              label: `[${roleLabel} · ${scope}] ${h.name || ""}${score}${ref ? ` (ref: ${ref})` : ""}`,
              ref,
              body: summary,
              provenance: h.provenance,
              id: h.id ? String(h.id) : undefined,
            });
          }
        }
      } catch {
        // NATS timeout or service unavailable — continue with Engram only
      }

      // Fan-out: Engram local observations (skip when --type filters to non-engram)
      if (ENGRAM_URL && typeFilter !== "wiki" && ![
        "lesson", "teaching", "decision", "artifact", "task", "diagnosis",
      ].includes(typeFilter || "")) {
        try {
          const url = `${ENGRAM_URL}/search?q=${encodeURIComponent(query)}&limit=8&project=${encodeURIComponent(PROJECT)}`;
          const res = await fetch(url, { signal: AbortSignal.timeout(5_000) });  
          if (res.ok) {
            const raw = await res.json();
            const hits: any[] = Array.isArray(raw) ? raw : [];
            for (const h of hits) {
              const label = `[engram:${h.type || "observation"} · ${h.scope || "project"}] ${h.title || ""}`;
              engramHits.push({ label, content: (h.content || "").slice(0, 400) });
            }
          }
        } catch {
          // Engram unavailable — skip
        }
      }

      // Inline recall browser (worker has no shared TUI class; use ctx.ui.select loop for worker)
      type WorkerRecallHit = { label: string; ref?: string; body?: string; source: "curated" | "engram"; provenance?: string; id?: string };
      const allItems: WorkerRecallHit[] = [
        ...curatedHits.map((h) => ({ ...h, source: "curated" as const })),
        ...engramHits.map((h) => ({ label: h.label, body: h.content, source: "engram" as const })),
      ];

      if (allItems.length === 0) {
        ctx.ui.notify(`No memory hits for: ${query.slice(0, 60)}`, "info");
        return;
      }

      // Pick loop with edit hint
      const selected: Array<{ label: string; body: string }> = [];
      const remaining = [...allItems];

      while (remaining.length > 0) {
        const choices = [
          ...remaining.map((h) => h.label),
          "── Done ──",
        ];
        const pick = await ctx.ui.select(`Pick memory to inject (${remaining.length} hit${remaining.length !== 1 ? "s" : ""} — query: ${query.slice(0, 40)}):`, choices);
        if (pick == null || pick === "── Done ──") break;

        const idx = remaining.findIndex((h) => h.label === pick);
        if (idx === -1) break;
        const hit = remaining[idx];
        remaining.splice(idx, 1);

        // Engram hits: read-only from here
        if (hit.source === "engram") {
          // Engram observations are managed via mem_update — offer inject only
          ctx.ui.notify("Engram observations: edit via mem_update tool", "info");
        }

        // Curated non-wiki entities with an id: offer edit
        if (hit.source === "curated" && hit.id && !hit.label.includes("[wiki")) {
          const editChoices = selected.length > 0
            ? ["inject"]  // multi-selection in progress — edit not offered
            : ["inject", "edit"];
          if (selected.length > 0) {
            ctx.ui.notify("Edit applies to a single item — multi-select is for inject", "info");
          }
          const action = await ctx.ui.select(`${hit.label.slice(0, 60)} — action:`, editChoices);
          if (action === "edit") {
            const existingText = hit.body || "";
            const edited = await ctx.ui.editor("Edit entity", existingText);
            if (edited?.trim()) {
              const wRulesSubject = `pinard.${VIGNOBLE}.memory.rules`;
              const wSessionId = `worker-lesson-${PROJECT}`;
              natsPublish(wRulesSubject, {
                op: "edit_entity",
                entity_id: hit.id,
                session_id: wSessionId,
                content: edited.trim(),
                project: VIGNOBLE,
              });
              ctx.ui.notify(`Entity updated (${String(hit.id).slice(0, 20)})`, "info");
            }
            continue;
          }
        }

        // Fetch full body for curated wiki/entity hits
        let body = hit.body || "";
        if (hit.source === "curated" && hit.ref) {
          try {
            const payload = JSON.stringify({
              session_id: SESSION,
              group_id: PROJECT,
              vignoble: VIGNOBLE,
              fetch: hit.ref,
            });
            const msg = await nc.request(recallSubject, new TextEncoder().encode(payload), { timeout: 8_000 });
            const resp = JSON.parse(new TextDecoder().decode(msg.data));
            const result = resp.result;
            if (result) {
              body = result.type === "wiki" ? (result.body || "") : (result.description || "");
            }
          } catch {
            // Use summary body as fallback
          }
        }

        selected.push({ label: hit.label, body });
      }

      if (selected.length === 0) return;

      const content = selected
        .map((s) => `### ${s.label}\n\n${s.body.trim() || "(no body)"}`)
        .join("\n\n---\n\n");

      if (!content || !content.trim()) {
        ctx.ui.notify("No result", "info");
        return;
      }
      piRef?.sendMessage({ customType: "pinard-recall", content, display: true }, { triggerTurn: true });
    },
  });

  pi.on("session_start", async (_event: any, ctx: any) => {
    seedProxyAuth();
    loadCapsuleStatsURL();
    await connectNats();
    if (nc) {
      await publishState("running", "active");
      await subscribeInbox();
      subscribeBtw();
      subscribeInterrupt();
      // Periodic heartbeat: refresh lastSeen in the KV so the /sessions index
      // doesn't expire this record while the agent is alive but idle.
      if (heartbeatTimer) clearInterval(heartbeatTimer);
      heartbeatTimer = setInterval(() => {
        void publishState(lastStateTempo.state, lastStateTempo.tempo);
      }, HEARTBEAT_INTERVAL_MS);
    }
    const natsDot = nc ? "📡" : "○";
    const jsDot = jsInboxConsumer ? "JS" : "sub";
    pi.setSessionName?.(`🧺 ${SESSION}`);
    ctx?.ui?.setStatus?.("worker", `🧺 Vendangeur ${SESSION} — ${natsDot} NATS (${jsDot})`);
  });

  // Listen for babysitter signaling it's waiting for an event
  pi.on?.("babysitter:waiting_for_event", (data: any) => {
    if (data?.effectId) {
      pendingEventEffect = { effectId: data.effectId, runDir: data.runDir || "" };
    }
  });

  // Mirror the babysitter's process state into KV (worker owns KV) so the aoc
  // dashboard shows the same process emoji + colored state as the Pi status line.
  pi.on?.("babysitter:status", (data: any) => {
    const s: string = data?.state || "";
    const tempo = s === "completed" ? "completed"
      : s === "failed" ? "failed"
      : s.startsWith("awaiting") ? "awaiting"
      : "active";
    void publishState("running", tempo, s);
  });

  pi.on("turn_start", async (_event: any, ctx: any) => {
    currentCtx = ctx;
    await publishState("running", "active");
  });

  pi.on("turn_end", async (event: any) => {
    currentCtx = null;
    await publishState("running", "blocked");
    if (capsuleStatsURL) {
      const usage = event?.message?.usage;
      if (usage) {
        capsuleInputTokens += usage.input ?? 0;
        capsuleOutputTokens += usage.output ?? 0;
        capsuleCacheReadTokens += usage.cacheRead ?? 0;
      }
      if (event?.message?.model) capsuleModel = event.message.model;
      capsuleTurnCount++;
      if (capsuleTurnCount % CAPSULE_STATS_EVERY === 0) {
        void patchCapsuleStats();
      }
    }
  });

  pi.on("message_end", async (event: any) => {
    // Capture LLM response as btw reply if a btw question is pending
    if (pendingBtw) {
      const content = event?.message?.content;
      let response = "";
      if (typeof content === "string") {
        response = content;
      } else if (Array.isArray(content)) {
        response = content
          .filter((b: any) => b.type === "text")
          .map((b: any) => b.text)
          .join("\n");
      }
      if (response) {
        publishBtwReply(response);
      }
    }
  });

  pi.on("tool_execution_end", async () => {
    if (capsuleStatsURL) capsuleToolCalls++;
  });

  pi.on("session_compact", async () => {
    if (capsuleStatsURL) capsuleCompactions++;
  });

  pi.on("notification", async (event: any, _ctx: any) => {
    if (event.type === "idle_prompt" || event.matcher === "idle_prompt") {
      natsPublish(eventsSubject("agent_idle"), {
        _project: PROJECT,
        _agentSessionId: SESSION,
        cwd: process.cwd(),
      });
      await publishState("running", "blocked");
    }
  });

  // Pi 0.78 fires session_shutdown (the old session_end is no longer emitted) for
  // reload, quit, and session replacement. Tear down NATS so a /reload — which
  // re-runs session_start and re-subscribes — doesn't leave a duplicate inbox
  // consumer on a stale connection. nc.drain() ends all subscription/consumer
  // iterators cleanly. Only reason "quit" is a real end (announce + clean KV).
  pi.on("session_shutdown", async (event: any) => {
    if (heartbeatTimer) { clearInterval(heartbeatTimer); heartbeatTimer = null; }
    const ending = event?.reason === "quit";
    if (ending && capsuleStatsURL) {
      await patchCapsuleStats();
    }
    if (ending) {
      natsPublish(eventsSubject("session_ended"), {
        _project: PROJECT,
        _agentSessionId: SESSION,
        cwd: process.cwd(),
      });
      if (kvAgents && !PROCESS_NAME) {
        try { await kvAgents.delete(AGENT_ID); } catch {}
      }
      // Keep the durable consumer for process workers (needed to resume after a
      // crash); freeform workers won't return, so drop theirs.
      if (js && !PROCESS_NAME) {
        try {
          const jsm = await js.jetstreamManager();
          await jsm.consumers.delete("pinard-inboxes", `worker-${AGENT_ID}`);
        } catch {}
      }
    }
    const _nc = nc;
    kvAgents = null;
    js = null;
    nc = null;
    inboxSub = null;
    btwSub = null;
    interruptSub = null;
    jsInboxConsumer = null;
    try { if (_nc) await _nc.drain(); } catch {}
  });
}
