import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { createTestNatsInfra, sleep, type TestNatsInfra } from "../setup/nats-test-infra.ts";

describe("JetStream consumer lifecycle", () => {
  let infra: TestNatsInfra;

  beforeAll(async () => {
    infra = await createTestNatsInfra();
  });

  afterAll(async () => {
    if (infra) await infra.cleanup();
  });

  it("creates a durable consumer and receives messages", async () => {
    const jsm = await infra.js.jetstreamManager();
    const consumerName = `test-basic-${infra.runId}`;

    await jsm.consumers.add(infra.streams.agentEvents, {
      durable_name: consumerName,
      filter_subject: `${infra.prefix}.${infra.vignoble}.parcelles.*.agents.*.events.>`,
      ack_policy: "explicit" as any,
      deliver_policy: "all" as any,
    });

    await infra.publishEvent(
      infra.sub("agents.worker-basic.events.agent_idle"),
      { test: true }
    );

    const stream = await infra.js.streams.get(infra.streams.agentEvents);
    const consumer = await stream.getConsumer(consumerName);
    const msg = await consumer.next({ expires: 5000 });

    expect(msg).not.toBeNull();
    expect(msg!.json<any>().test).toBe(true);
    msg!.ack();

    await jsm.consumers.delete(infra.streams.agentEvents, consumerName);
  });

  it("deliver_policy 'all' replays all messages for new consumer", async () => {
    // Use isolated infra to avoid interference from other tests' consumers
    const iso = await createTestNatsInfra();
    try {
      const jsm = await iso.js.jetstreamManager();

      for (let i = 1; i <= 3; i++) {
        await iso.publishEvent(
          iso.sub(`agents.worker-replay-${i}.events.mr_merged`),
          { mr: i }
        );
      }

      const consumerName = `test-replay-${iso.runId}`;
      await jsm.consumers.add(iso.streams.agentEvents, {
        durable_name: consumerName,
        filter_subject: `${iso.prefix}.${iso.vignoble}.parcelles.*.agents.*.events.>`,
        ack_policy: "explicit" as any,
        deliver_policy: "all" as any,
      });

      const stream = await iso.js.streams.get(iso.streams.agentEvents);
      const consumer = await stream.getConsumer(consumerName);
      const received: any[] = [];

      for (let i = 0; i < 3; i++) {
        const msg = await consumer.next({ expires: 5000 });
        expect(msg).not.toBeNull();
        received.push(msg!.json<any>());
        msg!.ack();
      }

      expect(received).toHaveLength(3);
      expect(received.map((r) => r.mr).sort()).toEqual([1, 2, 3]);
      await jsm.consumers.delete(iso.streams.agentEvents, consumerName);
    } finally {
      await iso.cleanup();
    }
  });

  it("deliver_policy 'last' only delivers the latest message", async () => {
    const jsm = await infra.js.jetstreamManager();

    await infra.publishEvent(infra.sub("agents.worker-last.events.agent_idle"), { seq: 1 });
    await infra.publishEvent(infra.sub("agents.worker-last.events.agent_idle"), { seq: 2 });
    await infra.publishEvent(infra.sub("agents.worker-last.events.agent_idle"), { seq: 3 });

    const consumerName = `test-last-${infra.runId}`;
    await jsm.consumers.add(infra.streams.agentEvents, {
      durable_name: consumerName,
      filter_subject: `${infra.prefix}.${infra.vignoble}.parcelles.worker-last.agents.worker-last.events.>`,
      ack_policy: "explicit" as any,
      deliver_policy: "last" as any,
    });

    const stream = await infra.js.streams.get(infra.streams.agentEvents);
    const consumer = await stream.getConsumer(consumerName);
    const msg = await consumer.next({ expires: 5000 });

    expect(msg).not.toBeNull();
    expect(msg!.json<any>().seq).toBe(3);
    msg!.ack();

    await jsm.consumers.delete(infra.streams.agentEvents, consumerName);
  });

  it("unacked message is redelivered after ack_wait expires", async () => {
    const jsm = await infra.js.jetstreamManager();

    const consumerName = `test-redeliver-${infra.runId}`;
    await jsm.consumers.add(infra.streams.agentEvents, {
      durable_name: consumerName,
      filter_subject: `${infra.prefix}.${infra.vignoble}.parcelles.worker-redeliver.agents.worker-redeliver.events.>`,
      ack_policy: "explicit" as any,
      deliver_policy: "all" as any,
      ack_wait: 2 * 1_000_000_000,
    });

    await infra.publishEvent(
      infra.sub("agents.worker-redeliver.events.pipeline_failed"),
      { redelivery: true }
    );

    const stream = await infra.js.streams.get(infra.streams.agentEvents);
    const consumer = await stream.getConsumer(consumerName);

    // First fetch — do NOT ack
    const msg1 = await consumer.next({ expires: 5000 });
    expect(msg1).not.toBeNull();
    expect(msg1!.json<any>().redelivery).toBe(true);
    // Intentionally not acking

    // Wait for ack_wait to expire
    await sleep(3000);

    // Second fetch — should redeliver
    const consumer2 = await stream.getConsumer(consumerName);
    const msg2 = await consumer2.next({ expires: 5000 });
    expect(msg2).not.toBeNull();
    expect(msg2!.json<any>().redelivery).toBe(true);
    expect(msg2!.info.redelivered).toBe(true);
    msg2!.ack();

    await jsm.consumers.delete(infra.streams.agentEvents, consumerName);
  });

  it("msg.working() extends ack deadline preventing redelivery", async () => {
    const jsm = await infra.js.jetstreamManager();

    const consumerName = `test-working-${infra.runId}`;
    await jsm.consumers.add(infra.streams.agentEvents, {
      durable_name: consumerName,
      filter_subject: `${infra.prefix}.${infra.vignoble}.parcelles.worker-working.agents.worker-working.events.>`,
      ack_policy: "explicit" as any,
      deliver_policy: "all" as any,
      ack_wait: 2 * 1_000_000_000,
    });

    await infra.publishEvent(
      infra.sub("agents.worker-working.events.needs_approval"),
      { working_test: true }
    );

    const stream = await infra.js.streams.get(infra.streams.agentEvents);
    const consumer = await stream.getConsumer(consumerName);
    const msg = await consumer.next({ expires: 5000 });

    expect(msg).not.toBeNull();

    // Extend deadline twice past ack_wait
    msg!.working();
    await sleep(1500);
    msg!.working();
    await sleep(1500);

    // 3s > ack_wait of 2s, but working() prevented redelivery
    const info = await jsm.consumers.info(infra.streams.agentEvents, consumerName);
    expect(info.num_ack_pending).toBe(1);

    msg!.ack();
    await jsm.consumers.delete(infra.streams.agentEvents, consumerName);
  });
});
