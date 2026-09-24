import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Batching and deduplication work together", () => {
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

  it("single event within batch window is delivered individually", async () => {
    await infra.publishEvent(infra.sub("agents.worker-1.events.agent_idle"), {
      _project: "exo-cli",
      _agentSessionId: "worker-1",
    });

    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("is idle"),
      5000
    );
    // Individual delivery — not a batch summary
    expect(msg.text).not.toContain("[agent-events]");
    expect(msg.text).toContain("[agent-event]");
  });

  it("multiple events within 2s window are batched as summary", async () => {
    // Publish multiple events rapidly
    await infra.publishEvent(infra.sub("agents.w1.events.agent_idle"), {
      _project: "proj-a",
      _agentSessionId: "w1",
    });
    await infra.publishEvent(infra.sub("agents.w2.events.session_ended"), {
      _project: "proj-b",
      _agentSessionId: "w2",
    });
    await infra.publishEvent(infra.sub("agents.w3.events.mr_merged"), {
      mr: 10,
      _project: "proj-c",
    });

    // Wait for batch flush
    const msg = await mockPi.waitForMessage(
      (m) => m.text.includes("[agent-events]") && m.text.includes("events"),
      5000
    );

    expect(msg.text).toContain("3 events");
    expect(msg.text).toContain("w1");
    expect(msg.text).toContain("w2");
    expect(msg.text).toContain("w3");
  });

  it("deduplication within a batch prevents double delivery", async () => {
    // Same event published twice within batch window
    await infra.publishEvent(infra.sub("agents.worker-dup.events.mr_merged"), {
      mr: 42,
      _project: "exo-cli",
    });
    await infra.publishEvent(infra.sub("agents.worker-dup.events.mr_merged"), {
      mr: 42,
      _project: "exo-cli",
    });

    await sleep(3500);

    // The agentEvents buffer records all (even dupes for logging)
    // But sendUserMessage should only fire once for the deduped event
    const allText = mockPi.messages.map((m) => m.text).join("\n");
    const mergedCount = (allText.match(/MR !42.*merged/g) || []).length;
    // In a batch, the individual handleAgentEvent calls with _batched flag
    // don't send messages — only the summary does. So 0 or 1 mentions.
    expect(mergedCount).toBeLessThanOrEqual(1);
  });

  it("events with different dedup keys are not deduped", async () => {
    await infra.publishEvent(infra.sub("agents.worker-a.events.pipeline_failed"), {
      mr: 42,
      _project: "exo-cli",
      attempt: 1,
      max: 3,
    });

    // Wait for first to flush
    await sleep(2500);

    await infra.publishEvent(infra.sub("agents.worker-a.events.pipeline_failed"), {
      mr: 42,
      _project: "exo-cli",
      attempt: 2,
      max: 3,
    });

    await sleep(3000);

    // Both should be delivered (different attempt = different dedup key)
    const allText = mockPi.messages.map((m) => m.text).join("\n");
    expect(allText).toContain("attempt 1/3");
    expect(allText).toContain("attempt 2/3");
  });
});
