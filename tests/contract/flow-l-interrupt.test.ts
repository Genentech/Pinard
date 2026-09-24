import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: Interrupt channel", () => {
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

  it("interrupt is published to correct NATS subject", async () => {
    const subject = infra.sub("agents.worker-int.interrupt");
    const sub = infra.nc.subscribe(subject);
    const messages: any[] = [];
    const loop = (async () => {
      for await (const msg of sub) {
        messages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        break;
      }
    })();

    conductor.publishInterrupt("worker-int", "New priority task");

    await Promise.race([loop, sleep(3000)]);
    sub.unsubscribe();

    expect(messages.length).toBe(1);
    expect(messages[0].reason).toBe("New priority task");
    expect(messages[0].from).toBe("conductor");
  });

  it("interrupt does NOT kill session (KV state preserved)", async () => {
    const kv = infra.kv.agents;
    await kv.put("worker-keep", JSON.stringify({
      project: "exo-cli",
      name: "worker-keep",
      state: "running",
      tempo: "active",
      vignoble: infra.vignoble,
    }));

    conductor.publishInterrupt("worker-keep", "stop current work");

    await sleep(1000);

    // KV entry should still exist
    const entry = await kv.get("worker-keep");
    expect(entry).toBeDefined();
    expect(entry!.json<any>().state).toBe("running");
  });

  it("interrupt uses default reason when none provided", async () => {
    const subject = infra.sub("agents.worker-def.interrupt");
    const sub = infra.nc.subscribe(subject);
    const messages: any[] = [];
    const loop = (async () => {
      for await (const msg of sub) {
        messages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        break;
      }
    })();

    conductor.publishInterrupt("worker-def");

    await Promise.race([loop, sleep(3000)]);
    sub.unsubscribe();

    expect(messages.length).toBe(1);
    expect(messages[0].reason).toBe("Interrupted by conductor");
  });

  it("interrupt does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-noinbox.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    conductor.publishInterrupt("worker-noinbox", "stop");

    await sleep(2000);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });
});
