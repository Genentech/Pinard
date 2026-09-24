import { describe, it, expect, afterAll } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi } from "../setup/mock-pi.ts";
import { startConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: deliver_policy 'all' replays unacked messages on restart (THE BUG)", () => {
  // Each test gets its own infra to avoid cross-test interference
  // since "all" replays the entire stream

  it("events published BEFORE conductor starts are delivered (replay)", async () => {
    const infra = await createTestNatsInfra();
    try {
      // Publish BEFORE any consumer exists
      await infra.publishEvent(
        infra.sub("agents.worker-replay.events.agent_idle"),
        { _project: "exo-cli", _agentSessionId: "worker-replay", cwd: "/x/exo-cli" }
      );

      // Start conductor with "all" — should replay
      const mockPi = createMockPi();
      const conductor = await startConductorHarness(mockPi, infra, {
        agentEventsPolicy: "all",
      });

      const msg = await mockPi.waitForMessage(
        (m) => m.text.includes("is idle"),
        5000
      );
      expect(msg.text).toContain("exo-cli");

      await conductor.cleanup();
    } finally {
      if (infra) await infra.cleanup();
    }
  });

  it("deliver_policy 'last' LOSES earlier messages (regression proof)", async () => {
    const infra = await createTestNatsInfra();
    try {
      const subject = infra.sub("agents.worker-lost.events.mr_merged");
      await infra.publishEvent(subject, { mr: 1, _project: "p1" });
      await infra.publishEvent(subject, { mr: 2, _project: "p2" });
      await infra.publishEvent(subject, { mr: 3, _project: "p3" });

      const mockPi = createMockPi();
      const conductor = await startConductorHarness(mockPi, infra, {
        agentEventsPolicy: "last",
      });

      await sleep(3000);

      // With "last", only the latest message arrives
      const allText = mockPi.messages.map((m) => m.text).join("\n");
      expect(allText).not.toContain("MR !1");
      expect(allText).not.toContain("MR !2");

      await conductor.cleanup();
    } finally {
      await infra.cleanup();
    }
  });

  it("events published while conductor is down are delivered on restart", async () => {
    const infra = await createTestNatsInfra();
    try {
      // Phase 1: Start conductor, process one event, then shutdown
      const mockPi1 = createMockPi();
      const conductor1 = await startConductorHarness(mockPi1, infra, {
        agentEventsPolicy: "all",
      });

      await infra.publishEvent(
        infra.sub("agents.worker-crash.events.session_ended"),
        { _project: "exo-cli", _agentSessionId: "worker-crash" }
      );

      await mockPi1.waitForMessage((m) => m.text.includes("session ended"), 5000);
      await conductor1.cleanup();

      // Phase 2: Publish while conductor is offline
      await infra.publishEvent(
        infra.sub("agents.worker-crash.events.mr_merged"),
        { mr: 77, _project: "exo-cli" }
      );

      // Phase 3: Restart conductor — should pick up the new event
      const mockPi2 = createMockPi();
      const conductor2 = await startConductorHarness(mockPi2, infra, {
        agentEventsPolicy: "all",
      });

      // It replays both events (session_ended + mr_merged) as a batch
      // The batch summary format lists events by type, not formatted message
      // Verify the mr_merged event was received and recorded
      await sleep(3000);
      const events = conductor2.getRecentEvents();
      expect(events.some((e) => e.type === "mr_merged" && e.data.mr === 77)).toBe(true);
      // And verify something was delivered to the LLM
      expect(mockPi2.messages.length).toBeGreaterThan(0);

      await conductor2.cleanup();
    } finally {
      await infra.cleanup();
    }
  });

  it("events are acked AFTER processing completes (not before)", async () => {
    const infra = await createTestNatsInfra();
    try {
      // Start conductor first, then publish
      const mockPi = createMockPi();
      const conductor = await startConductorHarness(mockPi, infra);

      await infra.publishEvent(
        infra.sub("agents.worker-ack-order.events.agent_idle"),
        { _project: "exo-cli", _agentSessionId: "worker-ack-order" }
      );

      // Wait for delivery
      await mockPi.waitForMessage((m) => m.text.includes("is idle"), 5000);

      // Give ack time to propagate
      await sleep(500);

      // After delivery, message should be acked
      const jsm = await infra.js.jetstreamManager();
      const consumers = await jsm.consumers.list(infra.streams.agentEvents).next();
      // Find our consumer
      let ackPending = 0;
      for await (const c of jsm.consumers.list(infra.streams.agentEvents)) {
        if (c.name.includes(infra.runId)) {
          ackPending = c.num_ack_pending;
        }
      }
      expect(ackPending).toBe(0);

      await conductor.cleanup();
    } finally {
      await infra.cleanup();
    }
  });
});
