import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";
import { createMockPi, type MockPi } from "../setup/mock-pi.ts";
import { startConductorHarness, type ConductorHarness } from "../setup/conductor-harness.ts";

describe("Contract: All dual-delivery event types dispatch to worker inbox", () => {
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

  it("main_pipeline_failed dispatches to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-main.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxPromise = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        if (inboxMessages.length >= 1) break;
      }
    })();

    const subject = infra.sub("agents.worker-main.events.main_pipeline_failed");
    await infra.publishEvent(subject, {
      mr: 55,
      _project: "exo-cli",
      url: "https://gitlab.com/pipeline/main-1",
    });

    await Promise.race([inboxPromise, sleep(5000)]);
    inboxSub.unsubscribe();

    expect(inboxMessages.length).toBeGreaterThanOrEqual(1);
    expect(inboxMessages[0].from).toBe("conductor");
  });

  it("tag_pipeline_failed dispatches to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-tag.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxPromise = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(JSON.parse(new TextDecoder().decode(msg.data)));
        if (inboxMessages.length >= 1) break;
      }
    })();

    const subject = infra.sub("agents.worker-tag.events.tag_pipeline_failed");
    await infra.publishEvent(subject, {
      mr: 60,
      _project: "charon",
      tag: "v2.0.1",
      url: "https://gitlab.com/pipeline/tag-1",
    });

    await Promise.race([inboxPromise, sleep(5000)]);
    inboxSub.unsubscribe();

    expect(inboxMessages.length).toBeGreaterThanOrEqual(1);
    expect(inboxMessages[0].from).toBe("conductor");
  });

  it("pipeline_passed does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-pass.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-pass.events.pipeline_passed");
    await infra.publishEvent(subject, { mr: 42, _project: "exo-cli" });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });

  it("session_ended does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-end.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-end.events.session_ended");
    await infra.publishEvent(subject, {
      _project: "exo-cli",
      _agentSessionId: "worker-end",
    });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });

  it("needs_approval does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-appr.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-appr.events.needs_approval");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      url: "https://gitlab.com/mr/42",
    });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });

  it("circuit_breaker does NOT dispatch to worker inbox", async () => {
    const inboxSubject = infra.sub("agents.worker-cb.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    const subject = infra.sub("agents.worker-cb.events.circuit_breaker");
    await infra.publishEvent(subject, {
      mr: 42,
      _project: "exo-cli",
      fail_count: 5,
    });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });

  it("issues_new does NOT dispatch to any worker inbox", async () => {
    const inboxSubject = infra.sub("agents.*.inbox");
    const inboxSub = infra.nc.subscribe(inboxSubject);
    const inboxMessages: any[] = [];
    const inboxLoop = (async () => {
      for await (const msg of inboxSub) {
        inboxMessages.push(msg);
      }
    })();

    await infra.publishEvent(infra.sub("issues.new"), {
      project: "exo-cli",
      _project: "exo-cli",
      iid: 99,
      title: "Test",
      url: "https://example.com",
    });

    await sleep(3500);
    inboxSub.unsubscribe();

    expect(inboxMessages).toHaveLength(0);
  });
});
