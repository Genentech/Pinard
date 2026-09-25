#!/usr/bin/env node
// Deterministic `kind:'node'` babysitter task (issue #342): resolves the
// worktree's current HEAD sha. No LLM involved — used to seed/refresh the
// baseline that notify-if-changed.js compares against.
import { execFileSync } from 'node:child_process';

function main() {
  const sha = execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();
  process.stdout.write(JSON.stringify({ sha }));
}

main();
