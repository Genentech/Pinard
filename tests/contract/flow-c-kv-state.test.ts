import { describe, it, expect, beforeAll, afterAll, beforeEach, afterEach } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";

describe("Contract: Worker state in KV is readable by conductor", () => {
  let infra: TestNatsInfra;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("worker publishes state to KV and it's readable", async () => {
    const kv = infra.kv.agents;
    const state = {
      project: "exo-cli",
      name: "exo-cli-1234",
      state: "running",
      tempo: "active",
      cwd: "/workspaces/exo-cli",
      vignoble: infra.vignoble,
    };
    await kv.put("exo-cli-1234", JSON.stringify(state));

    const entry = await kv.get("exo-cli-1234");
    expect(entry).toBeDefined();
    const data = entry!.json<any>();
    expect(data.project).toBe("exo-cli");
    expect(data.state).toBe("running");
    expect(data.tempo).toBe("active");
  });

  it("worker state transition active → blocked is reflected", async () => {
    const kv = infra.kv.agents;

    await kv.put(
      "worker-transition",
      JSON.stringify({
        project: "exo-cli",
        name: "worker-transition",
        state: "running",
        tempo: "active",
        vignoble: infra.vignoble,
      })
    );

    // Simulate turn_end
    await kv.put(
      "worker-transition",
      JSON.stringify({
        project: "exo-cli",
        name: "worker-transition",
        state: "running",
        tempo: "blocked",
        vignoble: infra.vignoble,
      })
    );

    const entry = await kv.get("worker-transition");
    expect(entry!.json<any>().tempo).toBe("blocked");
  });

  it("worker session_end deletes KV entry", async () => {
    const kv = infra.kv.agents;

    await kv.put(
      "worker-ending",
      JSON.stringify({
        project: "exo-cli",
        name: "worker-ending",
        state: "running",
        tempo: "active",
        vignoble: infra.vignoble,
      })
    );

    // Simulate session_end
    await kv.delete("worker-ending");

    const entry = await kv.get("worker-ending");
    expect(entry === null || entry.value.length === 0).toBe(true);
  });

  it("multiple workers are visible in the same KV bucket", async () => {
    const kv = infra.kv.agents;

    await kv.put(
      "multi-w1",
      JSON.stringify({
        project: "proj-a",
        name: "multi-w1",
        state: "running",
        tempo: "active",
        vignoble: infra.vignoble,
      })
    );
    await kv.put(
      "multi-w2",
      JSON.stringify({
        project: "proj-b",
        name: "multi-w2",
        state: "running",
        tempo: "blocked",
        vignoble: infra.vignoble,
      })
    );

    const keys: string[] = [];
    for await (const key of await kv.keys()) {
      if (key.startsWith("multi-")) keys.push(key);
    }
    expect(keys).toContain("multi-w1");
    expect(keys).toContain("multi-w2");
  });
});
