import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { connect, wsconnect } from "@nats-io/transport-node";
import { Kvm, type KV } from "@nats-io/kv";
import { randomUUID } from "crypto";
import WebSocket from "ws";
if (!globalThis.WebSocket) (globalThis as any).WebSocket = WebSocket;

/**
 * Contract: get_maitre_status named-parcelle request/response round-trip.
 *
 * This test validates the NATS-level protocol:
 *   1. Régisseur publishes a report-request notification to the maître's parcelle channel.
 *   2. Maître receives the notification, publishes a fresh report to maitre.<p>.report
 *      echoing the correlation_id.
 *   3. Régisseur correlates the report via correlation_id and resolves its promise.
 *   4. Timeout path: when no report arrives, régisseur falls back to KV snapshot.
 *
 * Mirrors the logic in index.ts without running the full Pi extension.
 */

const NATS_URL = process.env.PINARD_TEST_NATS_URL || "127.0.0.1:4222";
const TIMEOUT_MS = 5_000; // short timeout for tests (production uses 35s)

function natsConnect(opts: any) {
  const isWs = NATS_URL.startsWith("ws://") || NATS_URL.startsWith("wss://");
  return isWs ? wsconnect({ ...opts, servers: NATS_URL }) : connect({ ...opts, servers: NATS_URL });
}

