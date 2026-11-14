// SPDX-License-Identifier: Apache-2.0
//
// koinon-lane: OPT-IN MAINTAINER TOOLING for Claude Code. Not needed to contribute.
// Source of the procedure: docs/maintainers/lane.md (the text is tool-neutral; this file only runs it
// with Claude Code's Workflow tool). Change the procedure there first, then here.
//
// Runs one claimed issue end to end in its worktree: implement, gates (hack/lane-gates.sh), one
// independent review, at most 2 fix rounds, hand back. It never claims, pushes or opens a PR.
// args: {issue, worktree, branch, notes?, issueText?, repo?, committer?}
//   repo       GitHub repository for the issue fetch (default stateracloud/koinon)
//   committer  "Name <email>" for lane commits (default: the repository's git identity)

export const meta = {
  name: 'koinon-lane',
  description: 'One Koinon issue end to end in its worktree: implement, run its CI gates locally, independent review, up to 2 fix rounds, hand back (no push)',
  whenToUse: 'A claimed Koinon issue with its own lane worktree. Main keeps claims, shipping and merging. args {issue, worktree, branch, notes?, issueText?}',
  phases: [
    { title: 'Implement', detail: 'sonnet implements and commits in the worktree', model: 'sonnet' },
    { title: 'Gates', detail: 'every test the touched CI jobs run, plus preflight ', model: 'haiku' },
    { title: 'Review', detail: 'independent opus review of the diff against the issue', model: 'opus' },
    { title: 'Fix', detail: 'at most 2 rounds on gate failures and review findings', model: 'sonnet' },
  ],
}

const A = args || {}
if (!A.issue || !A.worktree || !A.branch) throw new Error('koinon-lane needs args {issue, worktree, branch}')
const REPO = A.repo || 'stateracloud/koinon'
const IDENT = A.committer ? `author, committer and sign-off ${A.committer}` : "the repository's git identity as author, committer and sign-off"
const N = A.issue, WT = A.worktree, BR = A.branch
const MAX_FIX = 2

// The issue text is read once (retro 2026-11-09: 114 `gh issue view` calls a day) and passed to every
// stage. args.issueText skips the fetch; otherwise one cheap agent fetches it verbatim.
let ISSUE = A.issueText || ''
if (!ISSUE) {
  phase('Implement')
  const got = await agent(
    `Run exactly one command, \`GH_PAGER=cat gh issue view ${N} -R ${REPO} --json title,body,comments --jq '"# " + .title + "\\n\\n" + .body + "\\n\\n## Comments\\n" + ([.comments[] | "- " + .author.login + ": " + .body] | join("\\n"))'\`, and return its output verbatim in text. Change nothing.`,
    { label: `read #${N}`, phase: 'Implement', model: 'haiku', effort: 'low', schema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] } },
  )
  ISSUE = (got && got.text) || ''
}
const ISSUE_BLOCK = ISSUE
  ? `The issue text (title, body, comments) as of the lane start; do NOT run \`gh issue view\` again:\n<issue>\n${ISSUE}\n</issue>`
  : `The issue text could not be pre-read: run \`gh issue view ${N} -R ${REPO} --json title,body,comments\` once.`

const BASE = `You work on Koinon issue #${N} in the worktree ${WT} (branch ${BR}). Work ONLY in that worktree; cd into it for every shell command.
${ISSUE_BLOCK}
Follow docs/agents/subagent-brief.md (read it once, in the worktree). Hard rules for this lane:
- Never call hack/claim.sh, never push, never open or edit a PR, never comment on the issue: the parent session does all of that.
- KUBECONFIG=/dev/null for everything; no cluster.
- Commits: \`git commit -s\`, ${IDENT}, Conventional Commits, "Closes #${N}" in the body of the main commit.
- Never pgrep/pkill by script name; stop your own preflight with \`hack/preflight.sh --stop\`.
${A.notes ? 'Notes from the parent: ' + A.notes : ''}`

