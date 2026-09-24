import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";

describe("KV bucket CRUD operations", () => {
  let infra: TestNatsInfra;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("put and get a value", async () => {
    const kv = infra.kv.agents;
    const state = { project: "exo-cli", state: "running", tempo: "active" };
    await kv.put("worker-1", JSON.stringify(state));

    const entry = await kv.get("worker-1");
    expect(entry).toBeDefined();
    const data = entry!.json<any>();
    expect(data.project).toBe("exo-cli");
    expect(data.state).toBe("running");
  });

  it("update an existing value", async () => {
    const kv = infra.kv.agents;
    await kv.put("worker-2", JSON.stringify({ tempo: "active" }));
    await kv.put("worker-2", JSON.stringify({ tempo: "blocked" }));

    const entry = await kv.get("worker-2");
    expect(entry!.json<any>().tempo).toBe("blocked");
  });

  it("delete a key", async () => {
    const kv = infra.kv.agents;
    await kv.put("worker-3", JSON.stringify({ state: "running" }));
    await kv.delete("worker-3");

    const entry = await kv.get("worker-3");
    // After delete, KV returns null or a tombstone entry with no value
    expect(entry === null || entry.value.length === 0).toBe(true);
  });

  it("list all keys", async () => {
    const kv = infra.kv.agents;
    await kv.put("list-a", JSON.stringify({ a: 1 }));
    await kv.put("list-b", JSON.stringify({ b: 2 }));

    const keys: string[] = [];
    for await (const key of await kv.keys()) {
      keys.push(key);
    }
    expect(keys).toContain("list-a");
    expect(keys).toContain("list-b");
  });

  it("watch detects puts", async () => {
    const kv = infra.kv.mrs;
    const received: any[] = [];

    const watch = await kv.watch();
    const watchPromise = (async () => {
      for await (const entry of watch) {
        if (entry.key === `watch-target`) {
          received.push(entry.json<any>());
          break;
        }
      }
      watch.stop();
    })();

    await sleep(200);
    await kv.put("watch-target", JSON.stringify({ mr: 42 }));

    await watchPromise;
    expect(received).toHaveLength(1);
    expect(received[0].mr).toBe(42);
  });
});
