import { connect, type NatsConnection } from "@nats-io/transport-node";
import { jetstream, type JetStreamClient, type PubAck } from "@nats-io/jetstream";
import { Kvm, type KV } from "@nats-io/kv";
import { randomUUID } from "crypto";

export interface TestNatsInfra {
  runId: string;
  vignoble: string;
  prefix: string;
  nc: NatsConnection;
  js: JetStreamClient;
  streams: {
    agentEvents: string;
    notifications: string;
    issues: string;
    schedulerEvents: string;
    inboxes: string;
  };
  kv: {
    agents: KV;
    mrs: KV;
    schedules: KV;
  };
  publishEvent(subject: string, payload: Record<string, any>): Promise<PubAck>;
  getConsumerInfo(streamName: string, consumerName: string): Promise<any>;
  cleanup(): Promise<void>;
  sub(path: string): string;
}

// PINARD_TEST_NATS_URL (not PINARD_NATS_URL): the latter is exported by the
// pinard launcher for the shared production cluster (often wss://), which the
// plain-TCP `connect()` used here can't speak — see global-nats-setup.ts.
const NATS_URL = process.env.PINARD_TEST_NATS_URL || "127.0.0.1:4222";

export async function createTestNatsInfra(): Promise<TestNatsInfra> {
  const runId = randomUUID().slice(0, 8);
  const prefix = `test-${runId}`;
  const vignoble = `v-${runId}`;

  const nc = await connect({ servers: NATS_URL });
  const js = jetstream(nc);
  const jsm = await js.jetstreamManager();

  const streams = {
    agentEvents: `${prefix}-agent-events`,
    notifications: `${prefix}-notifications`,
    issues: `${prefix}-issues`,
    schedulerEvents: `${prefix}-sched-events`,
    inboxes: `${prefix}-inboxes`,
  };

  // Use a unique prefix so subjects don't overlap with production streams
  // Production uses "pinard.*..." — we use "test-{uuid}.*..."
  await jsm.streams.add({
    name: streams.agentEvents,
    subjects: [`${prefix}.${vignoble}.parcelles.*.agents.*.events.>`],
    retention: "limits" as any,
    max_msgs: 1000,
    max_age: 600_000_000_000,
    duplicate_window: 120_000_000_000,
  });

  await jsm.streams.add({
    name: streams.schedulerEvents,
    subjects: [`${prefix}.${vignoble}.schedules.>`],
    retention: "limits" as any,
    max_msgs: 500,
    max_age: 600_000_000_000,
  });

  await jsm.streams.add({
    name: streams.issues,
    subjects: [`${prefix}.${vignoble}.issues.>`],
    retention: "limits" as any,
    max_msgs: 1000,
    max_age: 600_000_000_000,
  });

  await jsm.streams.add({
    name: streams.inboxes,
    subjects: [`${prefix}.${vignoble}.parcelles.*.agents.*.inbox`],
    retention: "limits" as any,
    max_msgs_per_subject: 50,
    max_age: 600_000_000_000,
  });

  await jsm.streams.add({
    name: streams.notifications,
    subjects: [`${prefix}.${vignoble}.notifications`],
    retention: "limits" as any,
    max_msgs: 100,
    max_age: 600_000_000_000,
  });

  const kvm = new Kvm(nc);
  const kvAgents = await kvm.create(`${prefix}-agents`, { history: 1, ttl: 600_000 });
  const kvMRs = await kvm.create(`${prefix}-mrs`, { history: 1, ttl: 600_000 });
  const kvSchedules = await kvm.create(`${prefix}-schedules`, { history: 1, ttl: 600_000 });

  const encoder = new TextEncoder();

  return {
    runId,
    vignoble,
    prefix,
    nc,
    js,
    streams,
    kv: { agents: kvAgents, mrs: kvMRs, schedules: kvSchedules },

    sub(path: string): string {
      // Auto-scope agent subjects with a `parcelles` segment so tests written
      // against the pre-parcelle hierarchy keep working. Uses the agent token as
      // the parcelle token (matches the conductor's fallback: no KV parcelle ->
      // session name). `agents.*...` -> `parcelles.*.agents.*...`;
      // `agents.<name>...` -> `parcelles.<name>.agents.<name>...`.
      const m = path.match(/^agents\.([^.]+)\.(.*)$/);
      if (m) {
        const who = m[1];
        path = `parcelles.${who}.agents.${who}.${m[2]}`;
      }
      return `${prefix}.${vignoble}.${path}`;
    },

    async publishEvent(subject: string, payload: Record<string, any>): Promise<PubAck> {
      return js.publish(subject, encoder.encode(JSON.stringify(payload)));
    },

    async getConsumerInfo(streamName: string, consumerName: string) {
      return jsm.consumers.info(streamName, consumerName);
    },

    async cleanup() {
      const jsm2 = await js.jetstreamManager();
      for (const name of Object.values(streams)) {
        try {
          await jsm2.streams.delete(name);
        } catch {}
      }
      const kvm2 = new Kvm(nc);
      for (const name of [
        `${prefix}-agents`,
        `${prefix}-mrs`,
        `${prefix}-schedules`,
      ]) {
        try {
          await kvm2.destroy(name);
        } catch {}
      }
      await nc.close();
    },
  };
}

export function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
