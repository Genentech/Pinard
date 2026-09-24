import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: ACK_REQUIRED events use pending queue with heartbeat", () => {
  let infra: TestNatsInfra;
  let mockPi: MockPi;
  let conductor: ConductorHarness;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  beforeEach(async () => {
    mockPi = createMockPi();
    conductor = await startConductorHarness(mockPi, infra, {
      schedulerAckWait: 8 * 1_000_000_000,
    });
  });

  afterEach(async () => {
    conductor.ackAllEvents();
    await conductor.cleanup();
    mockPi.clear();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("schedule_spawned enters pending queue and does NOT ack immediately", async () => {
    const subject = infra.sub("schedules.nightly-sync.spawned");
    await infra.publishEvent(subject, {
      schedule: "nightly-sync",
      _project: "exo-cli",
    });

    await sleep(1000);

    // Event should be in pending queue
    expect(conductor.pendingAckEvents.length).toBeGreaterThanOrEqual(1);
    const pe = conductor.pendingAckEvents[0];
    expect(pe.type).toBe("schedule_spawned");

    // Message was still delivered to LLM
    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("nightly-sync"),
      3000
    );
    expect(msg.text).toContain("Schedule nightly-sync fired");
  });

  it("ack_event removes from pending queue and acks NATS message", async () => {
    const subject = infra.sub("schedules.daily-check.spawned");
    await infra.publishEvent(subject, {
      schedule: "daily-check",
      _project: "charon",
    });

    await sleep(1000);
    expect(conductor.pendingAckEvents.length).toBeGreaterThanOrEqual(1);

    const eventId = conductor.pendingAckEvents[0].id;
    const result = conductor.ackEvent(eventId);
    expect(result).toBe(true);
    expect(conductor.pendingAckEvents.length).toBe(0);
  });

  it("invalid ack ID returns false", () => {
    const result = conductor.ackEvent("nonexistent-id");
    expect(result).toBe(false);
  });

  it("ackAllEvents clears entire queue", async () => {
    await infra.publishEvent(infra.sub("schedules.sched-1.spawned"), {
      _project: "a",
    });
    await infra.publishEvent(infra.sub("schedules.sched-2.skipped"), {
      _project: "b",
      reason: "cron not due",
    });

    await sleep(1500);
    expect(conductor.pendingAckEvents.length).toBeGreaterThanOrEqual(2);

    const count = conductor.ackAllEvents();
    expect(count).toBeGreaterThanOrEqual(2);
    expect(conductor.pendingAckEvents.length).toBe(0);
  });

  it("heartbeat prevents redelivery while event is pending", async () => {
    const subject = infra.sub("schedules.heartbeat-test.spawned");
    await infra.publishEvent(subject, { _project: "test" });

    await sleep(500);
    expect(conductor.pendingAckEvents.length).toBe(1);

    // Wait past the ack_wait (8s) — heartbeat should prevent redelivery
    await sleep(9000);

    // Should still be exactly 1 pending event (not redelivered as duplicate)
    expect(conductor.pendingAckEvents.length).toBe(1);

    // Verify only one delivery to LLM
    const matches = mockPi.getMessagesMatching(/heartbeat-test/);
    expect(matches).toHaveLength(1);

    conductor.ackEvent(conductor.pendingAckEvents[0].id);
  });

  it("needs_approval is ACK_REQUIRED", async () => {
    const subject = infra.sub("agents.worker-1.events.needs_approval");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      url: "https://gitlab.com/mr/42",
    });

    await sleep(1000);
    expect(conductor.pendingAckEvents.length).toBe(1);
    expect(conductor.pendingAckEvents[0].type).toBe("needs_approval");

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("awaiting human approval"),
      3000
    );
    expect(msg.text).toContain("MR !42");
  });

  it("circuit_breaker is ACK_REQUIRED", async () => {
    const subject = infra.sub("agents.worker-1.events.circuit_breaker");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      fail_count: 5,
    });

    await sleep(1000);
    expect(conductor.pendingAckEvents.length).toBe(1);
    expect(conductor.pendingAckEvents[0].type).toBe("circuit_breaker");
  });
});
