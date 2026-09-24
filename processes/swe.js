import { defineTask } from '@a5c-ai/babysitter-sdk';

// ── Task definitions ─────────────────────────────────────────

const fetchIssue = defineTask('fetch-issue', (args) => ({
  kind: 'agent',
  title: 'Fetch issue content',
  agent: {
    name: 'issue-fetcher',
    prompt: {
      role: 'A developer fetching issue details',
      task: `Use the read_issue tool to fetch issue #${args.issueId} on project "${args.project}". Return the issue title and description.`,
    },
    outputSchema: {
      type: 'object',
      properties: {
        title: { type: 'string' },
        description: { type: 'string' },
        labels: { type: 'array', items: { type: 'string' } },
      },
      required: ['title', 'description'],
    },
  },
}));

const analyzeWork = defineTask('analyze-work', (args) => ({
  kind: 'agent',
  title: 'Analyze issue and plan implementation',
  agent: {
    name: 'planner',
    prompt: {
      role: 'A senior software engineer analyzing a GitLab issue',
      task: `Read and analyze the following issue. Understand what needs to be done, identify the affected files, and produce an implementation plan.

Issue: ${args.issue || args.prompt}
Project: ${args.project}`,
      instructions: [
        'Read the issue description carefully',
        'Explore the codebase to understand the context',
        'Identify which files need to change',
        'Determine if this is a docs-only change, a bug fix, a feature, or a refactor',
        'Produce a concise implementation plan',
      ],
      outputFormat: 'A structured plan with change type and affected files',
    },
    outputSchema: {
      type: 'object',
      properties: {
        changeType: { type: 'string', enum: ['docs', 'bugfix', 'feature', 'refactor'] },
        plan: { type: 'string' },
        affectedFiles: { type: 'array', items: { type: 'string' } },
      },
      required: ['changeType', 'plan'],
    },
  },
}));

const implement = defineTask('implement', (args) => ({
  kind: 'agent',
  title: 'Implement the changes',
  agent: {
    name: 'implementer',
    prompt: {
      role: 'A software engineer implementing a planned change',
      task: `Implement the following plan. Make the code changes, ensure they are correct, and do not introduce regressions.

Plan: ${args.plan}

Affected files: ${JSON.stringify(args.affectedFiles || [])}`,
      instructions: [
        'Follow the plan step by step',
        'Write clean, production-quality code',
        'Do not add unnecessary comments or abstractions',
        'Stage your changes with git add',
      ],
      outputFormat: 'Summary of what was implemented',
    },
    outputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string' },
        filesChanged: { type: 'array', items: { type: 'string' } },
      },
      required: ['summary'],
    },
  },
}));

const runTests = defineTask('run-tests', (args) => ({
  kind: 'agent',
  title: 'Run tests',
  agent: {
    name: 'tester',
    prompt: {
      role: 'A QA engineer running the test suite',
      task: `Run the project test suite and report results.${args.command ? `\n\nTest command: ${args.command}` : ''}`,
      instructions: [
        args.command ? `Run: ${args.command}` : 'Find and run the appropriate test command for this project',
        'Report pass/fail status and any failure details',
        'If no tests exist or tests are skipped, report that explicitly',
      ],
      outputFormat: 'Test results with pass/fail status',
    },
    outputSchema: {
      type: 'object',
      properties: {
        passing: { type: 'boolean' },
        summary: { type: 'string' },
        failures: { type: 'array', items: { type: 'string' } },
      },
      required: ['passing', 'summary'],
    },
  },
}));

const fixTests = defineTask('fix-tests', (args) => ({
  kind: 'agent',
  title: 'Fix failing tests',
  agent: {
    name: 'fixer',
    prompt: {
      role: 'A software engineer fixing test failures',
      task: `Fix the following test failures.

Failures:
${(args.failures || []).join('\n')}`,
      instructions: [
        'Read each failure carefully',
        'Determine if the failure is in the implementation or the test',
        'Fix the root cause',
        'Re-run the failing tests to confirm',
      ],
    },
    outputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string' },
        fixed: { type: 'boolean' },
      },
      required: ['summary', 'fixed'],
    },
  },
}));