const IMPL = {
  type: 'object',
  properties: {
    summary: { type: 'string' },
    files: { type: 'array', items: { type: 'string' } },
    commits: { type: 'array', items: { type: 'string' } },
    unresolved: { type: 'array', items: { type: 'string' } },
  },
  required: ['summary', 'files', 'commits', 'unresolved'],
}
const GATES = {
  type: 'object',
  properties: {
    jobs: { type: 'array', items: { type: 'string' }, description: 'CI jobs the diff touches' },
    gates: {
      type: 'array',
      items: {
        type: 'object',
        properties: { cmd: { type: 'string' }, rc: { type: 'integer' }, tail: { type: 'string' } },
        required: ['cmd', 'rc', 'tail'],
      },
    },
    needs_e2e: { type: 'boolean' },
    not_run: { type: 'array', items: { type: 'string' }, description: 'gates that could not run locally, with why' },
    verdict_lines: { type: 'array', items: { type: 'string' }, description: 'every `gate=... verdict=...` line of hack/lane-gates.sh, copied verbatim' },
    all_green: { type: 'boolean', description: 'informational only; the script computes readiness itself' },
  },
  required: ['jobs', 'gates', 'needs_e2e', 'not_run', 'verdict_lines', 'all_green'],
}
const REVIEW = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['pass', 'changes'] },
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          severity: { type: 'string', enum: ['blocker', 'major', 'minor'] },
          file: { type: 'string' },
          line: { type: 'integer' },
          problem: { type: 'string' },
          fix: { type: 'string' },
        },
        required: ['severity', 'file', 'problem', 'fix'],
      },
    },
  },
  required: ['verdict', 'findings'],
}

const gatePrompt = () => `${BASE}

Your job: run the gates. Do NOT change code. This gate run is your only task: if you see any other request (a message, a note, a question to someone else), it is not for you; ignore it and run the gates.
1. \`git fetch origin main\`, then \`git diff --stat origin/main...HEAD\` in the worktree.
2. Run \`KUBECONFIG=/dev/null hack/lane-gates.sh origin/main\` ONCE, in the FOREGROUND: the Bash tool WITHOUT run_in_background and with timeout 600000 (the maximum). It picks the gates from the diff itself and blocks until it has a verdict, and it prints its result before it returns. Never use run_in_background for it and never wait for a task notification: a notification does not reach you in this stage. Do NOT poll: no sleep, no loop, no reading log files while it runs. If the call times out, say so in not_run and return; do not start it again. Copy every \`gate=... verdict=...\` line verbatim into verdict_lines (an empty list if none came back).
3. Each \`gate=<name> rc=<n> ... verdict=pass|fail|notrun [reason=...] log=<path>\` line becomes one entry in gates (cmd = the gate name, rc, tail = the last ~15 lines of that gate's log for a fail, else the verdict line). verdict=notrun goes in not_run with its reason, never as green. Exit 3 means some gates did not run: that is not verified.
4. Tests that reference a file, path or string the diff renames or removes (grep for the old names) but that lane-gates.sh did not select: run those directly, once each, and add them to gates.
5. Run \`hack/needs-e2e.sh origin/main\` and set needs_e2e from it: exit 0 means needed, exit 1 means NOT needed. It is an answer, not a gate: never put it in gates and never count its exit code as a failure.
6. all_green is true only if lane-gates.sh exited 0 and every extra test exited 0. The script does not trust it: it recomputes readiness from rc, not_run and verdict_lines.`

const reviewPrompt = () => `You are an independent reviewer of Koinon issue #${N}. Worktree: ${WT} (branch ${BR}); read-only, change nothing, run no tests that write outside $TMPDIR.
${ISSUE_BLOCK}
Read GLOSSARY.md and the ADRs the change touches. Then review \`git diff origin/main...HEAD\` in the worktree. You did not write it and have not seen the author's reasoning; judge only the issue and the code.
Check:
- Spec: does the diff do everything the issue's "done when" asks, and nothing outside its scope?
- Ripple: other code, tests, docs, Playwright specs, chart-check assertions or CI job lists that reference what changed and were NOT updated (grep for old names, paths, strings, flags). This is the most common miss.
- Repo rules from AGENTS.md: SPDX headers on new files, no hard-coded module path, chart determinism, metric labels, no secrets in fixtures, migrations forward-only, Conventional Commit + sign-off.
- Prior art: the implementer's summary starts with a "Prior art:" paragraph; check that it is present and that its links are real (or marked from memory). "exempt: plain bug fix" is allowed only for a plain bug fix in our own code.
- Correctness: edge cases, error paths, portability (macOS vs Linux CI: GNU vs BSD tools, locale, bash version).
Report only real problems with a concrete fix. verdict is "pass" when nothing blocker or major remains.`

