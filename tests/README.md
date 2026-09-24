# Pinard Test Harness

## Prerequisites

- **Node.js 20+** and npm (for Vitest tests)
- **Go 1.24+** (for Go unit tests)
- **NATS 2.14+** running locally with JetStream enabled (for integration and contract tests).
  `npm test` auto-starts an ephemeral `nats-server -js` on `127.0.0.1:4222` if
  nothing is already listening there and the `nats-server` binary is on PATH
  (see `setup/global-nats-setup.ts`); otherwise install it or point
  `PINARD_TEST_NATS_URL` at an existing instance. Deliberately independent of
  `PINARD_NATS_URL` (the production cluster the pinard launcher exports —
  tests must never touch it).

Install JS dependencies:

```bash
cd tests && npm install
```

## Running tests

### TypeScript tests (Pi extension logic)

```bash
cd tests
npm test                    # All Vitest (unit + integration + contract)
npm run test:unit           # Pure functions, no NATS needed
npm run test:integration    # NATS consumer lifecycle, KV CRUD
npm run test:contract       # Full event flows with real NATS + MockPi
```

### Go tests (CLI/daemon logic)

```bash
cd pinard
go test ./internal/...
```

## Test layers

| Layer | Framework | What it tests | NATS required |
|-------|-----------|---------------|---------------|
| **Unit** | Vitest | Pure functions: formatting, dedup keys, status, tool ownership | No |
| **Integration** | Vitest | JetStream consumer lifecycle, KV CRUD, redelivery, auth | Yes |
| **Contract** | Vitest | Full event flows across component boundaries via MockPi | Yes |
| **Go unit** | go test | Cron matching, state persistence, watcher logic | No |

## Test isolation

Each Vitest test run creates isolated NATS resources using unique prefixes (`test-{uuid}`), so tests never interfere with production streams or with each other. All resources are cleaned up on teardown with a 10-minute TTL safety net.

## File layout

```
tests/
  setup/
    mock-pi.ts              # MockPi: captures sendUserMessage, tools, events
    nats-test-infra.ts      # Creates/destroys test-scoped streams + KV
    conductor-harness.ts    # Reproduces conductor's NATS pipeline for contract tests
  unit/                     # Pure function tests (formatting, dedup, tool ownership)
  integration/              # NATS consumer lifecycle, KV CRUD, auth
  contract/                 # Full event flows (flows A-L)

internal/
  cron/cron_test.go         # Cron expression matching
  state/state_test.go       # Atomic write-through, concurrent access
  watcher/mrs_test.go       # MR watcher state transitions
  watcher/issues_test.go    # Issue watcher logic
  watcher/scheduler_test.go # Scheduler firing logic
```
