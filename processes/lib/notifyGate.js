// Deterministic gate for turn-end notifications (issue #342).
//
// A vendangeur turn should only announce "Addressed review feedback" /
// "Fixed pipeline" when it actually landed a new commit. This is a pure,
// framework-free function so it can run inside a `kind:'node'` babysitter
// task (no LLM involved) and be unit-tested in isolation.

/**
 * @param {string | undefined | null} previousSha - the last SHA a notify fired for (or was seeded with)
 * @param {string | undefined | null} currentSha - the current HEAD sha
 * @returns {boolean} true when a notify should fire
 */
export function shouldNotify(previousSha, currentSha) {
  if (!currentSha) return false;
  if (!previousSha) return true;
  return previousSha !== currentSha;
}