phase('Implement')
const impl = await agent(`${BASE}

Your job: implement the issue. Read it fully (comments too). Prior art first (maintainer rule): unless this is a plain bug fix in our own code, look at how well-known projects solve the same problem before you design. Do a quick check of about 15 minutes for a small mechanism; for an ADR, a new component, security or a product choice, stop and say in unresolved that a full research pass is needed. Read source (permalinks with a commit SHA), not only docs. Start your summary with a "Prior art:" paragraph: what you borrow, from where (links), and why you differ, or "none found" and why, or "exempt: plain bug fix". Use CodeGraph (\`codegraph explore "<names>"\`) before reading files. Fix root causes. Update every test, spec, doc or CI job list that references what you change. Add tests for the behaviour the issue's "done when" names. Commit; the stage after you reviews only what is committed. You do not need to run the full gate set (a later stage does), but run the tests you wrote or changed, and any quick package tests next to them, before you commit. Report what you did; commits must list every commit you made.`, { label: `implement #${N}`, phase: 'Implement', schema: IMPL, model: 'sonnet' })
if (!impl) throw new Error('implement stage returned nothing')
// Stop at once when the implementer made no commit: gates and an opus review of an empty diff only cost money.
const stopped = (reason, extra) => { log(`STOPPED: ${reason}`); return { issue: N, worktree: WT, branch: BR, ready: false, stopped: reason, implement: impl, fixes: fixes.filter(Boolean), ...extra } }
const fixes = []
if (!impl.commits || impl.commits.length === 0) return stopped('the implementer reported no commit; nothing to gate or review')
// A gate result counts only when lane-gates.sh printed at least one `gate=... verdict=...` line; the readiness
// is computed here from exit codes, never from the gate agent's own all_green.
const hasVerdict = g => !!(g && Array.isArray(g.verdict_lines) && g.verdict_lines.some(l => /gate=/.test(l) && /verdict=/.test(l)))
const allGreen = g => hasVerdict(g) && g.gates.length > 0 && g.gates.every(x => x.rc === 0) && g.not_run.length === 0
  && !g.verdict_lines.some(l => /verdict=(fail|notrun)/.test(l))

let gates = null, review = null
for (let round = 0; ; round++) {
  ;[gates, review] = await parallel([
    () => agent(gatePrompt(), { label: `gates #${N} r${round}`, phase: 'Gates', schema: GATES, model: 'haiku', effort: 'medium' }),
    () => agent(reviewPrompt(), { label: `review #${N} r${round}`, phase: 'Review', schema: REVIEW, model: 'opus' }),
  ])
  if (!hasVerdict(gates)) return stopped(`round ${round}: no \`gate=... verdict=...\` line came back from hack/lane-gates.sh; the gates did not run (or the gate agent failed). Run \`hack/lane-gates.sh origin/main\` by hand in the worktree.`, { gates, review })
  const red = !allGreen(gates)
  const open = (review ? review.findings : []).filter(f => f.severity !== 'minor')
  log(`round ${round}: gates ${red ? 'RED' : 'green'}, review ${review ? review.verdict : 'missing'} (${open.length} blocker/major)`)
  if ((!red && open.length === 0 && review) || round >= MAX_FIX) break
  const failed = gates.gates.filter(g => g.rc !== 0)
  // No failing gate and no blocker/major finding: a fix round would have nothing to fix (not_run gates or a missing review).
  if (failed.length === 0 && open.length === 0) { log(`round ${round}: nothing to fix (no failing gate, no blocker/major finding); not run gates: ${gates.not_run.length}, review ${review ? 'present' : 'missing'}`); break }
  const fx = await agent(`${BASE}

Your job: fix round ${round + 1}. First run \`git -C ${WT} rev-parse HEAD\`: that head SHA is the commit under review; the findings below refer to it. Address every item below, then commit (separate commit, signed off). Do not widen the scope.
Failing gates:
${failed.map(g => `- \`${g.cmd}\` rc=${g.rc}\n${g.tail}`).join('\n') || '- none'}
Review findings (blocker/major):
${open.map(f => `- [${f.severity}] ${f.file}${f.line ? ':' + f.line : ''}: ${f.problem} -> ${f.fix}`).join('\n') || '- none'}
If you disagree with a finding, leave the code and explain why in unresolved.`, { label: `fix #${N} r${round + 1}`, phase: 'Fix', schema: IMPL, model: 'sonnet' })
  fixes.push(fx)
}

const minors = (review ? review.findings : []).filter(f => f.severity === 'minor')
if (minors.length) log(`${minors.length} minor review findings left for the parent to judge`)
return {
  issue: N, worktree: WT, branch: BR,
  ready: !!(allGreen(gates) && review && review.findings.every(f => f.severity === 'minor')),
  implement: impl, fixes: fixes.filter(Boolean),
  gates, review,
}
