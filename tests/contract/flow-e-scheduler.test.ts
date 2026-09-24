import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Scheduler events flow to conductor", () => {
  let infra: TestNatsInfra;
  let mockPi: MockPi;
  let conductor: ConductorHarness;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  beforeEach(async () => {
    mockPi = createMockPi();
    conductor = await startConductorHarness(mockPi, infra, {
      schedulerAckWait: 10 * 1_000_000_000,
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

  it("schedule_spawned delivers formatted message", async () => {
    await infra.publishEvent(infra.sub("schedules.user-guide-sync.spawned"), {
      _project: "exo-cli",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("user-guide-sync"),
      5000
    );
    expect(msg.text).toContain("fired");
    expect(msg.text).toContain("/inbox");
  });

  it("schedule_skipped delivers reason", async () => {
    await infra.publishEvent(infra.sub("schedules.nightly-build.skipped"), {
      _project: "charon",
      reason: "cron not due",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("nightly-build"),
      5000
    );
    expect(msg.text).toContain("cron not due");
  });

  it("schedule_failed delivers error message", async () => {
    await infra.publishEvent(infra.sub("schedules.deploy-check.failed"), {
      _project: "exo-cli",
      error: "spawn timed out after 120s",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("deploy-check"),
      5000
    );
    expect(msg.text).toContain("FAILED");
    expect(msg.text).toContain("spawn timed out");
  });

  it("all schedule event types are ACK_REQUIRED", async () => {
    await infra.publishEvent(infra.sub("schedules.test-sched.spawned"), {
      _project: "a",
    });
    await sleep(1000);
    expect(conductor.pendingAckEvents.some((p) => p.type === "schedule_spawned")).toBe(true);
  });
});
