import { type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { execSync, execFileSync } from "node:child_process";
import { existsSync } from "node:fs";
import path from "node:path";

function escapeSingleQuotes(s: string): string {
  return s.replace(/'/g, `'\\''`);
}

const PROCESS = process.env.BABYSITTER_PROCESS || "";
const PROCESS_PATH = process.env.BABYSITTER_PROCESS_PATH || "";
const PROCESS_ARGS = process.env.BABYSITTER_ARGS || "";
const PINARD_REPO = process.env.PINARD_REPO || "";
const RUNS_DIR = process.env.BABYSITTER_RUNS_DIR || "";
const RUN_ID = process.env.BABYSITTER_RUN_ID || "";
const PARCELLE = process.env.BABYSITTER_PARCELLE || "";
const GITLAB_HOST = process.env.GITLAB_HOST || "";
const PINARD_CAPSULE_CONTRACT = process.env.PINARD_CAPSULE_CONTRACT || "";
// When set (1/true/yes), ctx.breakpoint approval gates are auto-approved without
// operator interaction — for fully autonomous/unattended runs (e.g. HPC workers
// with no one attached to answer). Default off: breakpoints are delivered to the
// agent, which asks the operator and posts their decision.
const BREAKPOINT_AUTO_APPROVE = /^(1|true|yes)$/i.test(process.env.PINARD_BREAKPOINT_AUTO_APPROVE || "");

const BABYSITTER_CLI = PINARD_REPO
  ? path.join(PINARD_REPO, "deps/babysitter/packages/sdk/dist/cli/main.js")
  : "";

export default function babysitter(pi: ExtensionAPI) {
  if (!PROCESS) {
    return;
  }

  if (!PROCESS_PATH) {
    console.error(`[babysitter] BABYSITTER_PROCESS_PATH not set — process resolution failed`);
    return;
  }

  if (!BABYSITTER_CLI) {
    console.error(`[babysitter] PINARD_REPO not set — cannot locate babysitter CLI`);
    return;
  }

  const GITLAB_TOKEN = process.env.GITLAB_TOKEN || process.env.GLAB_TOKEN || "";
  if (!GITLAB_TOKEN) {
    console.error(`[babysitter] WARNING: No GITLAB_TOKEN or GLAB_TOKEN set — git host operations via glab will fail (aoc pressoir commands use their own token resolution)`);
  }

  let runDir: string | null = null;
  let currentEffectId: string | null = null;
  let waitingForEvent = false;
  let processFinished = false;
  let taskDispatched = false;
  let uiCtx: any = null;

  function bsCli(command: string): string {
    return execSync(`node ${BABYSITTER_CLI} ${command}`, {
      encoding: "utf8",
      cwd: process.cwd(),
      timeout: 30_000,
    }).trim();
  }

  // NOTE: `stateRebuildReason: "journal_mismatch"` is a ROUTINE state-cache
  // rebuild — the engine rebuilds state from the journal and continues; it is NOT
  // fatal and occurs on the vast majority of runs (182+ historically, including
  // runs that completed and merged). Prior handling keyed on this benign signal and
  // broke every swe worker: #62/!124 auto-forked (stacking `-fork-<hash>` → runaway),
  // then !127 failed loud (hard halt). Both were wrong. We do nothing special here
  // — let the engine rebuild-and-continue, exactly as it did pre-#62. A genuine
  // structural process edit corrupting a standalone multi-week resume (real Bug B)
  // needs a NARROW structure-change detector, not this blanket rebuild signal.

  function findOrCreateRun(): string {
    const fs = require("node:fs");
    const runsDir = RUNS_DIR || path.join(process.cwd(), ".a5c", "runs");

    // Check for existing run to resume
    if (RUN_ID) {
      const existingRunDir = path.join(runsDir, RUN_ID);
      const runJson = path.join(existingRunDir, "run.json");
      if (fs.existsSync(runJson)) {
        // Check if already completed
        const statusOutput = bsCli(`run:status "${existingRunDir}" --json`);
        const status = JSON.parse(statusOutput);
        if (status.state === "completed") {
          console.error(`[babysitter] Run ${RUN_ID} already completed — skipping`);
          processFinished = true;
          return existingRunDir;
        }
        console.error(`[babysitter] Resuming existing run: ${RUN_ID}`);
        return existingRunDir;
      }
    }

    // Create new run
    let cmd = `run:create --process-id ${PROCESS} --entry "${PROCESS_PATH}#process" --runs-dir "${runsDir}" --json`;
    if (RUN_ID) {
      cmd += ` --run-id "${RUN_ID}"`;
    }
    if (PROCESS_ARGS) {
      const inputsFile = path.join(runsDir, "process-inputs.json");
      fs.mkdirSync(path.dirname(inputsFile), { recursive: true });
      fs.writeFileSync(inputsFile, PROCESS_ARGS);
      cmd += ` --inputs "${inputsFile}"`;
    }
    const output = bsCli(cmd);
    const result = JSON.parse(output);
    return result.runDir || result.run_dir;
  }

  function iterate(): { status: string; nextActions?: any[]; completionProof?: string } {
    const output = bsCli(`run:iterate "${runDir}" --json`);
    return JSON.parse(output);
  }

  const SESSION = process.env.WORKER_SESSION || "unknown";

  // Colored status dot by babysitter state: 🟢 done · 🔴 failed · 🟡 waiting · 🔵 working.
  function stateDot(state: string): string {
    if (state === "completed") return "🟢";
    if (state === "failed") return "🔴";
    if (state.startsWith("awaiting")) return "🟡";
    return "🔵";
  }
  function updateStatus(state: string): void {
    uiCtx?.ui?.setStatus?.("process", `process: ${PROCESS}    steps: ${stateDot(state)} ${state}`);
    pi.setSessionName?.(`🧺 ${SESSION}`);
    pi.emit?.("babysitter:status", { process: PROCESS, state });
  }

  // Derive a stable fork run-id: strip any existing -fork-<hex> suffix from the
  // base id, then append -fork-<8-char hash of the process file content>.
  // This is idempotent — re-forking from the same base+process always produces
  // the same id, never stacks suffixes.
  function deriveForkRunId(currentRunDir: string): string {
    const crypto = require("node:crypto");
    const fs = require("node:fs");
    const runsDir = RUNS_DIR || path.join(process.cwd(), ".a5c", "runs");
    const currentId = path.basename(currentRunDir);
    const baseId = currentId.replace(/-fork-[0-9a-f]+$/i, "");
    let processHash = "00000000";
    try {
      const src = fs.readFileSync(PROCESS_PATH, "utf8");
      processHash = crypto.createHash("sha256").update(src).digest("hex").slice(0, 8);
    } catch { /* fall back to fixed suffix */ }
    return path.join(runsDir, `${baseId}-fork-${processHash}`);
  }

  // forkAttempted prevents recursive fork loops: if the forked run also fails
  // with a structural error, we fail-loud rather than forking again.
  let forkAttempted = false;

  function driveIteration(): void {
    if (!runDir || processFinished) return;

    let result: ReturnType<typeof iterate>;
    try {
      result = iterate();
    } catch (e: any) {
      const msg: string = e.message || "";
      const isStructural =
        msg.includes("Duplicate invocation key") ||
        msg.includes("Journal sequence gap") ||
        msg.includes("Journal seq mismatch") ||
        msg.includes("Journal ULID order regression") ||
        msg.includes("Journal sequence regression");
      if (isStructural && !forkAttempted) {
        forkAttempted = true;
        const forkDir = deriveForkRunId(runDir);
        const forkId = path.basename(forkDir);
        console.error(
          `[babysitter] Structural journal mismatch detected — forking to ${forkId}
` +
          `  (process structure changed since this run was created)
` +
          `  Original error: ${msg}`,
        );
        try {
          const fs = require("node:fs");
          const runsDir = RUNS_DIR || path.join(process.cwd(), ".a5c", "runs");
          let cmd = `run:create --process-id ${PROCESS} --entry "${PROCESS_PATH}#process" --runs-dir "${runsDir}" --run-id "${forkId}" --json`;
          if (PROCESS_ARGS) {
            const inputsFile = path.join(runsDir, "process-inputs.json");
            fs.mkdirSync(path.dirname(inputsFile), { recursive: true });
            fs.writeFileSync(inputsFile, PROCESS_ARGS);
            cmd += ` --inputs "${inputsFile}"`;
          }
          const createOutput = bsCli(cmd);
          const created = JSON.parse(createOutput);
          runDir = created.runDir || created.run_dir || forkDir;
          console.error(`[babysitter] Forked to new run: ${runDir}`);
        } catch (forkErr: any) {
          console.error(`[babysitter] Fork failed: ${forkErr.message} — halting`);
          processFinished = true;
        }
        return;
      }
      if (isStructural && forkAttempted) {
        console.error(
          `[babysitter] FATAL: structural mismatch persists after fork — halting.
` +
          `  The forked run still cannot replay the journal. Manual intervention needed.
` +
          `  Run dir: ${runDir}
  Error: ${msg}`,
        );
        processFinished = true;
        return;
      }
      console.error(`[babysitter] run:iterate failed: ${msg}`);
      return;
    }

    // (journal_mismatch after iterate is a routine state-cache rebuild — the
    // engine already rebuilt from the journal; continue. See NOTE above.)

    if (result.status === "completed") {
      processFinished = true;
      updateStatus("completed");
      console.error(`[babysitter] Process completed`);
      pi.emit?.("babysitter:process_completed", { process: PROCESS });
      setTimeout(() => process.exit(0), 1000);
      return;
    }

    if (result.status === "failed") {
      processFinished = true;
      updateStatus("failed");
      console.error(`[babysitter] Process failed`);
      pi.emit?.("babysitter:process_failed", { process: PROCESS });
      return;
    }

    if (result.status === "waiting" || result.status === "executed") {
      const actions = result.nextActions || [];
      for (const action of actions) {
        currentEffectId = action.effectId;

        if (action.kind === "event") {
          waitingForEvent = true;
          const eventTypes = action.taskDef?.event?.types || [];
          updateStatus(`awaiting ${eventTypes.join("|")}`);
          // Signal worker extension via file (pi.emit doesn't cross extensions)
          const fs = require("node:fs");
          const signalFile = path.join(process.cwd(), ".babysitter-event-wait.json");
          fs.writeFileSync(signalFile, JSON.stringify({ effectId: action.effectId, runDir, eventTypes }));
          pi.emit?.("babysitter:waiting_for_event", {
            effectId: action.effectId,
            runDir,
            eventTypes,
          });
          console.error(`[babysitter] Waiting for event effect ${action.effectId}`);
          return;
        }

        if (action.kind === "breakpoint") {
          // Question/options live under taskDef.metadata.payload in the current SDK;
          // fall back to the older taskDef.breakpoint path and the title.
          const payload = action.taskDef?.metadata?.payload || {};
          const question: string =
            payload.question || action.taskDef?.breakpoint?.question || action.taskDef?.title || "Approval needed";
          const options: string[] = Array.isArray(payload.options) ? payload.options : ["Approve", "Reject"];
          pi.emit?.("babysitter:breakpoint", { effectId: action.effectId, question, options });

          // Auto-approve mode (unattended): resolve the gate immediately and keep
          // driving — no operator interaction. Consecutive gates recurse (few).
          if (BREAKPOINT_AUTO_APPROVE) {
            console.error(`[babysitter] Breakpoint auto-approved (PINARD_BREAKPOINT_AUTO_APPROVE): ${question.slice(0, 80)}`);
            updateStatus(`auto-approved: ${action.taskDef?.title || "breakpoint"}`);
            try {
              bsCli(
                `task:post "${runDir}" ${action.effectId} --status ok --value-inline '{"approved":true,"response":"auto-approved (PINARD_BREAKPOINT_AUTO_APPROVE)"}'`,
              );
            } catch (e: any) {
              console.error(`[babysitter] Auto-approve task:post failed: ${e.message || e} — halting`);
              processFinished = true;
              return;
            }
            currentEffectId = null;
            driveIteration();
            return;
          }

          // Interactive: deliver the gate to the agent so it asks the operator and
          // posts the decision. Treated like a task — turn_end waits for the
          // EFFECT_RESOLVED before iterating.
          updateStatus(`awaiting approval: ${action.taskDef?.title || "breakpoint"}`);
          taskDispatched = true;
          console.error(`[babysitter] Breakpoint dispatched to agent: ${question.slice(0, 80)}`);
          pi.sendUserMessage(formatBreakpointPrompt(action, question, options), {
            deliverAs: initialized ? "followUp" : "steer",
          });
          return;
        }

        if (action.kind === "node") {
          driveNodeTask(action);
          return;
        }

        // Agent or shell task — inject as user message for LLM to execute
        const prompt = formatTaskPrompt(action);
        const taskTitle = action.taskDef?.title || action.taskId || action.effectId;
        updateStatus(taskTitle);
        taskDispatched = true;
        console.error(`[babysitter] Dispatching task: ${taskTitle}`);
        pi.sendUserMessage(prompt, { deliverAs: initialized ? "followUp" : "steer" });
        return;
      }
    }
  }

  // Deterministic `kind:'node'` tasks (issue #342) run a plain script — no LLM
  // turn, no chance of the model skipping or misreporting the check. Mirrors
  // the auto-approve breakpoint path: execute synchronously, post the result,
  // and keep driving the iteration loop.
  function driveNodeTask(action: any): void {
    const node = action.taskDef?.node || {};
    const entry: string = node.entry || "";
    const nodeArgs: string[] = Array.isArray(node.args) ? node.args : [];
    const nodeEnv: Record<string, string> = node.env || {};
    const nodeCwd: string = node.cwd || process.cwd();
    const timeoutMs: number = typeof node.timeoutMs === "number" ? node.timeoutMs : 15 * 60 * 1000;
    const taskTitle = action.taskDef?.title || action.taskId || action.effectId;
    updateStatus(taskTitle);
    console.error(`[babysitter] Auto-executing node task: ${taskTitle} (${entry})`);

    let entryPath: string;
    try {
      entryPath = resolveNodeEntry(entry);
    } catch (e: any) {
      console.error(`[babysitter] Node task ${action.effectId}: ${e.message}`);
      postNodeTaskError(action.effectId, e.message);
      return;
    }
    try {
      const stdout = execFileSync(process.execPath, [entryPath, ...nodeArgs], {
        cwd: nodeCwd,
        env: { ...process.env, ...nodeEnv },
        timeout: timeoutMs,
        encoding: "utf8",
      });
      const trimmed = stdout.trim();
      const value = trimmed.length ? JSON.parse(trimmed) : {};
      bsCli(`task:post "${runDir}" ${action.effectId} --status ok --value-inline '${escapeSingleQuotes(JSON.stringify(value))}'`);
    } catch (e: any) {
      const message = e.message || String(e);
      console.error(`[babysitter] Node task ${action.effectId} (${entry}) failed: ${message}`);
      postNodeTaskError(action.effectId, message);
      return;
    }
    currentEffectId = null;
    driveIteration();
  }

  // A relative node-task `entry` is resolved against process.cwd() first (the
  // worker's own worktree — the common case for a script that ships inside a
  // per-project override). If that doesn't exist, fall back to resolving
  // against the running process file's own directory (BABYSITTER_PROCESS_PATH):
  // process files like swe.js are shared across every project's spawns via
  // $PINARD_REPO, and a relative entry hard-coded there must resolve to
  // *that* file's location, not whatever project happens to be checked out
  // as cwd. Throws naming both paths tried when neither exists.
  function resolveNodeEntry(entry: string): string {
    if (path.isAbsolute(entry)) return entry;
    const cwdCandidate = path.join(process.cwd(), entry);
    if (existsSync(cwdCandidate)) return cwdCandidate;
    if (PROCESS_PATH) {
      const processDirCandidate = path.join(path.dirname(PROCESS_PATH), entry);
      if (existsSync(processDirCandidate)) return processDirCandidate;
      throw new Error(
        `node task entry "${entry}" not found at "${cwdCandidate}" or "${processDirCandidate}"`,
      );
    }
    throw new Error(`node task entry "${entry}" not found at "${cwdCandidate}" (BABYSITTER_PROCESS_PATH unset, no fallback tried)`);
  }

  function postNodeTaskError(effectId: string, message: string): void {
    const fs = require("node:fs");
    const os = require("node:os");
    const errorFile = path.join(os.tmpdir(), `babysitter-node-error-${effectId}.json`);
    fs.writeFileSync(errorFile, JSON.stringify({ name: "NodeTaskError", message }));
    try {
      bsCli(`task:post "${runDir}" ${effectId} --status error --error "${errorFile}"`);
    } catch (postErr: any) {
      console.error(`[babysitter] Failed to post node task failure: ${postErr.message || postErr} — halting`);
      processFinished = true;
    }
  }

  function formatTaskPrompt(action: any): string {
    const taskDef = action.taskDef || {};
    const agent = taskDef.agent || {};
    const prompt = agent.prompt || {};

    let message = `[babysitter task — effectId: ${action.effectId}]\n\n`;

    if (taskDef.title) {
      message += `## ${taskDef.title}\n\n`;
    }

    if (prompt.role) {
      message += `You are: ${prompt.role}\n\n`;
    }

    if (prompt.task) {
      message += `Task: ${prompt.task}\n\n`;
    }

    if (prompt.instructions && Array.isArray(prompt.instructions)) {
      message += `Instructions:\n${prompt.instructions.map((i: string) => `- ${i}`).join("\n")}\n\n`;
    }

    if (prompt.context) {
      message += `Context: ${JSON.stringify(prompt.context)}\n\n`;
    }

    if (agent.outputSchema) {
      message += `Output format (respond with JSON matching this schema):\n\`\`\`json\n${JSON.stringify(agent.outputSchema, null, 2)}\n\`\`\`\n\n`;
    }

    message += `When done, run: node ${BABYSITTER_CLI} task:post "${runDir}" ${action.effectId} --status ok --value-inline '<your JSON result>'`;

    return message;
  }

  // Breakpoints are HUMAN APPROVAL GATES. Deliver the question to the agent so it
  // asks the operator (interactive) and records their verbatim decision. Never let
  // the agent decide on its own or infer approval from an empty/ambiguous answer.
  //
  // The verdict is OPTION-AWARE: the agent must pass the operator's verbatim
  // selected option in the `option` field so that process.js gates can
  // distinguish Skip / Abort / Approve by name (e.g. `gate.option.includes('skip')`).
  // approved=true for any non-abort selection; approved=false only for Abort.
  function formatBreakpointPrompt(action: any, question: string, options: string[]): string {
    const optList = options.map((o) => `- ${o}`).join("\n");

    // Identify the abort option (case-insensitive), if any.
    const abortOption = options.find((o) => /abort/i.test(o));

    // Build per-option payload examples for the agent.
    const exampleLines: string[] = [];
    for (const opt of options) {
      const isAbort = abortOption && opt === abortOption;
      const approvedVal = isAbort ? "false" : "true";
      exampleLines.push(
        `  # ${opt}:`,
        `  node ${BABYSITTER_CLI} task:post "${runDir}" ${action.effectId} --status ok --value-inline '{"approved":${approvedVal},"option":"${opt}","response":"<their words>"}'`,
      );
    }

    return [
      `[babysitter breakpoint — HUMAN APPROVAL REQUIRED — effectId: ${action.effectId}]`,
      ``,
      question,
      ``,
      `Options:`,
      optList,
      ``,
      `Ask the operator for their decision using an interactive question (present the`,
      `options verbatim). Do NOT decide yourself and do NOT start any work.`,
      `- Pass through ONLY the operator's actual selection. Never fabricate or infer approval.`,
      `- Empty / dismissed / ambiguous response = NOT approved (re-ask or keep pending).`,
      ``,
      `Record the decision with --status ok for ALL options (only use --status error if`,
      `the question tool itself errored). Always include the verbatim option text:`,
      ...exampleLines,
      ``,
      `After posting, STOP and wait for the next task.`,
    ].join("\n");
  }

  // --- Hooks ---

  let initialized = false;

  function injectBootContext(): void {
    const vignoble = process.env.NATS_VIGNOBLE || "";
    const groupId = process.env.WORKER_PROJECT || process.env.BABYSITTER_PARCELLE || "";
    if (!vignoble || !groupId) {
      console.error("[babysitter] Boot inject: NATS_VIGNOBLE or WORKER_PROJECT not set — skipping");
      return;
    }

    // Derive task text from process args (issue title/description if available,
    // or spawn prompt). Falls back to empty string for broad group knowledge.
    let taskText = "";
    try {
      if (PROCESS_ARGS) {
        const args = JSON.parse(PROCESS_ARGS);
        taskText = args.prompt || args.title || "";
        // If issue-driven, prefer the issue description over a bare title.
        if (args.issueId && !taskText) {
          taskText = `issue ${args.issueId} ${args.project || ""}`.trim();
        }
      }
    } catch { /* ignore */ }

    try {
      const taskArg = taskText ? `--task ${JSON.stringify(taskText)}` : "";
      const cmd = `aoc memory-boot-context --vignoble ${JSON.stringify(vignoble)} --group-id ${JSON.stringify(groupId)} ${taskArg} --timeout 6000`.trim();
      const block = execSync(cmd, { encoding: "utf8", timeout: 7_000 }).trim();
      if (block) {
        console.error("[babysitter] Boot inject: injecting knowledge block");
        pi.sendUserMessage(block, { deliverAs: "steer" });
      } else {
        console.error("[babysitter] Boot inject: no knowledge available");
      }
    } catch (e: any) {
      console.error(`[babysitter] Boot inject failed (non-fatal): ${e.message || e}`);
    }
  }

  function registerWorkerOnPressoir(): void {
    if (!PROCESS_ARGS) return;
    try {
      const args = JSON.parse(PROCESS_ARGS);
      const repo = args.repo || "";
      const issueId = args.issueId || args.issue || "";
      if (!repo) return;

      let comment = `🧺 **Vendangeur attached**\n- Session: ${SESSION}\n- Process: ${PROCESS}\n- Parcelle: ${PARCELLE}\n- Run ID: ${RUN_ID}\n- Started: ${new Date().toISOString()}`;
      // Append a read-only web-terminal link so reviewers can watch this
      // vendangeur live. `aoc webterm-link --auto` prints the link (unsigned +
      // SSO-gated, or signed+expiring) or nothing when webterm/post_links is off.
      try {
        const link = execSync(`aoc webterm-link --target ${JSON.stringify(SESSION)} --auto`, { encoding: "utf8", timeout: 10_000 }).trim();
        if (link) comment += `\n- 🖥️ [Live terminal](${link}) (read-only)`;
      } catch { /* webterm not configured — post the comment without a link */ }

      // Comment on issue
      if (issueId) {
        try {
          execSync(`aoc pressoir comment-issue --repo ${JSON.stringify(repo)} --number ${issueId} --body ${JSON.stringify(comment)}`, { encoding: "utf8", timeout: 10_000 });
          console.error(`[babysitter] Registered on issue #${issueId}`);
        } catch (e: any) {
          console.error(`[babysitter] Failed to register on issue #${issueId}: ${e.message || e}`);
        }
      }

      // Comment on PR (find from journal if exists)
      const fs = require("node:fs");
      if (runDir) {
        const tasksDir = path.join(runDir, "tasks");
        if (fs.existsSync(tasksDir)) {
          for (const effectDir of fs.readdirSync(tasksDir)) {
            const resultFile = path.join(tasksDir, effectDir, "result.json");
            if (fs.existsSync(resultFile)) {
              try {
                const result = JSON.parse(fs.readFileSync(resultFile, "utf8"));
                if (result.taskId === "open-mr" && result.value?.mrIid) {
                  execSync(`aoc pressoir comment-pr --repo ${JSON.stringify(repo)} --number ${result.value.mrIid} --body ${JSON.stringify(comment)}`, { encoding: "utf8", timeout: 10_000 });
                  console.error(`[babysitter] Registered on PR #${result.value.mrIid}`);
                  break;
                }
              } catch (e: any) {
                console.error(`[babysitter] Failed to register on PR: ${e.message || e}`);
              }
            }
          }
        }
      }
    } catch (e: any) {
      console.error(`[babysitter] Registration failed: ${e.message}`);
    }
  }

  pi.on("session_start", async (_event: any, ctx: any) => {
    uiCtx = ctx;
    try {
      runDir = findOrCreateRun();
      if (processFinished) {
        console.error(`[babysitter] Run ${RUN_ID} already completed — exiting`);
        setTimeout(() => process.exit(0), 500);
        return;
      }
      console.error(`[babysitter] Run ready: ${runDir}`);

      // Register worker on issue/MR for traceability (runs on every spawn/resume)
      registerWorkerOnPressoir();

      // Boot injection: seed agent with accumulated scope knowledge before the
      // first task. Best-effort — any failure is logged and silently skipped.
      injectBootContext();

      // Check if resuming with a pending event effect — go straight to waiting
      try {
        const fs = require("node:fs");
        const statusOutput = bsCli(`run:status "${runDir}" --json`);
        const status = JSON.parse(statusOutput);

        // (journal_mismatch in the state cache on resume is a routine rebuild —
        // the engine rebuilds from the journal and continues. See NOTE above.)

        const pendingByKind = status.pendingEffectsSummary?.countsByKind || {};
        if (pendingByKind.event > 0) {
          waitingForEvent = true;
          initialized = true;
          // Find pending event effect from journal (last EFFECT_REQUESTED without matching RESOLVED)
          const journalDir = path.join(runDir, "journal");
          const files = fs.readdirSync(journalDir).sort().reverse();
          for (const file of files) {
            const entry = JSON.parse(fs.readFileSync(path.join(journalDir, file), "utf8"));
            if (entry.type === "EFFECT_REQUESTED" && entry.data?.kind === "event") {
              currentEffectId = entry.data.effectId;
              // Read event types from task definition
              let eventTypes: string[] = [];
              try {
                const taskDefPath = path.join(runDir, entry.data.taskDefRef || `tasks/${entry.data.effectId}/task.json`);
                const taskDef = JSON.parse(fs.readFileSync(taskDefPath, "utf8"));
                eventTypes = taskDef.event?.types || [];
              } catch {}
              const signalFile = path.join(process.cwd(), ".babysitter-event-wait.json");
              fs.writeFileSync(signalFile, JSON.stringify({ effectId: entry.data.effectId, runDir, eventTypes }));
              pi.emit?.("babysitter:waiting_for_event", {
                effectId: entry.data.effectId,
                runDir,
                eventTypes: [],
              });
              updateStatus(entry.data.label || "awaiting events");
              break;
            }
          }
          console.error(`[babysitter] Resuming in event-wait state (effectId: ${currentEffectId})`);
        }
      } catch (e: any) {
        console.error(`[babysitter] Event-wait detection failed: ${e.message || e}`);
      }
    } catch (e: any) {
      console.error(`[babysitter] Failed to initialize run: ${e.message}`);
    }
  });

  pi.on("turn_end", async () => {
    if (!runDir || processFinished) return;

    // Check if event was resolved by worker extension (signal file deleted)
    if (waitingForEvent) {
      const fs = require("node:fs");
      const signalFile = path.join(process.cwd(), ".babysitter-event-wait.json");
      if (!fs.existsSync(signalFile)) {
        waitingForEvent = false;
        currentEffectId = null;
        console.error(`[babysitter] Event resolved (signal file removed)`);
        driveIteration();
      }
      return;
    }
    if (!initialized) {
      initialized = true;
      driveIteration();
      return;
    }
    // A task is dispatched — only iterate once the LLM has posted the result.
    // Check if the effect is still pending in the journal.
    if (taskDispatched && currentEffectId) {
      const fs = require("node:fs");
      try {
        const journalDir = path.join(runDir, "journal");
        const files = fs.readdirSync(journalDir).sort();
        let resolved = false;
        for (const file of files) {
          const entry = JSON.parse(fs.readFileSync(path.join(journalDir, file), "utf8"));
          if (entry.type === "EFFECT_RESOLVED" && entry.data?.effectId === currentEffectId) {
            resolved = true;
            break;
          }
        }
        if (!resolved) return;
      } catch {
        return;
      }
      taskDispatched = false;
      currentEffectId = null;
    }
    driveIteration();
  });

  // Listen for event resolution from worker extension
  pi.on?.("babysitter:event_resolved", () => {
    waitingForEvent = false;
    currentEffectId = null;
    driveIteration();
  });

  // ── Capsule exhaustion handler ──────────────────────────────────────────────
  // When `aoc capsule-redeem` returns 410/404 (budget spent), it writes
  // <runDir>/capsule-exhausted.json before exiting non-zero. Pi surfaces that
  // as a provider failure which ends the current turn. We check for the marker
  // on every turn_end so we can park + report using our own GITLAB_TOKEN (which
  // is always available, unlike the funder's quota). Idempotent: the marker
  // is removed after the first successful park so we never post a second note
  // or repeat the PATCH on session resume.

  function handleCapsuleExhaustion(currentRunDir: string): void {
    const fs = require("node:fs");
    const markerPath = path.join(currentRunDir, "capsule-exhausted.json");
    if (!fs.existsSync(markerPath)) return;
    if (!PINARD_CAPSULE_CONTRACT) return;

    const GITLAB_TOKEN_VAL = process.env.GITLAB_TOKEN || process.env.GLAB_TOKEN || "";

    let contractID = PINARD_CAPSULE_CONTRACT;
    try {
      const marker = JSON.parse(fs.readFileSync(markerPath, "utf8"));
      contractID = marker.contract_id || contractID;
    } catch { /* use env contractID */ }

    // Load capsule.json to get result_patch_url, result_url, description.
    let capsuleData: any = null;
    const capsuleJsonPath = path.join(currentRunDir, "capsule.json");
    if (fs.existsSync(capsuleJsonPath)) {
      try { capsuleData = JSON.parse(fs.readFileSync(capsuleJsonPath, "utf8")); } catch { /* ignore */ }
    }

    // Derive issue repo + IID from PROCESS_ARGS.
    let encodedRepo = "";
    let issueIid = 0;
    try {
      if (PROCESS_ARGS) {
        const args = JSON.parse(PROCESS_ARGS);
        const repo = args.repo || args.encodedRepo || "";
        encodedRepo = args.encodedRepo || encodeURIComponent(repo);
        issueIid = parseInt(args.issueId || args.issue || "0", 10);
      }
    } catch { /* skip */ }

    const description = capsuleData?.description || `contract \`${contractID}\``;
    console.error(`[babysitter] \u26fd Capsule budget exhausted — parking run (contract=${contractID})`);

    // 1. Post issue note (uses GITLAB_TOKEN — always available, not funder quota).
    if (GITLAB_TOKEN_VAL && encodedRepo && issueIid) {
      const note = `\u26fd **Ran out of funder budget mid-run.** Paused. Re-fund the contract to resume.\n\n` +
        `Contract: \`${contractID}\`` +
        (capsuleData?.result_url ? `\n[View on Mnemosyne](${capsuleData.result_url})` : "");
      try {
        const noteBody = JSON.stringify({ body: note });
        execSync(
          `curl -s -X POST "https://${GITLAB_HOST}/api/v4/projects/${encodedRepo}/issues/${issueIid}/notes"` +
          ` -H "PRIVATE-TOKEN: ${GITLAB_TOKEN_VAL}" -H "Content-Type: application/json"` +
          ` -d '${noteBody.replace(/'/g, "'\\''")}' >/dev/null`,
          { encoding: "utf8", timeout: 15_000 }
        );
        console.error(`[babysitter] Posted exhaustion note on issue #${issueIid}`);
      } catch (e: any) {
        console.error(`[babysitter] Failed to post exhaustion note: ${e.message}`);
      }
    }

    // 2. PATCH result_patch_url with paused-status HTML (authless capability URL).
    if (capsuleData?.result_patch_url) {
      const pausedHTML = [
        `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8">`,
        `<title>Capsule Paused \u2014 ${description}</title>`,
        `<style>body{font-family:sans-serif;max-width:600px;margin:4rem auto;padding:0 1rem;color:#2c2c2c}`,
        `.badge{display:inline-block;background:#c9a84c;color:#fff;padding:.3rem .8rem;border-radius:3px;font-size:.85rem;margin-bottom:1.5rem}`,
        `h1{color:#6b2737;font-size:1.4rem;font-weight:normal;margin-bottom:.5rem}`,
        `code{background:#eee;padding:.1em .35em;border-radius:2px;font-size:.85em}</style></head><body>`,
        `<div class="badge">\u26fd Paused \u2014 Budget Exhausted</div>`,
        `<h1>${description}</h1>`,
        `<p>The funder's budget was fully consumed mid-run. The agent has been paused.</p>`,
        `<p>Contract: <code>${contractID}</code></p>`,
        `<p>Re-fund this contract to resume the run. The agent will pick up where it left off.</p>`,
        `<footer style="margin-top:2rem;font-size:.78rem;color:#999">Generated by Pinard \u00b7 ${contractID}</footer>`,
        `</body></html>`,
      ].join("");
      const patchOps = JSON.stringify([
        { op: "replace", path: "/content_type", value: "text/html" },
        { op: "replace", path: "/data", value: pausedHTML },
      ]);
      try {
        execSync(
          `curl -s -X PATCH "${capsuleData.result_patch_url}"` +
          ` -H "Content-Type: application/json"` +
          ` -d '${patchOps.replace(/'/g, "'\\''")}' >/dev/null`,
          { encoding: "utf8", timeout: 15_000 }
        );
        console.error(`[babysitter] PATCHed result_patch_url with paused-status HTML`);
      } catch (e: any) {
        console.error(`[babysitter] Failed to PATCH result_patch_url: ${e.message}`);
      }
    }

    // 3. Relabel: capsule:active → capsule:exhausted + capsule:awaiting-funding.
    //    This hands off back to CapsulePoller so resume-on-refund is automatic.
    if (GITLAB_TOKEN_VAL && encodedRepo && issueIid) {
      try {
        const labelBody = JSON.stringify({
          remove_labels: "capsule:active",
          add_labels: "capsule:exhausted,capsule:awaiting-funding",
        });
        execSync(
          `curl -s -X PUT "https://${GITLAB_HOST}/api/v4/projects/${encodedRepo}/issues/${issueIid}"` +
          ` -H "PRIVATE-TOKEN: ${GITLAB_TOKEN_VAL}" -H "Content-Type: application/json"` +
          ` -d '${labelBody.replace(/'/g, "'\\''")}' >/dev/null`,
          { encoding: "utf8", timeout: 15_000 }
        );
        console.error(`[babysitter] Relabeled issue #${issueIid}: capsule:active \u2192 capsule:exhausted + capsule:awaiting-funding`);
      } catch (e: any) {
        console.error(`[babysitter] Failed to relabel issue: ${e.message}`);
      }
    }

    // 4. Remove the marker so this handler is idempotent across session restarts.
    //    On resume after refund, the marker is gone so we skip the park flow.
    try {
      fs.unlinkSync(markerPath);
      console.error(`[babysitter] Removed exhaustion marker (idempotent guard)`);
    } catch { /* best-effort */ }
  }

  // Check exhaustion on every turn_end (catches budget failure mid-turn).
  pi.on("turn_end", async () => {
    if (runDir && PINARD_CAPSULE_CONTRACT) {
      handleCapsuleExhaustion(runDir);
    }
  });
}
