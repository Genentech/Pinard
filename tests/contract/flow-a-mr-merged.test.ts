import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: MR merged event flows from watcher to LLM", () => {
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

  it("delivers formatted mr_merged message to LLM via sendUserMessage", async () => {
    const subject = infra.sub("agents.exo-cli-1234.events.mr_merged");
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("[agent-event]") && m.text.includes("MR !42"),
      5000
    );

    expect(msg.text).toBe("[agent-event] MR !42 on exo-cli was merged.");
    expect(msg.options.deliverAs).toBe("followUp");
  });

  it("deduplicates identical mr_merged events", async () => {
    const subject = infra.sub("agents.exo-cli-dedup.events.mr_merged");
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });

    // Wait for first event to flush (2s batch window)
    await sleep(2500);

    // Publish duplicate after first was already processed
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });
    await sleep(3000);

    // Dedup prevents second delivery — only 1 individual message
    const matches = mockPi.getMessagesMatching(/MR !42.*merged/);
    expect(matches).toHaveLength(1);
  });

  it("does not deduplicate events for different MRs", async () => {
    // Publish first, let it flush
    const sub1 = infra.sub("agents.worker-diff-1.events.mr_merged");
    await infra.publishEvent(sub1, { mr: 42, _project: "exo-cli" });
    await sleep(2500);

    // Publish second separately
    const sub2 = infra.sub("agents.worker-diff-2.events.mr_merged");
    await infra.publishEvent(sub2, { mr: 99, _project: "charon" });
    await sleep(3000);

    const allText = mockPi.messages.map((m) => m.text).join("\n");
    expect(allText).toContain("MR !42");
    expect(allText).toContain("MR !99");
  });

  it("event is recorded in agentEvents buffer", async () => {
    const subject = infra.sub("agents.worker-1.events.mr_merged");
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });

    await sleep(3000);

    const events = conductor.getRecentEvents();
    expect(events.some((e) => e.type === "mr_merged")).toBe(true);
  });
});
