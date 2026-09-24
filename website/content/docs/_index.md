---
title: Documentation
ledger_synced_commit: 68af73234e818afe008246aeb3cbbe2e89432854
ledger_synced_at: 2026-09-23
---

## Orchestrate fleets of agents — reliably

Pinard is a **distributed engine for running fleets of LLM agents through
*semi-deterministic loops*** — deterministic runbooks that drive non-deterministic
agents, coordinated over NATS across many repositories and machines.

A raw agent is powerful but unpredictable: it wanders, forgets, and can't be
resumed. Pinard wraps every agent in a defined loop where the **control flow is
code** and only the **work inside a step** is left to the model. The result is agent
work that is **reliable** (the loop decides what happens next), **resumable** (every
step is journaled), **auditable** (you can read exactly what ran), and
**distributed** (loops run as a fleet across repos and machines).

Three pillars hold it up:

> **Deterministic control** · **Non-deterministic agents** · **Persistent memory**

<figure class="doc-figure">
  <div class="doc-figure-visual">
    <img src="/images/docs/docs-estate-overview.jpg" alt="A sketched vineyard estate with several independent worker parcels connected to a central control house and a persistent cellar archive.">
    <span class="doc-figure-label charcoal" style="--x: 50%; --y: 18%;">C · Central control</span>
    <span class="doc-figure-label doc-figure-label--desktop terracotta" style="--x: 22%; --y: 72%;">A · Agent parcels</span>
    <span class="doc-figure-label doc-figure-label--desktop charcoal" style="--x: 50%; --y: 78%;">M · Persistent memory</span>
    <span class="doc-figure-label doc-figure-label--desktop mustard" style="--x: 77%; --y: 58%;">↔ · Deterministic routes</span>
  </div>
  <figcaption><strong>One coordinated estate.</strong> Deterministic routes connect independent agent work to shared control and durable memory.</figcaption>
  <ul class="doc-figure-legend" aria-label="Pinard system overview">
    <li><span class="doc-figure-key charcoal">C</span><span><strong>Central control</strong> — the daemon and optional conductors coordinate the estate.</span></li>
    <li><span class="doc-figure-key terracotta">A</span><span><strong>Agent parcels</strong> — workers operate independently inside bounded workstreams.</span></li>
    <li><span class="doc-figure-key">↔</span><span><strong>Deterministic routes</strong> — explicit control and events connect every component.</span></li>
    <li><span class="doc-figure-key charcoal">M</span><span><strong>Persistent memory</strong> — distilled knowledge survives individual sessions.</span></li>
  </ul>
</figure>

Automating code review — turning issues into merged MRs — is just *one* built-in
loop. You write your own loops for whatever your fleet does, and the fleet gets
smarter over time as it writes what it learns to memory.

*One conductor. Many agents. One harvest.* 🍷

New here? Start with the **[Overview](/docs/overview/)**, then
**[The Semi-Deterministic Loop](/docs/semi-deterministic-loop/)** and
**[Memory & Recall](/docs/memory/)** — the ideas that make the rest cohere.
