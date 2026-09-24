// Vitest globalSetup: ensures a local JetStream-enabled NATS server is
// reachable at PINARD_TEST_NATS_URL (default 127.0.0.1:4222) for the
// integration/contract test layers (see tests/README.md prerequisites).
//
// If something is already listening there, it's assumed to be a usable
// NATS server and left alone. Otherwise, when a `nats-server` binary is on
// PATH, an ephemeral instance is spawned for the duration of the test run
// (JetStream store in a throwaway tmp dir) and torn down on teardown.
//
// Deliberately does NOT read PINARD_NATS_URL: that var is exported by the
// pinard launcher for the shared production cluster (often a wss:// URL),
// which the plain-TCP `@nats-io/transport-node` `connect()` used by the
// test harness cannot speak — accidentally inheriting it produces a
// confusing "doesn't support websockets, use wsconnect" failure instead of
// a clear "no local NATS" one. Tests must never touch production NATS.
import { spawn, type ChildProcess } from "node:child_process";
import { connect as netConnect } from "node:net";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const NATS_URL = process.env.PINARD_TEST_NATS_URL || "127.0.0.1:4222";

function parseHostPort(url: string): { host: string; port: number } {
  const [host, portStr] = url.replace(/^nats:\/\//, "").split(":");
  return { host: host || "127.0.0.1", port: Number(portStr) || 4222 };
}

function portReachable(host: string, port: number, timeoutMs = 500): Promise<boolean> {
  return new Promise((resolve) => {
    const sock = netConnect({ host, port, timeout: timeoutMs });
    sock.once("connect", () => {
      sock.destroy();
      resolve(true);
    });
    sock.once("error", () => resolve(false));
    sock.once("timeout", () => {
      sock.destroy();
      resolve(false);
    });
  });
}

export default async function globalSetup() {
  const { host, port } = parseHostPort(NATS_URL);

  if (await portReachable(host, port)) {
    return; // already have a server (local dev instance, or CI sidecar) — reuse it
  }
  if (host !== "127.0.0.1" && host !== "localhost") {
    // A non-local host was explicitly configured; don't try to spawn one there.
    return;
  }

  const binAvailable = await new Promise<boolean>((resolve) => {
    const check = spawn("nats-server", ["-v"], { stdio: "ignore" });
    check.once("error", () => resolve(false));
    check.once("exit", (code) => resolve(code === 0));
  });
  if (!binAvailable) {
    console.warn(
      `[global-nats-setup] no NATS reachable at ${host}:${port} and 'nats-server' binary not found — ` +
        `integration/contract tests requiring NATS will fail. Install nats-server or set PINARD_TEST_NATS_URL.`
    );
    return;
  }

  const storeDir = mkdtempSync(join(tmpdir(), "pinard-test-nats-"));
  const child: ChildProcess = spawn(
    "nats-server",
    ["-js", "-a", host, "-p", String(port), "-sd", storeDir],
    { stdio: "ignore" }
  );

  // Wait for it to accept connections (up to ~10s).
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    if (await portReachable(host, port, 200)) break;
    await new Promise((r) => setTimeout(r, 200));
  }

  console.log(`[global-nats-setup] started ephemeral nats-server (pid ${child.pid}) at ${host}:${port}`);

  return async () => {
    child.kill();
    try {
      rmSync(storeDir, { recursive: true, force: true });
    } catch {}
  };
}