const openMR = defineTask('open-mr', (args) => ({
  kind: 'agent',
  title: 'Open merge request',
  agent: {
    name: 'mr-opener',
    prompt: {
      role: 'A developer opening a merge request',
      task: `Commit the staged changes and open a merge request.

Summary of changes: ${args.summary}
Target branch: ${args.targetBranch || 'main'}
${args.issueId ? `Closes issue: #${args.issueId}` : ''}

Instructions for opening the MR:
1. Create a commit with a meaningful message
2. Push the branch to origin
3. Write the MR description (with real newlines, not \\n) to .git/mr-description.md. Include at the end:
   ${args.issueId ? `Closes #${args.issueId}\n\n` : ''}---
   🍇 Pinard worker: ${args.session}
   Process: swe | Parcelle: ${args.parcelle || 'default'} | Run ID: ${args.runId || 'unknown'}
4. Open MR via API (glab mr create does not work with ssh remotes):
   glab api projects/${args.encodedRepo}/merge_requests -X POST --hostname ${args.host} \\
     -f source_branch=$(git branch --show-current) \\
     -f target_branch=${args.targetBranch || 'main'} \\
     -f title="<title>" \\
     -F description=@.git/mr-description.md \\
     -f assignee_id=$(glab api users -X GET --hostname ${args.host} -f username=${args.assignee} 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin)[0]['id'])" 2>/dev/null) \\
     -f reviewer_ids=$(glab api users -X GET --hostname ${args.host} -f username=${args.reviewer} 2>/dev/null | python3 -c "import sys,json;print(json.load(sys.stdin)[0]['id'])" 2>/dev/null)
5. Parse the MR IID from the response
6. Run: aoc notify "[${args.session}] Opened MR !<iid> on ${args.project}"`,
    },
    outputSchema: {
      type: 'object',
      properties: {
        mrIid: { type: 'number' },
        title: { type: 'string' },
        url: { type: 'string' },
      },
      required: ['mrIid'],
    },
  },
}));

const trackMR = defineTask('track-mr', (args) => ({
  kind: 'agent',
  title: 'Track MR for review',
  agent: {
    name: 'tracker',
    prompt: {
      role: 'A developer registering an MR for monitoring',
      task: `Call the track_mr tool with MR number ${args.mrIid} and project "${args.project}" to register it for pipeline and review monitoring.`,
    },
    outputSchema: {
      type: 'object',
      properties: { tracked: { type: 'boolean' } },
      required: ['tracked'],
    },
  },
}));

const waitForEvent = defineTask('wait-for-event', (args) => ({
  kind: 'event',
  title: `Waiting for: ${(args.events || []).join(', ')}`,
  event: {
    types: args.events,
  },
}));

const addressReview = defineTask('address-review', (args) => ({
  kind: 'agent',
  title: 'Address review feedback',
  agent: {
    name: 'reviewer-responder',
    prompt: {
      role: 'A developer addressing code review feedback',
      task: `Address the following review comments. Make the requested changes, reply to each comment on the MR, and push.

${args.message || JSON.stringify(args.comments)}`,
      instructions: [
        'Read each review comment carefully',
        'Make the requested changes',
        'Reply to each comment explaining what you did',
        'Push the updated code',
        'Confirm the push actually landed on the remote before signaling completion — run `git fetch origin && test "$(git rev-parse HEAD)" = "$(git rev-parse origin/$(git branch --show-current))"`; if it does not match, push again and re-check',
        `Only once the push is confirmed, run: aoc notify "[${args.session}] Addressed review feedback"`,
      ],
    },
    outputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string' },
        commentsAddressed: { type: 'number' },
      },
      required: ['summary'],
    },
  },
}));

const fixPipeline = defineTask('fix-pipeline', (args) => ({
  kind: 'agent',
  title: `Fix ${args.isMain ? 'main' : 'MR'} pipeline failure`,
  agent: {
    name: 'pipeline-fixer',
    prompt: {
      role: 'A developer investigating and fixing a CI pipeline failure',
      task: `The ${args.isMain ? 'main branch' : 'MR'} pipeline failed. Investigate the logs and fix.

Pipeline URL: ${args.pipelineUrl || '(check latest pipeline)'}
Message: ${args.message || ''}`,
      instructions: [
        'Check the pipeline logs to identify the root cause',
        'Determine if this is a legitimate error, a transient timing issue, or a GitLab infra issue',
        'If transient/infra: re-run the failed job via the API',
        'If legitimate: fix the code and push',
        `Run: aoc notify "[${args.session}] ${args.isMain ? 'Fixed main pipeline' : 'Fixed pipeline'}"`,
      ],
    },
    outputSchema: {
      type: 'object',
      properties: {
        summary: { type: 'string' },
        cause: { type: 'string', enum: ['legitimate', 'transient', 'infra'] },
        fixed: { type: 'boolean' },
      },
      required: ['summary', 'cause', 'fixed'],
    },
  },
}));

const claimIssue = defineTask('claim-issue', (args) => ({
  kind: 'agent',
  title: 'Claim issue and mark in-progress',
  agent: {
    name: 'issue-claimer',
    prompt: {
      role: 'A developer starting work on a GitLab issue',
      task: `Mark issue #${args.issueId} as in-progress and assign to pinard.

Use the update_issue tool with:
- repo: "${args.repo}"
- issue: ${args.issueId}
- add_labels: "in-progress"
- assignee: "${args.assignee}"

This task is ONLY claiming the issue. Do NOT read or change any code, do NOT analyze,
implement, commit, push, or open an MR — those are separate later tasks. After the
update_issue call, call task:post and stop.`,
    },
    outputSchema: {
      type: 'object',
      properties: {
        claimed: { type: 'boolean' },
      },
      required: ['claimed'],
    },
  },
}));

const commentOnIssue = defineTask('comment-on-issue', (args) => ({
  kind: 'agent',
  title: `Update issue: ${args.phase}`,
  agent: {
    name: 'issue-commenter',
    prompt: {
      role: 'A developer posting a progress update on a GitLab issue',
      task: `Post a comment on issue #${args.issueId} with this progress update. Use the update_issue tool.

Comment: "🔄 **${args.phase}**\\n${args.detail}"`,
    },
    outputSchema: {
      type: 'object',
      properties: { commented: { type: 'boolean' } },
      required: ['commented'],
    },
  },
}));

const discardIssue = defineTask('discard-issue', (args) => ({
  kind: 'agent',
  title: 'Discard issue — unassign and label',
  agent: {
    name: 'issue-discarder',
    prompt: {
      role: 'A developer marking an issue as discarded by pinard',
      task: `Mark issue #${args.issueId} as discarded. Run these API calls:

1. Unassign pinard:
   glab api projects/${args.encodedRepo}/issues/${args.issueId} -X PUT --hostname ${args.host} -f assignee_ids=[]

2. Remove "in-progress" and add "pinard:discarded" label:
   glab api projects/${args.encodedRepo}/issues/${args.issueId} -X PUT --hostname ${args.host} -f remove_labels=in-progress -f add_labels=pinard:discarded`,
    },
    outputSchema: {
      type: 'object',
      properties: { discarded: { type: 'boolean' } },
      required: ['discarded'],
    },
  },
}));

const postCapsuleResult = defineTask('post-capsule-result', (args) => ({
  kind: 'agent',
  title: 'Post HTML result to Mnemosyne',
  agent: {
    name: 'result-poster',
    prompt: {
      role: 'A developer posting a completion result to Mnemosyne',
      task: `Run the following command to render the capsule report and post it to Mnemosyne:

\`\`\`
${args.command}
\`\`\`

If the command fails, report the error. The command may print warnings to stderr — that is normal.`,
      instructions: [
        'Run the command exactly as given',
        'Report success or failure',
      ],
    },
    outputSchema: {
      type: 'object',
      properties: {
        posted: { type: 'boolean' },
        error: { type: 'string' },
      },
      required: ['posted'],
    },
  },
}));

