import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { connect } from "@nats-io/transport-node";
import { Kvm, type KV } from "@nats-io/kv";
import { randomUUID } from "crypto";

const NATS_URL = process.env.PINARD_TEST_NATS_URL || "127.0.0.1:4222";

describe("Contract: pinard-maitre-status KV bucket provisioning and round-trip", () => {
  let nc: Awaited<ReturnType<typeof connect>>;
  let kvm: Kvm;
  let bucketName: string;
  let bucket: KV;

  beforeAll(async () => {
    nc = await connect({ servers: NATS_URL });
    kvm = new Kvm(nc);
    bucketName = `test-maitre-status-${randomUUID().slice(0, 8)}`;
    // Mirrors what the extension does: kvm.create (create-or-open, idempotent)
    bucket = await kvm.create(bucketName, { history: 1 });
  });

  afterAll(async () => {
    try { await kvm.destroy(bucketName); } catch {}
    await nc.close();
  });

  it("bucket is created successfully via kvm.create", () => {
    expect(bucket).toBeDefined();
  });

  it("kvm.create is idempotent — second call returns the existing bucket", async () => {
    const bucket2 = await kvm.create(bucketName, { history: 1 });
    expect(bucket2).toBeDefined();
  });

  it("report_to_regisseur round-trip: put snapshot is readable by get_maitre_status", async () => {
    const vignoble = "test-v";
    const parcelle = "comms";
    const key = `${vignoble}.${parcelle}`;
    const snapshot = {
      parcelle,
      report: "Working on issue #259",
      timestamp: new Date().toISOString(),
    };
    await bucket.put(key, JSON.stringify(snapshot));

    const entry = await bucket.get(key);
    expect(entry).toBeDefined();
    const parsed = JSON.parse(new TextDecoder().decode(entry!.value));
    expect(parsed.parcelle).toBe(parcelle);
    expect(parsed.report).toBe("Working on issue #259");
    expect(parsed.timestamp).toBeTruthy();
  });

  it("empty bucket returns no entries (not an error)", async () => {
    const emptyBucketName = `test-maitre-empty-${randomUUID().slice(0, 8)}`;
    const emptyBucket = await kvm.create(emptyBucketName, { history: 1 });
    try {
      const keys: string[] = [];
      // Iterating keys on an empty bucket must not throw
      try {
        for await (const key of await emptyBucket.keys()) {
          keys.push(key);
        }
      } catch (e: any) {
        // nats.go throws "no keys found" for an empty bucket — treat as empty
        if (!e?.message?.includes("no keys found")) throw e;
      }
      expect(keys.length).toBe(0);
    } finally {
      try { await kvm.destroy(emptyBucketName); } catch {}
    }
  });
});
