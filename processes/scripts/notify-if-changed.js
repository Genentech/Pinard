#!/usr/bin/env node
// Deterministic `kind:'node'` babysitter task (issue #342): only calls
// `aoc notify` when the worktree's HEAD sha actually changed since the last
// notify. This replaces the old free-text "run: aoc notify ..." prompt
// instruction — the LLM sub-agent can no longer skip or misreport this
// check, since it never runs in the model's turn at all.
import { execFileSync } from 'node:child_process';
import { shouldNotify } from '../lib/notifyGate.js';

function parseArgs(argv) {
  const out = { message: '', lastSha: '' };
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--message') out.message = argv[++i] ?? '';
    else if (argv[i] === '--last-sha') out.lastSha = argv[++i] ?? '';
  }
  return out;
}

function main() {
  const { message, lastSha } = parseArgs(process.argv.slice(2));
  const currentSha = execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();

  let notified = false;
  if (shouldNotify(lastSha, currentSha)) {
    if (!message) {
      throw new Error('notify-if-changed: --message is required when the head sha has changed');
    }
    execFileSync('aoc', ['notify', message], { encoding: 'utf8' });
    notified = true;
  }

  process.stdout.write(JSON.stringify({ notified, sha: currentSha }));
}

main();
