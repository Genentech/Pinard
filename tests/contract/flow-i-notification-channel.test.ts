import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Worker notification channel (aoc_notify → NATS → conductor)", () => {
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

  it("notification published to NATS is delivered to conductor LLM", async () => {
    const subject = infra.sub("notifications");
    await infra.publishEvent(subject, {
      message: "[worker-1] Opened MR !42 on exo-cli",
      timestamp: new Date().toISOString(),
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("MR !42"),
      5000
    );
    expect(msg.text).toContain("worker-1");
    expect(msg.text).toContain("MR !42");
  });

  it("notification does NOT dispatch back to the originating worker inbox", async () => {
    // Subscribe to worker-1's inbox to verify nothing arrives
    const inboxSubject = infra.sub("agents.worker-1.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    // Worker-1 sends a notification
    const subject = infra.sub("notifications");
    await infra.publishEvent(subject, {
      message: "[worker-1] Done with the fix",
      timestamp: new Date().toISOString(),
    });

    // Wait for conductor to process
    await sleep(3500);
    inboxSub.unsubscribe();

    // Nothing dispatched back to worker-1's inbox
    expect(inboxMessages).toHaveLength(0);
  });

  it("notification is NOT ACK_REQUIRED (auto-acked)", async () => {
    const subject = infra.sub("notifications");
    await infra.publishEvent(subject, {
      message: "[worker-2] Task complete",
      timestamp: new Date().toISOString(),
    });

    await sleep(2000);

    expect(conductor.pendingAckEvents.length).toBe(0);
  });
});