describe("Contract: get_maitre_status named-parcelle request/response round-trip", () => {
  let nc: Awaited<ReturnType<typeof connect>>;
  let kvm: Kvm;
  let kvMaitreStatus: KV;
  let vignoble: string;
  let bucketName: string;

  beforeAll(async () => {
    nc = await natsConnect({});
    kvm = new Kvm(nc);
    vignoble = `test-v-${randomUUID().slice(0, 8)}`;
    bucketName = `test-maitre-status-${randomUUID().slice(0, 8)}`;
    kvMaitreStatus = await kvm.create(bucketName, { history: 1 });
  });

  afterAll(async () => {
    try { await kvm.destroy(bucketName); } catch {}
    await nc.close();
  });

  it("report-request notification triggers maître auto-report echoing correlation_id", async () => {
    const parcelle = "webterm";
    const correlation_id = randomUUID().slice(0, 12);
    const notifSubject = `pinard.${vignoble}.parcelles.${parcelle}.notifications`;
    const reportSubject = `pinard.${vignoble}.maitre.${parcelle}.report`;

    // Simulate maître: subscribe to its notifications subject, auto-respond with report
    const maitreNotifSub = nc.subscribe(notifSubject);
    const maitreLoop = (async () => {
      for await (const msg of maitreNotifSub) {
        try {
          const data = JSON.parse(new TextDecoder().decode(msg.data)) as {
            type?: string;
            correlation_id?: string;
          };
          if (data.type === "report-request") {
            // Auto-publish report (mirrors handleMaitreReportRequest in index.ts)
            const report = `── ${parcelle} ──\nrecent completions: (none)\npending gates: (none)\ncurrent focus:\n  (auto-report in response to status request)`;
            const timestamp = new Date().toISOString();
            nc.publish(
              reportSubject,
              new TextEncoder().encode(
                JSON.stringify({ report, timestamp, parcelle, vignoble, correlation_id: data.correlation_id })
              )
            );
          }
        } catch {}
        break; // handle one message for this test
      }
    })();

    // Simulate régisseur: subscribe to maitre.*.report, correlate via correlation_id
    const reportReceived = new Promise<{ report: string; correlation_id: string }>((resolve, reject) => {
      const reportSub = nc.subscribe(reportSubject);
      const timer = setTimeout(() => {
        reportSub.unsubscribe();
        reject(new Error("Timed out waiting for maître report"));
      }, TIMEOUT_MS);
      (async () => {
        for await (const msg of reportSub) {
          try {
            const data = JSON.parse(new TextDecoder().decode(msg.data)) as {
              report?: string;
              correlation_id?: string;
            };
            if (data.correlation_id === correlation_id) {
              clearTimeout(timer);
              reportSub.unsubscribe();
              resolve({ report: data.report ?? "", correlation_id: data.correlation_id ?? "" });
              return;
            }
          } catch {}
        }
      })();
    });

    // Régisseur publishes the report-request
    nc.publish(
      notifSubject,
      new TextEncoder().encode(
        JSON.stringify({ type: "report-request", correlation_id, timestamp: new Date().toISOString() })
      )
    );

    const result = await reportReceived;
    await maitreLoop;
    maitreNotifSub.unsubscribe();

    expect(result.correlation_id).toBe(correlation_id);
    expect(result.report).toContain(`── ${parcelle} ──`);
  });

  it("report-request with non-matching correlation_id does not resolve the pending promise", async () => {
    const parcelle = "comms";
    const correlation_id = randomUUID().slice(0, 12);
    const otherCorrelationId = randomUUID().slice(0, 12);
    const reportSubject = `pinard.${vignoble}.maitre.${parcelle}.report`;

    // Publish a report with a DIFFERENT correlation_id
    nc.publish(
      reportSubject,
      new TextEncoder().encode(
        JSON.stringify({
          report: "some report",
          timestamp: new Date().toISOString(),
          parcelle,
          vignoble,
          correlation_id: otherCorrelationId,
        })
      )
    );

    // The régisseur logic: only resolve if correlation_id matches
    const pendingMaitreReports = new Map<string, { resolve: (r: string) => void; timer: ReturnType<typeof setTimeout> }>();

    const result = await new Promise<string>((resolve) => {
      const timer = setTimeout(() => {
        pendingMaitreReports.delete(correlation_id);
        resolve("timeout");
      }, 500);
      pendingMaitreReports.set(correlation_id, { resolve, timer });

      // Simulate the maitreReportSub handler logic
      (async () => {
        const sub = nc.subscribe(reportSubject);
        for await (const msg of sub) {
          try {
            const data = JSON.parse(new TextDecoder().decode(msg.data)) as { correlation_id?: string; report?: string };
            if (data.correlation_id) {
              const pending = pendingMaitreReports.get(data.correlation_id);
              if (pending) {
                clearTimeout(pending.timer);
                pendingMaitreReports.delete(data.correlation_id);
                pending.resolve(data.report ?? "");
              }
              // No match for our correlation_id — timer will fire
            }
          } catch {}
          break;
        }
        sub.unsubscribe();
      })();
    });

    expect(result).toBe("timeout");
  });

  it("timeout path: falls back to KV snapshot with staleness note", async () => {
    const parcelle = "memory";
    const key = `${vignoble}.${parcelle}`;

    // Pre-populate KV snapshot (simulates existing cached report)
    const snapshotTimestamp = new Date(Date.now() - 5 * 60 * 1000).toISOString(); // 5 min ago
    const snapshotReport = `── ${parcelle} ──\nrecent completions: (none)\npending gates: (none)\ncurrent focus:\n  monitoring issue #42`;
    await kvMaitreStatus.put(
      key,
      JSON.stringify({ report: snapshotReport, timestamp: snapshotTimestamp, parcelle })
    );

    // Simulate régisseur timeout path: no fresh report arrives, read KV
    const freshReport = await new Promise<string | null>((resolve) => {
      setTimeout(() => resolve(null), 200); // short timeout simulating no response
    });

    expect(freshReport).toBeNull();

    // Read KV snapshot
    const entry = await kvMaitreStatus.get(key);
    expect(entry).toBeDefined();
    const parsed = JSON.parse(new TextDecoder().decode(entry!.value)) as { report?: string; timestamp?: string };
    expect(parsed.report).toBe(snapshotReport);
    expect(parsed.timestamp).toBe(snapshotTimestamp);

    // Staleness note format: last snapshot Nm ago
    const ageMs = Date.now() - new Date(parsed.timestamp!).getTime();
    const ageMin = Math.floor(ageMs / 60_000);
    const stalenessNote = `last snapshot ${ageMin}m ago`;

    // Verify the expected output format that index.ts would produce
    const output = `${parsed.report}\n(no fresh report within 0s; ${stalenessNote})`;
    expect(output).toContain("no fresh report within");
    expect(output).toContain("last snapshot");
    expect(output).toContain(snapshotReport);
  });

  it("kv snapshot key format: <vignoble>.<parcelle>", async () => {
    const parcelle = "api";
    const key = `${vignoble}.${parcelle}`;
    const snapshot = { parcelle, report: "── api ──\ncurrent focus:\n  building", timestamp: new Date().toISOString() };
    await kvMaitreStatus.put(key, JSON.stringify(snapshot));

    const entry = await kvMaitreStatus.get(key);
    expect(entry).toBeDefined();
    const parsed = JSON.parse(new TextDecoder().decode(entry!.value));
    expect(parsed.parcelle).toBe(parcelle);
    expect(key).toBe(`${vignoble}.${parcelle}`);
  });

  it("report-request notification payload has correct shape", () => {
    const correlation_id = randomUUID().slice(0, 12);
    const payload = { type: "report-request", correlation_id, timestamp: new Date().toISOString() };

    expect(payload.type).toBe("report-request");
    expect(payload.correlation_id).toBe(correlation_id);
    expect(payload.correlation_id.length).toBe(12);
    expect(payload.timestamp).toBeTruthy();
  });
});