const writeReport = defineTask('write-capsule-report', (args) => ({
  kind: 'agent',
  title: 'Write capsule result report',
  agent: {
    name: 'report-writer',
    prompt: {
      role: 'A developer writing a completion report for a funded capsule run',
      task: `Write a completion report for this funded run to the file \`${args.runDir}/capsule-report.md\`.

The report should include:
1. A brief summary of the work completed
2. Key decisions and changes made
3. Links to relevant resources: MR, issue, pipeline
4. Any notable findings or outcomes

MR: ${args.mrIid ? '!' + args.mrIid : 'N/A'}
Issue: ${args.issueId ? '#' + args.issueId : 'N/A'}
Session: ${args.session}

Write the file now using bash: \`cat > ${args.runDir}/capsule-report.md << 'EOF'\n<content>\nEOF\``,
      instructions: [
        'Write a concise but complete markdown report',
        'Include all relevant issue/MR/pipeline URLs you have available',
        'Save the file to the exact path specified',
        'Confirm the file was written',
      ],
    },
    outputSchema: {
      type: 'object',
      properties: {
        written: { type: 'boolean' },
      },
      required: ['written'],
    },
  },
}));

const closeIssue = defineTask('close-issue', (args) => ({
  kind: 'agent',
  title: 'Close issue after successful delivery',
  agent: {
    name: 'issue-closer',
    prompt: {
      role: 'A developer closing a completed issue',
      task: `Close issue #${args.issueId} and post a completion summary.

Use the update_issue tool to:
1. Remove label "in-progress" (if present)
2. Add label "done"
3. Close the issue
4. Post a comment:
   "✅ **Delivered**
   - MR: !${args.mrIid}
   - Main pipeline: passed
   - Session: ${args.session}
   - Completed: ${new Date().toISOString()}"`,
    },
    outputSchema: {
      type: 'object',
      properties: {
        closed: { type: 'boolean' },
      },
      required: ['closed'],
    },
  },
}));

