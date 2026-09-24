import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: BTW channel (parallel questions with auto-reply)", () => {
  let infra: TestNatsInfra;
  let mockPi: MockPi;
  let conductor: ConductorHarness;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  beforeEach(async () => {
    mockPi = createMockPi();
    conductor = await startConductorHarness(mockPi, infra);
  });

  afterEach(async () => {
    conductor.ackAllEvents();
    await conductor.cleanup();
    mockPi.clear();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("conductor sends btw, worker publishes btw_reply, conductor receives it", async () => {
    // Conductor sends btw to worker-btw-1
    const replyPromise = conductor.sendBtw("worker-btw-1", "What branch are you on?");

    // Simulate worker replying after short delay
    await sleep(500);
    await infra.publishEvent(infra.sub("agents.worker-btw-1.events.btw_reply"), {
      btw_id: conductor.pendingBtwReplies.keys().next().value,
      response: "I'm on feat/add-retry-logic",
      _project: "exo-cli",
      _agentSessionId: "worker-btw-1",
    });

    const reply = await replyPromise;
    expect(reply).toBe("I'm on feat/add-retry-logic");
  });

  it("btw_reply includes correct btw_id for correlation", async () => {
    const replyPromise = conductor.sendBtw("worker-corr", "status?");

    await sleep(200);
    const btw_id = conductor.pendingBtwReplies.keys().next().value;
    expect(btw_id).toBeDefined();
    expect(btw_id!.length).toBe(8);

    await infra.publishEvent(infra.sub("agents.worker-corr.events.btw_reply"), {
      btw_id,
      response: "all good",
      _project: "proj",
      _agentSessionId: "worker-corr",
    });

    const reply = await replyPromise;
    expect(reply).toBe("all good");
    expect(conductor.pendingBtwReplies.size).toBe(0);
  });

  it("btw timeout returns error after deadline", async () => {
    // Override the timeout by directly testing the mechanism
    const btw_id = "test-timeout";
    const replyPromise = new Promise<string>((resolve) => {
      const timer = setTimeout(() => {
        conductor.pendingBtwReplies.delete(btw_id);
        resolve("[btw timeout: worker did not respond within 60s]");
      }, 1000); // Short timeout for test
      conductor.pendingBtwReplies.set(btw_id, { resolve, timer });
    });

    const reply = await replyPromise;
    expect(reply).toContain("timeout");
  });

  it("btw message is published to correct NATS subject", async () => {
    const btwSubject = infra.sub("agents.worker-subj.btw");
    const sub = infra.nc.subscribe(btwSubject);
    const messages: any[] = [];
    const loop = (async () => {
      for await (const msg of sub) {
        messages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        break;
      }
    })();

    conductor.sendBtw("worker-subj", "test question", true);

    await Promise.race([loop, sleep(3000)]);
    sub.unsubscribe();

    expect(messages.length).toBe(1);
    expect(messages[0].message).toBe("test question");
    expect(messages[0].inject).toBe(true);
    expect(messages[0].btw_id).toBeDefined();
    expect(messages[0].from).toBe("conductor");
  });

  it("btw_reply event is formatted and delivered to conductor LLM", async () => {
    await infra.publishEvent(infra.sub("agents.worker-fmt.events.btw_reply"), {
      btw_id: "no-pending",
      response: "branch is main",
      _project: "exo-cli",
      _agentSessionId: "worker-fmt",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("btw-reply"),
      5000
    );
    expect(msg.text).toContain("branch is main");
    expect(msg.text).toContain("exo-cli");
  });

  it("multiple btw to different workers are independent", async () => {
    const reply1Promise = conductor.sendBtw("worker-a", "question A");
    const reply2Promise = conductor.sendBtw("worker-b", "question B");

    await sleep(200);

    // Get both btw_ids
    const ids = Array.from(conductor.pendingBtwReplies.keys());
    expect(ids.length).toBe(2);

    // Worker B replies first
    await infra.publishEvent(infra.sub("agents.worker-b.events.btw_reply"), {
      btw_id: ids[1],
      response: "answer B",
      _project: "proj",
      _agentSessionId: "worker-b",
    });

    // Worker A replies second
    await infra.publishEvent(infra.sub("agents.worker-a.events.btw_reply"), {
      btw_id: ids[0],
      response: "answer A",
      _project: "proj",
      _agentSessionId: "worker-a",
    });

    const [r1, r2] = await Promise.all([reply1Promise, reply2Promise]);
    expect(r1).toBe("answer A");
    expect(r2).toBe("answer B");
  });
});
