import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Issue watcher events flow to conductor", () => {
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

  it("issues_new delivers formatted message with iid, title, and URL", async () => {
    await infra.publishEvent(infra.sub("issues.new"), {
      project: "exo-cli",
      _project: "exo-cli",
      iid: 11,
      title: "Fix login timeout",
      url: "https://gitlab.com/exo-cli/issues/11",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("#11"),
      5000
    );
    expect(msg.text).toContain("New issue #11");
    expect(msg.text).toContain("Fix login timeout");
    expect(msg.text).toContain("https://gitlab.com/exo-cli/issues/11");
  });

  it("issues_new event is recorded in agentEvents buffer", async () => {
    await infra.publishEvent(infra.sub("issues.new"), {
      project: "charon",
      _project: "charon",
      iid: 7,
      title: "Memory leak",
      url: "https://gitlab.com/charon/issues/7",
    });

    await sleep(3000);

    const events = conductor.getRecentEvents();
    const issueEvent = events.find((e) => e.type === "issues_new");
    expect(issueEvent).toBeDefined();
    expect(issueEvent!.data.iid).toBe(7);
  });

  it("issues_new is NOT ACK_REQUIRED (auto-acked)", async () => {
    await infra.publishEvent(infra.sub("issues.new"), {
      project: "exo-cli",
      _project: "exo-cli",
      iid: 99,
      title: "Test issue",
      url: "https://example.com",
    });

    await sleep(2000);

    // Should NOT be in pending queue
    expect(conductor.pendingAckEvents.length).toBe(0);
  });
});