// ── Process definition ───────────────────────────────────────

export async function process(inputs = {}, ctx) {
  const project = inputs.project || '';
  const host = inputs.host;
  const encodedRepo = inputs.encodedRepo || '';
  const targetBranch = inputs.targetBranch || 'main';
  const session = inputs.session || '';
  const assignee = inputs.assignee || '';
  const reviewer = inputs.reviewer || '';
  const testStrategy = inputs.testStrategy || 'local';
  const testCommand = inputs.testCommand || '';

  // Phase 0: Claim issue (if issue-driven)
  if (inputs.issueId) {
    await ctx.task(claimIssue, {
      issueId: inputs.issueId,
      session,
      parcelle: inputs.parcelle || '',
      runId: inputs.runId || '',
      repo: inputs.repo || '',
      encodedRepo,
      host,
      assignee,
    });
  }

  // Phase 1: Fetch issue content (if issue-driven)
  let issueContent = inputs.prompt || '';
  if (inputs.issueId) {
    const issue = await ctx.task(fetchIssue, {
      issueId: inputs.issueId,
      project,
    });
    issueContent = `Issue #${inputs.issueId}: ${issue.title}\n\n${issue.description}`;
  }

  // Phase 2: Analyze
  const analysis = await ctx.task(analyzeWork, {
    issue: issueContent,
    prompt: issueContent,
    project,
  });

  // Comment: analysis done
  if (inputs.issueId) {
    await ctx.task(commentOnIssue, {
      issueId: inputs.issueId,
      phase: 'Analysis complete',
      detail: `Change type: ${analysis.changeType}\\nPlan: ${analysis.plan}`,
    });
  }

  // Phase 2: Implement
  const impl = await ctx.task(implement, {
    plan: analysis.plan,
    affectedFiles: analysis.affectedFiles,
  });

  // Comment: implementation done
  if (inputs.issueId) {
    await ctx.task(commentOnIssue, {
      issueId: inputs.issueId,
      phase: 'Implementation complete',
      detail: impl.summary,
    });
  }

  // Phase 3: Test (skip for docs, skip for k3d strategy)
  if (analysis.changeType !== 'docs' && testStrategy === 'local') {
    let tests = await ctx.task(runTests, { command: testCommand });
    let attempts = 0;
    while (!tests.passing && attempts < 3) {
      await ctx.task(fixTests, { failures: tests.failures });
      tests = await ctx.task(runTests, { command: testCommand });
      attempts++;
    }
    if (!tests.passing) {
      await ctx.breakpoint({
        question: `Tests failing after ${attempts} attempts: ${tests.summary}. Proceed anyway?`,
      });
    }
  }

  // Phase 4: Open MR and track
  if (inputs.issueId) {
    await ctx.task(commentOnIssue, {
      issueId: inputs.issueId,
      phase: 'Opening MR',
      detail: `Target branch: ${targetBranch}`,
    });
  }
  const mr = await ctx.task(openMR, {
    summary: impl.summary,
    targetBranch,
    issueId: inputs.issueId,
    encodedRepo,
    host,
    assignee,
    reviewer,
    session,
    project,
    parcelle: inputs.parcelle || '',
    runId: inputs.runId || '',
  });
  await ctx.task(trackMR, { mrIid: mr.mrIid, project });

  if (inputs.issueId) {
    await ctx.task(commentOnIssue, {
      issueId: inputs.issueId,
      phase: 'MR opened',
      detail: `MR !${mr.mrIid}${mr.url ? ` — ${mr.url}` : ''}\\nWaiting for review and CI.`,
    });
  }

  // Phase 5: Review loop — wait for merge or close
  let merged = false;
  let closed = false;
  while (!merged && !closed) {
    const event = await ctx.task(waitForEvent, {
      events: ['pipeline_failed', 'pipeline_cancelled', 'review_comment', 'mr_merged', 'auto_merged', 'mr_closed'],
    });

    if (event.type === 'pipeline_failed') {
      await ctx.task(fixPipeline, {
        pipelineUrl: event.url,
        message: event.message,
        session,
        isMain: false,
      });
    } else if (event.type === 'pipeline_cancelled') {
      await ctx.breakpoint({
        question: `Pipeline was cancelled (${event.url || 'unknown'}). What should I do? Re-run, fix something, or wait?`,
      });
    } else if (event.type === 'review_comment') {
      await ctx.task(addressReview, {
        comments: event.notes,
        message: event.message,
        session,
      });
    } else if (event.type === 'mr_merged' || event.type === 'auto_merged') {
      merged = true;
    } else if (event.type === 'mr_closed') {
      closed = true;
    }
  }

  if (closed) {
    if (inputs.issueId) {
      await ctx.task(commentOnIssue, {
        issueId: inputs.issueId,
        phase: 'MR closed — work discarded',
        detail: `MR !${mr.mrIid} was closed without merging. Remove \`pinard:discarded\` label and reassign to retry.`,
      });
      await ctx.task(discardIssue, {
        issueId: inputs.issueId,
        encodedRepo,
        host,
        assignee,
      });
    }
    return { mrIid: mr.mrIid, status: 'closed' };
  }

  // Phase 6: Monitor main pipeline
  let mainPassed = false;
  while (!mainPassed) {
    const mainEvent = await ctx.task(waitForEvent, {
      events: ['main_pipeline_passed', 'main_pipeline_failed'],
    });

    if (mainEvent.type === 'main_pipeline_passed') {
      mainPassed = true;
    } else if (mainEvent.type === 'main_pipeline_failed') {
      // Fix on same branch, open new MR
      await ctx.task(fixPipeline, {
        pipelineUrl: mainEvent.url,
        message: mainEvent.message,
        session,
        isMain: true,
      });

      // Open fix MR (same branch, new commit)
      const fixMR = await ctx.task(openMR, {
        summary: 'Fix main pipeline failure',
        targetBranch,
        encodedRepo,
        host,
        assignee,
        reviewer,
        session,
        project,
        parcelle: inputs.parcelle || '',
        runId: inputs.runId || '',
      });
      await ctx.task(trackMR, { mrIid: fixMR.mrIid });

      // Inner review loop for the fix MR
      let fixMerged = false;
      while (!fixMerged) {
        const fixEvent = await ctx.task(waitForEvent, {
          events: ['pipeline_failed', 'review_comment', 'mr_merged', 'auto_merged'],
        });
        if (fixEvent.type === 'pipeline_failed') {
          await ctx.task(fixPipeline, { pipelineUrl: fixEvent.url, session, isMain: false });
        } else if (fixEvent.type === 'review_comment') {
          await ctx.task(addressReview, { comments: fixEvent.notes, message: fixEvent.message, session });
        } else if (fixEvent.type === 'mr_merged' || fixEvent.type === 'auto_merged') {
          fixMerged = true;
        }
      }
      // Loop back to wait for main pipeline again
    }
  }

  // Phase 7: Close issue (if issue-driven)
  if (inputs.issueId) {
    await ctx.task(closeIssue, {
      issueId: inputs.issueId,
      mrIid: mr.mrIid,
      session,
    });
  }

  // Phase 8: Capsule result posting (funded runs only)
  const capsuleContract = inputs.capsuleContract || process.env.PINARD_CAPSULE_CONTRACT || '';
  if (capsuleContract) {
    // Determine the run directory so the agent can write capsule-report.md there.
    const runDir = process.env.BABYSITTER_RUN_DIR ||
      (process.env.BABYSITTER_RUNS_DIR && process.env.RUN_ID
        ? `${process.env.BABYSITTER_RUNS_DIR}/${process.env.RUN_ID}`
        : '');

    if (runDir) {
      // Ask the agent to write a markdown summary.
      await ctx.task(writeReport, {
        runDir,
        mrIid: mr.mrIid,
        issueId: inputs.issueId || '',
        session,
      });
    }

    // Render md→HTML and PATCH result_patch_url via aoc.
    // aoc capsule-post-result finds capsule.json via env (BABYSITTER_RUN_DIR / BABYSITTER_RUNS_DIR+RUN_ID).
    // It falls back to a minimal deterministic report if capsule-report.md is absent.
    const postCmd = [
      'aoc', 'capsule-post-result',
      '--status', 'completed',
      ...(inputs.vignoble ? ['--vignoble', inputs.vignoble] : []),
      ...(mr.url ? ['--mr-url', JSON.stringify(mr.url)] : []),
      ...(inputs.issueId && inputs.host && inputs.encodedRepo
        ? ['--issue-url', JSON.stringify(`https://${inputs.host}/${decodeURIComponent(inputs.encodedRepo)}/-/issues/${inputs.issueId}`)]
        : []),
    ].join(' ');
    await ctx.task(postCapsuleResult, { command: postCmd });
  }

  return { mrIid: mr.mrIid, status: 'main_pipeline_passed' };
}
