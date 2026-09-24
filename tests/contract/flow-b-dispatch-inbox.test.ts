import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Conductor dispatches actionable events to worker inbox", () => {
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
    await conductor.cleanup();
    mockPi.clear();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("pipeline_failed dispatches fix instructions to worker inbox", async () => {
    // Subscribe to the worker's inbox to verify dispatch
    const inboxSubject = infra.sub("agents.worker-1.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxPromise = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        if (inboxMessages.length >= 1) break;
      }
    })();

    // Publish pipeline_failed event
    const subject = infra.sub("agents.worker-1.events.pipeline_failed");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      attempt: 2,
      max: 3,
      url: "https://gitlab.com/pipeline/123",
    });

    // Wait for both delivery to LLM and dispatch to inbox
    await Promise.race([
      inboxPromise,
      sleep(5000),
    ]);
    inboxSub.unsubscribe();

    expect(inboxMessages.length).toBeGreaterThanOrEqual(1);
    expect(inboxMessages[0].message).toContain("CI pipeline failed");
    expect(inboxMessages[0].message).toContain("MR !42");
    expect(inboxMessages[0].from).toBe("conductor");
  });

  it("review_comment dispatches to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-2.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxPromise = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        if (inboxMessages.length >= 1) break;
      }
    })();

    const subject = infra.sub("agents.worker-2.events.review_comment");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      message: "Please fix the null check on line 55",
    });

    await Promise.race([inboxPromise, sleep(5000)]);
    inboxSub.unsubscribe();

    expect(inboxMessages.length).toBeGreaterThanOrEqual(1);
    expect(inboxMessages[0].message).toContain("null check on line 55");
  });

  it("mr_merged does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-3.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-3.events.mr_merged");
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });

    // Wait for conductor to process
    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });

  it("agent_idle does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-4.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-4.events.agent_idle");
    await infra.publishEvent(subject, {
      _project: "exo-cli",
      _agentSessionId: "worker-4",
    });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });
});
