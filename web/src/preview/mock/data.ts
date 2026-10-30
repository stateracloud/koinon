// SPDX-License-Identifier: Apache-2.0

// Typed mock data for the v2 UI preview, seeded from the approved mockups' sample data (scratchpad mockups-v2, the
// renderVals() of each *.dc.html). Nothing here talks to the API. Screens import what they need; a screen that needs
// data only it uses keeps it in its own module under src/preview/mock/ (for example mock/run.ts) so parallel work
// does not collide here.

// ---------------------------------------------------------------------------------------------------------------
// States

/** TaskState is the board's seven states (README "Task states and ageing"). */
export type TaskState = 'submitted' | 'working' | 'blocked' | 'stale' | 'completed' | 'failed' | 'canceled'

export const taskStates: { key: TaskState; label: string }[] = [
  { key: 'submitted', label: 'Submitted' },
  { key: 'working', label: 'Working' },
  { key: 'blocked', label: 'Blocked on you' },
  { key: 'stale', label: 'Stale' },
  { key: 'completed', label: 'Completed' },
  { key: 'failed', label: 'Failed' },
  { key: 'canceled', label: 'Canceled' },
]

export type RunState = 'queued' | 'starting' | 'running' | 'parked' | 'succeeded' | 'failed' | 'canceled'
export type AgentState = 'active' | 'draft' | 'unhealthy' | 'paused' | 'archived'
export type ApprovalState = 'pending' | 'approved' | 'denied' | 'executed' | 'failed' | 'expired' | 'canceled'
export type ConnectionState = 'ok' | 'failing' | 'untested'

/** runDot maps a run state to the task-state dot colour it shows (the Runs table). */
export const runDot: Record<RunState, TaskState> = {
  queued: 'submitted',
  starting: 'working',
  running: 'working',
  parked: 'blocked',
  succeeded: 'completed',
  failed: 'failed',
  canceled: 'canceled',
}

// ---------------------------------------------------------------------------------------------------------------
// People, spaces and orgs

export type Person = { name: string; initials: string; email: string }

export const me: Person = { name: 'procoduck', initials: 'AS', email: 'andre@example.test' }

/** Hue is a design-token scale name; marks use --<hue>-200 ground, --<hue>-1000 text, --<hue>-600 ring. */
export type Hue = 'blue' | 'green' | 'gold' | 'teal' | 'gray' | 'orange' | 'sky' | 'lavender' | 'slate' | 'red' | 'amber' | 'purple'

export type OrgRole = 'Owner' | 'Admin' | 'Member'

export type Org = {
  id: string
  slug: string
  name: string
  initials: string
  hue: Hue
  role: OrgRole
  agents: number
  /** Items waiting on the viewer in this org (the switcher's count). */
  waiting: number
}

export const orgs: Org[] = [
  { id: 'pe', slug: 'platform-engineering', name: 'Platform Engineering', initials: 'PE', hue: 'blue', role: 'Owner', agents: 5, waiting: 7 },
  { id: 'fo', slug: 'finops', name: 'FinOps', initials: 'FO', hue: 'green', role: 'Member', agents: 2, waiting: 0 },
  { id: 'dx', slug: 'developer-experience', name: 'Developer Experience', initials: 'DX', hue: 'gold', role: 'Admin', agents: 3, waiting: 0 },
]

/** The personal space. */
export const idion = { name: 'Idion', initials: 'Id', hue: 'teal' as Hue, blurb: 'Your own space, with Daimon', waiting: 1 }

// ---------------------------------------------------------------------------------------------------------------
// Agents

export type Agent = {
  id: string
  name: string
  team: string
  /** Display name of the agent this one reports to, or "the org". */
  reports: string
  model: string
  modelFrom: string
  state: AgentState
  running: boolean
  runId?: string
  runs?: { id: string; state: RunState }[]
  lastRun?: string
  spendToday: number
  limitToday: number
  tokensToday: string
  cacheHit: string
  waiting: number
  oldestWaiting?: string
  health: string
  nextSchedule: string
}

export const agents: Agent[] = [
  { id: 'finops-analyst', name: 'FinOps Analyst', team: 'FinOps', reports: 'the org', model: 'claude-haiku-5.5', modelFrom: 'agent default', state: 'active', running: true, runs: [{ id: '3f9a1c2e7b', state: 'running' }], spendToday: 1.86, limitToday: 5, tokensToday: '1.24M', cacheHit: '82%', waiting: 3, oldestWaiting: '4 d', health: 'Healthy', nextSchedule: 'Daily cost report · tomorrow 07:00' },
  { id: 'lcm-upgrader', name: 'LCM Upgrader', team: 'Upgrades', reports: 'the org', model: 'gpt-6-luna', modelFrom: 'pinned in v7', state: 'active', running: true, runs: [{ id: '9c41d07a2e', state: 'running' }], spendToday: 9.12, limitToday: 10, tokensToday: '2.61M', cacheHit: '47%', waiting: 3, oldestWaiting: '9 d', health: 'Healthy', nextSchedule: 'Upgrade window · Mon 02:00' },
  { id: 'researcher', name: 'Researcher', team: 'Research', reports: 'the org', model: 'deepseek-v4.1-flash', modelFrom: 'org default', state: 'active', running: false, lastRun: 'Last run 26 min ago · succeeded · 4 min 12 s', spendToday: 0.71, limitToday: 3, tokensToday: '3.90M', cacheHit: '64%', waiting: 1, oldestWaiting: '2 d', health: 'Healthy', nextSchedule: 'No schedule' },
  { id: 'release-notes-scout', name: 'Release Notes Scout', team: 'Upgrades', reports: 'LCM Upgrader', model: 'deepseek-v4.1-flash', modelFrom: 'org default', state: 'unhealthy', running: false, lastRun: 'Last run yesterday 18:00 · failed · provider returned 401', spendToday: 0.09, limitToday: 1, tokensToday: '410k', cacheHit: '71%', waiting: 0, health: 'Unhealthy, provider failing', nextSchedule: 'Release scan · today 18:00' },
  { id: 'docs-gardener', name: 'Docs Gardener', team: 'Research', reports: 'Researcher', model: 'claude-haiku-5.5', modelFrom: 'template default', state: 'draft', running: false, lastRun: 'Never run', spendToday: 0, limitToday: 2, tokensToday: '0', cacheHit: 'No runs', waiting: 0, health: 'Not started', nextSchedule: 'No schedule' },
]

export function agentByName(name: string): Agent | undefined {
  return agents.find((a) => a.name === name)
}

/** The org's spend today against its daily limit (Overview header). */
export const orgSpendToday = { spent: 11.78, limit: 25, running: 2 }

// ---------------------------------------------------------------------------------------------------------------
// Tasks (the board) and what is waiting on you

export type Task = {
  title: string
  state: TaskState
  agent: string
  source: 'schedule' | 'trigger' | 'manual' | 'concierge'
  /** Hours blocked on you, for blocked tasks. */
  hoursWaiting?: number
  subs?: { name: string; state: TaskState }[]
}

export const tasks: Task[] = [
  { title: 'Tag untagged EBS volumes', state: 'submitted', agent: 'FinOps Analyst', source: 'schedule' },
  { title: 'Weekly cost report, week 44', state: 'working', agent: 'FinOps Analyst', source: 'schedule' },
  { title: 'Upgrade cert-manager to 1.19', state: 'working', agent: 'LCM Upgrader', source: 'trigger', subs: [{ name: 'Release Notes Scout', state: 'failed' }, { name: 'Staging smoke test', state: 'completed' }] },
  { title: 'Merge ingress-nginx 1.14 PR', state: 'blocked', agent: 'LCM Upgrader', source: 'trigger', hoursWaiting: 218 },
  { title: 'Which cost center owns the idle GPU nodes?', state: 'blocked', agent: 'FinOps Analyst', source: 'schedule', hoursWaiting: 98 },
  { title: 'Compare three vector databases', state: 'blocked', agent: 'Researcher', source: 'manual', hoursWaiting: 50 },
  { title: 'Rightsize the staging node pool', state: 'blocked', agent: 'FinOps Analyst', source: 'manual', hoursWaiting: 5 },
  { title: 'Draft the Kyverno removal notes', state: 'stale', agent: 'Docs Gardener', source: 'manual' },
  { title: 'Summarize Kubernetes 1.35 release notes', state: 'completed', agent: 'Release Notes Scout', source: 'schedule' },
  { title: 'Upgrade external-dns to 0.16', state: 'completed', agent: 'LCM Upgrader', source: 'trigger' },
  { title: 'Check Argo CD 3.2 breaking changes', state: 'failed', agent: 'Release Notes Scout', source: 'schedule' },
  { title: 'Draft the GPU reservation proposal', state: 'canceled', agent: 'Researcher', source: 'concierge' },
]

export type InboxKind = 'Approval' | 'Question' | 'Plan' | 'Tool call' | 'Needs your reply' | 'Mention' | 'Reminder'

export type InboxItem = {
  id: string
  kind: InboxKind
  title: string
  agent: string
  space: string
  hoursWaiting: number
  /** Priority items are the ones only you can unblock; the rest are "Other". */
  priority: boolean
}

export const inboxItems: InboxItem[] = [
  { id: 'pr', kind: 'Approval', title: 'Merge ingress-nginx 1.14 into platform-gitops', agent: 'LCM Upgrader', space: 'Platform Engineering', hoursWaiting: 218, priority: true },
  { id: 'q', kind: 'Question', title: 'Which cost center owns the idle GPU nodes?', agent: 'FinOps Analyst', space: 'Platform Engineering', hoursWaiting: 98, priority: true },
  { id: 'plan', kind: 'Plan', title: 'Upgrade cert-manager in staging: 3 steps, 30 min grant', agent: 'LCM Upgrader', space: 'Platform Engineering', hoursWaiting: 50, priority: true },
  { id: 'edit', kind: 'Tool call', title: 'Create a GitHub issue in platform-gitops', agent: 'Researcher', space: 'Platform Engineering', hoursWaiting: 26, priority: true },
  { id: 'snooze', kind: 'Needs your reply', title: 'Rightsize the staging node pool', agent: 'FinOps Analyst', space: 'Platform Engineering', hoursWaiting: 5, priority: true },
  { id: 'mention', kind: 'Mention', title: 'Docs Gardener mentioned you in "Kyverno removal notes"', agent: 'Docs Gardener', space: 'Platform Engineering', hoursWaiting: 30, priority: false },
  { id: 'reminder', kind: 'Reminder', title: 'Renew the conference booking', agent: 'Daimon', space: 'Idion', hoursWaiting: 3, priority: false },
]

/** The Overview's "Waiting on you" strip: 5 of 7, oldest first. */
export const waitingOnYou: { kind: InboxKind; title: string; agent: string; hoursWaiting: number }[] = [
  { kind: 'Approval', title: 'Merge ingress-nginx 1.14 into platform-gitops', agent: 'LCM Upgrader', hoursWaiting: 218 },
  { kind: 'Question', title: 'Which cost center owns the idle GPU nodes?', agent: 'FinOps Analyst', hoursWaiting: 98 },
  { kind: 'Plan', title: 'Grant 30 min of write access to staging', agent: 'LCM Upgrader', hoursWaiting: 50 },
  { kind: 'Question', title: 'Include pgvector in the comparison?', agent: 'Researcher', hoursWaiting: 26 },
  { kind: 'Needs your reply', title: 'Rightsize the staging node pool', agent: 'FinOps Analyst', hoursWaiting: 5 },
]
export const waitingTotal = 7

export type Notice = { id: string; urgent?: boolean; text: string; linkText: string; to: string }

export const notices: Notice[] = [
  { id: 'n1', urgent: true, text: 'Release Notes Scout is unhealthy: its provider failed the last 3 calls.', linkText: 'Open the agent', to: '/preview/agents/release-notes-scout' },
  { id: 'n2', text: 'LCM Upgrader has used 91% of today’s limit ($9.12 of $10.00).', linkText: 'See budgets', to: '/preview/spend' },
]

// ---------------------------------------------------------------------------------------------------------------
// Runs

export type Run = {
  id: string
  agent: string
  task: string
  state: RunState
  source: 'trigger' | 'schedule' | 'manual' | 'sub-run'
  started: string
  duration: string
  tokens: string
  cost: string
  parent?: string
}

export const runs: Run[] = [
  { id: '9c41d07a', agent: 'LCM Upgrader', task: 'Upgrade cert-manager to 1.19', state: 'running', source: 'trigger', started: '13:02', duration: '18 min', tokens: '412k', cost: '$1.38' },
  { id: '5be2a913', agent: 'Release Notes Scout', task: 'Read cert-manager 1.19 notes', state: 'failed', source: 'sub-run', started: '13:04', duration: '42 s', tokens: '18k', cost: '$0.01', parent: '9c41d07a' },
  { id: '3f9a1c2e', agent: 'FinOps Analyst', task: 'Weekly cost report, week 44', state: 'running', source: 'schedule', started: '12:40', duration: '40 min', tokens: '688k', cost: '$0.92' },
  { id: 'a71c0e44', agent: 'LCM Upgrader', task: 'Merge ingress-nginx 1.14 PR', state: 'parked', source: 'trigger', started: '19 Oct', duration: '9 d waiting', tokens: '231k', cost: '$0.77' },
  { id: '0d93be12', agent: 'Researcher', task: 'Compare three vector databases', state: 'succeeded', source: 'manual', started: '12:16', duration: '4 min 12 s', tokens: '940k', cost: '$0.31' },
  { id: '66ab21f0', agent: 'LCM Upgrader', task: 'Upgrade external-dns to 0.16', state: 'succeeded', source: 'trigger', started: 'Mon 02:00', duration: '26 min', tokens: '1.02M', cost: '$3.14' },
  { id: 'e2c7448d', agent: 'Release Notes Scout', task: 'Check Argo CD 3.2 breaking changes', state: 'failed', source: 'schedule', started: 'yesterday 18:00', duration: '11 s', tokens: '2k', cost: '$0.00' },
  { id: '7f01aa3c', agent: 'LCM Upgrader', task: 'Upgrade Loki chart to 7.2', state: 'canceled', source: 'manual', started: 'Sun 10:12', duration: '3 min', tokens: '44k', cost: '$0.15' },
]

/** Run 9c41d07a in detail (Run.dc.html). */
export const runDetail = {
  id: '9c41d07a2e',
  title: 'Upgrade cert-manager to 1.19',
  agent: 'LCM Upgrader',
  steps: [
    { n: 1, state: 'completed' as TaskState, title: 'Read the trigger', detail: 'GitHub release jetstack/cert-manager v1.19.0, published 12:58', meta: '0.4 s' },
    { n: 2, state: 'completed' as TaskState, title: 'Planned the upgrade', detail: 'gpt-6-luna · 22k in · 1.1k out · 0 cached', meta: '$0.08 · 7 s' },
    { n: 3, state: 'failed' as TaskState, title: 'Asked Release Notes Scout for the 1.19 notes', detail: 'Sub-run 5be2a913 failed: provider returned 401', meta: '42 s' },
    { n: 4, state: 'completed' as TaskState, title: 'Read the cluster state', detail: 'kubectl get across 6 namespaces, collapsed', meta: '11 s', repeats: 6 },
    { n: 5, state: 'completed' as TaskState, title: 'Rendered the chart diff', detail: 'helm template, retried once after OCI registry 503', meta: '1 min 20 s' },
    { n: 6, state: 'completed' as TaskState, title: 'Ran the staging smoke test', detail: 'Sub-run c03e88f1 succeeded', meta: '3 min 05 s' },
    { n: 7, state: 'completed' as TaskState, title: 'Model turns while editing values', detail: 'gpt-6-luna · 241k in · 14k out · 93k cached', meta: '$1.21 · 9 min', repeats: 4 },
    { n: 8, state: 'working' as TaskState, title: 'Opening the pull request', detail: 'github.create_pull_request is ask-tier: it will wait for your approval', meta: 'now' },
  ],
  /** Timeline bars: left and width in percent of the run so far. */
  bars: [
    { name: 'trigger', outcome: 'ok', left: 0, width: 1, label: '0.4 s' },
    { name: 'model · plan', outcome: 'model', left: 1, width: 4, label: '7 s' },
    { name: 'ask_agent · scout', outcome: 'err', left: 5, width: 4, label: '42 s' },
    { name: 'kubectl get ×6', outcome: 'ok', left: 9, width: 6, label: '11 s' },
    { name: 'helm template', outcome: 'retry', left: 15, width: 4, label: 'try 1 · 503' },
    { name: 'helm template', outcome: 'ok', left: 19, width: 6, label: 'try 2 · 52 s' },
    { name: 'ask_agent · smoke test', outcome: 'ok', left: 25, width: 17, label: '3 min 05 s' },
    { name: 'model · edit values ×4', outcome: 'model', left: 42, width: 48, label: '9 min' },
    { name: 'shell · git commit', outcome: 'ok', left: 90, width: 3, label: '3 s' },
    { name: 'create_pull_request', outcome: 'wait', left: 93, width: 7, label: 'waiting' },
  ] as { name: string; outcome: 'ok' | 'err' | 'retry' | 'model' | 'wait'; left: number; width: number; label: string }[],
  /** Log lines; redactedAfter marks a secret cut out of the line, shown as a redaction chip, then the rest. */
  log: [
    { n: 36, time: '13:17:44', msg: 'helm template cert-manager jetstack/cert-manager --version v1.19.0' },
    { n: 37, time: '13:17:51', msg: 'error: GET oci://quay.io/jetstack/charts: 503 Service Unavailable', level: 'warn' },
    { n: 38, time: '13:17:51', msg: 'retrying helm template in 10 s (attempt 2 of 3)', level: 'warn' },
    { n: 39, time: '13:18:02', msg: 'rendered 41 manifests, 3 changed: Deployment/cert-manager-webhook, CRD/certificates, CRD/issuers' },
    { n: 40, time: '13:18:05', msg: 'ask_agent Release Notes Scout: failed (401 from provider "deepseek-org")', level: 'err' },
    { n: 41, time: '13:18:06', msg: 'continuing without release notes; will flag this in the pull request body', highlight: true },
    { n: 42, time: '13:18:07', msg: 'git clone https://x-access-token:', highlight: true, redactedAfter: '@github.com/acme/platform-gitops' },
    { n: 43, time: '13:18:09', msg: 'git checkout -b lcm/cert-manager-1.19', highlight: true },
    { n: 44, time: '13:18:10', msg: 'editing clusters/prod/cert-manager/values.yaml (image tag, webhook.timeoutSeconds)', highlight: true },
    { n: 45, time: '13:19:40', msg: 'model turn: 18.2k in, 1.4k out, 9.1k cached, $0.07' },
    { n: 46, time: '13:19:52', msg: 'create_pull_request is ask-tier; parking until a person decides', level: 'warn' },
  ] as { n: number; time: string; msg: string; level?: 'warn' | 'err'; highlight?: boolean; redactedAfter?: string }[],
}

// ---------------------------------------------------------------------------------------------------------------
// Agent page and templates

export const lcmTriggers = [
  { name: 'GitHub release', what: 'jetstack/cert-manager, kubernetes/ingress-nginx, kubernetes-sigs/external-dns', when: 'Last fired today 12:58 · 14 this month' },
  { name: 'Schedule', what: 'Upgrade window, Monday 02:00 Europe/Amsterdam', when: 'Next in 4 d 12 h' },
  { name: 'Manual', what: 'From this page, ⌘K or the API', when: 'Last used 2 Oct' },
]

export type Template = {
  id: string
  name: string
  category: 'Lifecycle' | 'Cost' | 'Research' | 'Operations'
  source: string
  description: string
  model: string
  cost: string
  skills: string
  mcp: string
}

export const templates: Template[] = [
  { id: 'lcm-upgrader', name: 'LCM Upgrader', category: 'Lifecycle', source: 'Platform', description: 'Watches releases of the charts you run, prepares the upgrade on a branch and opens a pull request for review.', model: 'gpt-6-luna', cost: '$2 to $8 a day', skills: 'helm-upgrade, release-notes', mcp: 'GitHub, Argo CD' },
  { id: 'finops-analyst', name: 'FinOps Analyst', category: 'Cost', source: 'Platform', description: 'Reads the cloud bill and cluster usage, finds waste and writes the weekly cost report.', model: 'claude-haiku-5.5', cost: '$1 to $3 a day', skills: 'cost-report', mcp: 'AWS Cost Explorer, Prometheus' },
  { id: 'researcher', name: 'Researcher', category: 'Research', source: 'Platform', description: 'Answers technical questions with sources: web search, docs and your repositories.', model: 'deepseek-v4.1-flash', cost: 'Under $1 a task', skills: 'cite-sources', mcp: 'SearXNG, GitHub' },
  { id: 'release-notes-scout', name: 'Release Notes Scout', category: 'Lifecycle', source: 'Org · Maria Okafor', description: 'Summarizes breaking changes in upstream release notes for the upgrader.', model: 'deepseek-v4.1-flash', cost: 'Under $0.20 a run', skills: 'release-notes', mcp: 'GitHub' },
  { id: 'on-call-triage', name: 'On-call Triage', category: 'Operations', source: 'Platform', description: 'Groups alerts from Alertmanager, links runbooks and drafts the incident channel update.', model: 'claude-haiku-5.5', cost: '$1 to $4 a day', skills: 'runbook-lookup', mcp: 'Alertmanager, Grafana' },
  { id: 'docs-gardener', name: 'Docs Gardener', category: 'Research', source: 'Org · procoduck', description: 'Finds stale pages in the platform docs and proposes edits as pull requests.', model: 'claude-haiku-5.5', cost: 'Under $1 a day', skills: 'docs-style', mcp: 'GitHub' },
]

// ---------------------------------------------------------------------------------------------------------------
// Marketplace: MCP servers and skills

export type MarketItem = {
  mono: string
  name: string
  publisher: string
  kind: 'MCP server' | 'Skill'
  description: string
  scan: string
  scanDot: TaskState
  health: string
  healthDot: TaskState
  cli: string
  action: string
  installed?: boolean
  /** The skill id for /preview/skills/$skillId, for skills. */
  skillId?: string
}

export const marketItems: MarketItem[] = [
  { mono: 'AC', name: 'Argo CD', publisher: 'Koinon catalog · argoproj-labs', kind: 'MCP server', description: 'Apps, sync status and diffs. Koinon deploys and runs it for the org.', scan: '0 findings', scanDot: 'completed', health: 'Healthy · 99.9%', healthDot: 'completed', cli: 'koinon mcp add argocd --org platform-engineering', action: 'Install to org', installed: true },
  { mono: 'GH', name: 'GitHub', publisher: 'Remote · github.com', kind: 'MCP server', description: 'Issues, pull requests and code search, with the org connection or your own account.', scan: 'Tools hash pinned', scanDot: 'completed', health: 'Healthy · 210 ms', healthDot: 'completed', cli: 'koinon mcp add github --remote https://api.githubcopilot.com/mcp/', action: 'Use org connection', installed: true },
  { mono: 'PR', name: 'Prometheus', publisher: 'Koinon catalog · pab1it0', kind: 'MCP server', description: 'PromQL queries and alert state, read-only.', scan: '0 findings', scanDot: 'completed', health: 'Not installed', healthDot: 'canceled', cli: 'koinon mcp add prometheus', action: 'Install to org' },
  { mono: 'K8', name: 'Kubernetes', publisher: 'Koinon catalog · containers', kind: 'MCP server', description: 'Read cluster objects and events. Writes need an approved plan.', scan: '1 medium', scanDot: 'stale', health: 'Healthy · 99.4%', healthDot: 'completed', cli: 'koinon mcp add kubernetes --read-only', action: 'Install to org', installed: true },
  { mono: 'hu', name: 'helm-upgrade', publisher: 'Org skill · Platform Engineering', kind: 'Skill', description: 'Render a chart diff against running values and explain CRD changes.', scan: '0 findings · Approved', scanDot: 'completed', health: 'Used by 2 agents', healthDot: 'completed', cli: 'koinon skills add stateracloud/platform-skills@helm-upgrade', action: 'Open', installed: true, skillId: 'helm-upgrade' },
  { mono: 'pdf', name: 'pdf', publisher: 'Platform skill · anthropics/skills', kind: 'Skill', description: 'Read, fill and merge PDF files with the standard library.', scan: '2 low · Approved', scanDot: 'completed', health: 'Used by 1 agent', healthDot: 'completed', cli: 'npx skills add anthropics/skills --skill pdf', action: 'Add to org', skillId: 'pdf' },
  { mono: 'rn', name: 'release-notes', publisher: 'Platform skill · stateracloud', kind: 'Skill', description: 'Find breaking changes in upstream release notes. Needs feedparser.', scan: '0 findings · Approved', scanDot: 'completed', health: 'Used by 2 agents', healthDot: 'completed', cli: 'koinon skills add stateracloud/skills@release-notes', action: 'Open', installed: true, skillId: 'release-notes' },
  { mono: 'gs', name: 'gh-sweeper', publisher: 'skills.sh · 1.2k installs', kind: 'Skill', description: 'Closes stale issues in bulk. A script calls curl on a pasted URL.', scan: '1 high · Needs review', scanDot: 'failed', health: 'Not added', healthDot: 'canceled', cli: 'npx skills add octo-tools/gh-sweeper', action: 'Review before adding', skillId: 'gh-sweeper' },
]

export const marketCategories: { name: string; count: number }[] = [
  { name: 'All', count: 38 },
  { name: 'Code hosts', count: 4 },
  { name: 'Kubernetes', count: 7 },
  { name: 'Observability', count: 6 },
  { name: 'Cloud cost', count: 3 },
  { name: 'Search and docs', count: 9 },
  { name: 'Files and data', count: 9 },
]

// ---------------------------------------------------------------------------------------------------------------
// Providers, prices, secrets and spend

export type Provider = {
  name: string
  kind: string
  source: 'Inherited from Platform' | 'Org override' | 'Org'
  credential: string
  lives: string
  models: number
  state: ConnectionState
  spend: string
  /** Which canned test result this provider shows. */
  test: 'ok' | 'bad'
}

export const providers: Provider[] = [
  { name: 'anthropic', kind: 'Anthropic Messages', source: 'Inherited from Platform', credential: 'anthropic-platform', lives: 'gateway', models: 4, state: 'ok', spend: '$41.20', test: 'ok' },
  { name: 'openrouter', kind: 'OpenRouter · OpenAI chat format', source: 'Org override', credential: 'openrouter-pe', lives: 'gateway', models: 9, state: 'ok', spend: '$96.88', test: 'ok' },
  { name: 'deepseek-org', kind: 'DeepSeek · OpenAI chat format', source: 'Org', credential: 'deepseek-pe', lives: 'OpenShell provider record', models: 2, state: 'failing', spend: '$12.05', test: 'bad' },
  { name: 'vllm-gpu-lab', kind: 'OpenAI-compatible · in-cluster', source: 'Inherited from Platform', credential: 'none (cluster network)', lives: 'gateway', models: 2, state: 'untested', spend: '$0.00', test: 'ok' },
]

export const providerTests = {
  ok: {
    title: 'Test of openrouter passed',
    state: 'ok' as ConnectionState,
    echo: 'Matched: the model returned koinon-echo-7f3a',
    passed: true,
    latency: '412 ms',
    cost: '$0.00004',
    models: '9 of 214 listed are in the price catalog',
    request: 'POST https://openrouter.ai/api/v1/chat/completions\nmodel: deepseek/deepseek-v4.1-flash\nmessages: [{"role":"user","content":"Reply with exactly: koinon-echo-7f3a"}]\nmax_tokens: 8',
    response: '{"id":"gen-1793214","model":"deepseek/deepseek-v4.1-flash","choices":[{"message":{"content":"koinon-echo-7f3a"}}],"usage":{"prompt_tokens":14,"completion_tokens":6}}',
  },
  bad: {
    title: 'Test of deepseek-org failed',
    state: 'failing' as ConnectionState,
    echo: 'No answer: 401 Unauthorized',
    passed: false,
    latency: '96 ms',
    cost: '$0.00',
    models: 'Could not list models',
    request: 'POST https://api.deepseek.com/v1/chat/completions\nmodel: deepseek-v4.1-flash\nmessages: [{"role":"user","content":"Reply with exactly: koinon-echo-7f3a"}]',
    response: '{"error":{"message":"Authentication Fails, Your api key: ****f9c2 is invalid","type":"authentication_error"}}',
  },
}

export type Price = {
  model: string
  provider: string
  match: string
  /** USD per million tokens; null when the provider has no such price. */
  input: number
  output: number
  cacheRead: number | null
  cacheWrite: number | null
  from: string
  source: 'Default' | 'Override'
  /** Priced in tiers by context length. */
  tiered: boolean
}

export const prices: Price[] = [
  { model: 'claude-haiku-5.5', provider: 'anthropic', match: 'claude-haiku-5.5*', input: 1.0, output: 5.0, cacheRead: 0.1, cacheWrite: 1.25, from: '1 Oct 2026', source: 'Default', tiered: false },
  { model: 'claude-sonnet-5', provider: 'anthropic', match: 'claude-sonnet-5*', input: 3.0, output: 15.0, cacheRead: 0.3, cacheWrite: 3.75, from: '15 Oct 2026', source: 'Default', tiered: true },
  { model: 'gpt-6-luna', provider: 'openrouter', match: 'openai/gpt-6-luna', input: 1.25, output: 10.0, cacheRead: 0.125, cacheWrite: null, from: '20 Sep 2026', source: 'Default', tiered: false },
  { model: 'deepseek-v4.1-flash', provider: 'openrouter', match: 'deepseek/deepseek-v4.1-flash', input: 0.14, output: 0.56, cacheRead: 0.014, cacheWrite: null, from: '2 Oct 2026', source: 'Override', tiered: false },
  { model: 'deepseek-v4.1-flash', provider: 'deepseek-org', match: 'deepseek-v4.1-flash', input: 0.12, output: 0.48, cacheRead: 0.012, cacheWrite: null, from: '2 Oct 2026', source: 'Default', tiered: false },
  { model: 'qwen3.5-coder-32b', provider: 'vllm-gpu-lab', match: 'Qwen/Qwen3.5-Coder-32B*', input: 0.0, output: 0.0, cacheRead: null, cacheWrite: null, from: '1 Oct 2026', source: 'Override', tiered: false },
  { model: 'gemini-3-flash', provider: 'openrouter', match: 'google/gemini-3-flash*', input: 0.3, output: 2.5, cacheRead: 0.075, cacheWrite: null, from: '30 Sep 2026', source: 'Default', tiered: true },
]

export type SecretRow = {
  name: string
  addedBy: string
  type: 'Secret' | 'Config'
  value: string
  source: string
  lives: string
  opens: string
}

export const orgSecrets: SecretRow[] = [
  { name: 'github-platform', addedBy: 'added by: GitHub connection', type: 'Secret', value: '•••••••• set 14 Oct', source: 'Inherited from Platform', lives: 'OpenShell provider record', opens: 'api.github.com' },
  { name: 'openrouter-pe', addedBy: 'added by: Providers', type: 'Secret', value: '•••••••• set 2 Oct', source: 'Org override', lives: 'gateway', opens: 'openrouter.ai' },
  { name: 'aws-cost-readonly', addedBy: 'added by: AWS Cost Explorer MCP', type: 'Secret', value: '•••••••• set 9 Oct', source: 'Org', lives: 'secret store', opens: 'ce.us-east-1.amazonaws.com' },
  { name: 'ARGOCD_URL', addedBy: 'procoduck', type: 'Config', value: 'https://argocd.platform.internal', source: 'Org', lives: 'config', opens: '—' },
  { name: 'DEFAULT_BRANCH_PROTECTION', addedBy: 'Platform', type: 'Config', value: 'require-review', source: 'Inherited from Platform', lives: 'config', opens: '—' },
]

export const agentSecrets: SecretRow[] = [
  { name: 'ARGOCD_URL', addedBy: 'Maria Okafor', type: 'Config', value: 'https://argocd.staging.internal', source: 'Agent override', lives: 'config', opens: '—' },
  { name: 'github-platform', addedBy: 'added by: GitHub connection', type: 'Secret', value: '•••••••• set 14 Oct', source: 'Inherited from Platform', lives: 'OpenShell provider record', opens: 'api.github.com' },
  { name: 'openrouter-pe', addedBy: 'added by: Providers', type: 'Secret', value: '•••••••• set 2 Oct', source: 'Inherited from org', lives: 'gateway', opens: 'openrouter.ai' },
]

export type Budget = { layer: 'Platform' | 'Org' | 'Agent' | 'API key'; name: string; note: string; used: number; limit: number; inherited?: boolean }

export const budgets: Budget[] = [
  { layer: 'Platform', name: 'All orgs on this install', note: 'Set by platform admins', used: 64.1, limit: 200 },
  { layer: 'Org', name: 'Platform Engineering', note: 'From the platform default', used: 11.78, limit: 25, inherited: true },
  { layer: 'Agent', name: 'LCM Upgrader', note: 'Custom', used: 9.12, limit: 10 },
  { layer: 'API key', name: 'ci-pipeline', note: 'From the org default for keys', used: 3, limit: 3, inherited: true },
]

/** Daily org spend in October, USD; the last three days are projected (Spend.dc.html). */
export const spendByDay: number[] = [9, 7, 8, 6, 12, 5, 4, 8, 9, 7, 10, 6, 22, 9, 8, 7, 9, 6, 5, 8, 11, 9, 10, 12, 8, 7, 24, 12]
export const spendProjectedPerDay = 14

export const spendRatios = [
  { name: 'Cache', text: '61% hit · 39% miss', parts: [61, 39, 0] },
  { name: 'Outcomes', text: '91% ok · 6% error · 3% timeout', parts: [91, 3, 6] },
  { name: 'Credentials', text: '72% own keys · 28% platform', parts: [72, 28, 0] },
]

// ---------------------------------------------------------------------------------------------------------------
// Platform console

export type PlatformOrg = {
  name: string
  slug: string
  owners: string
  members: number
  agents: number
  status: 'Active' | 'Archived'
  statusNote: string
  spend: string
  providers: string
}

export const platformOrgs: PlatformOrg[] = [
  { name: 'Platform Engineering', slug: 'platform-engineering', owners: 'procoduck, Maria Okafor', members: 14, agents: 5, status: 'Active', statusNote: '', spend: '$328.40', providers: 'anthropic, openrouter (override), deepseek-org' },
  { name: 'FinOps', slug: 'finops', owners: 'Lena Vogt', members: 6, agents: 2, status: 'Active', statusNote: '', spend: '$61.12', providers: 'anthropic' },
  { name: 'Developer Experience', slug: 'developer-experience', owners: 'Tomás Rivera', members: 9, agents: 3, status: 'Active', statusNote: '', spend: '$140.77', providers: 'anthropic, vllm-gpu-lab' },
  { name: 'Data Science', slug: 'data-science', owners: 'Priya Natarajan', members: 11, agents: 3, status: 'Active', statusNote: '', spend: '$212.05', providers: 'openrouter' },
  { name: 'Security', slug: 'security', owners: 'Jonas Berg', members: 5, agents: 1, status: 'Active', statusNote: '', spend: '$8.30', providers: 'anthropic' },
  { name: 'Mobile', slug: 'mobile', owners: 'Ada Mensah', members: 0, agents: 0, status: 'Archived', statusNote: 'since 2 Sep · read-only', spend: '$0.00', providers: 'none' },
]

export type PlatformAgent = {
  name: string
  org: string
  owner: string
  /** The owner left the install. */
  ownerLeft?: string
  state: AgentState
  model: string
  template: string
  spend: string
  lastRun: string
}

export const platformAgents: PlatformAgent[] = [
  { name: 'LCM Upgrader', org: 'Platform Engineering', owner: 'procoduck', state: 'active', model: 'gpt-6-luna', template: 'LCM Upgrader v3', spend: '$142.30', lastRun: 'running' },
  { name: 'FinOps Analyst', org: 'Platform Engineering', owner: 'procoduck', state: 'active', model: 'claude-haiku-5.5', template: 'FinOps Analyst v2', spend: '$61.02', lastRun: 'running' },
  { name: 'Researcher', org: 'Platform Engineering', owner: 'Maria Okafor', state: 'active', model: 'deepseek-v4.1-flash', template: 'Researcher v4', spend: '$22.40', lastRun: '26 min ago' },
  { name: 'Release Notes Scout', org: 'Platform Engineering', owner: 'Maria Okafor', state: 'unhealthy', model: 'deepseek-v4.1-flash', template: 'org template', spend: '$3.10', lastRun: 'failed yesterday' },
  { name: 'Docs Gardener', org: 'Platform Engineering', owner: 'procoduck', state: 'draft', model: 'claude-haiku-5.5', template: 'org template', spend: '$0.00', lastRun: 'never' },
  { name: 'Notebook Reviewer', org: 'Data Science', owner: 'Priya Natarajan', state: 'active', model: 'claude-sonnet-5', template: 'none', spend: '$118.44', lastRun: '2 h ago' },
  { name: 'Feature Store Steward', org: 'Data Science', owner: 'Ken Ito', ownerLeft: 'left 12 Oct', state: 'active', model: 'gpt-6-luna', template: 'none', spend: '$71.20', lastRun: '5 h ago' },
  { name: 'Cost Watch', org: 'FinOps', owner: 'Lena Vogt', state: 'active', model: 'claude-haiku-5.5', template: 'FinOps Analyst v2', spend: '$40.90', lastRun: '1 h ago' },
  { name: 'PR Reviewer', org: 'Developer Experience', owner: 'Tomás Rivera', state: 'active', model: 'qwen3.5-coder-32b', template: 'none', spend: '$0.00', lastRun: '4 min ago' },
  { name: 'CVE Triage', org: 'Security', owner: 'Jonas Berg', state: 'archived', model: 'claude-haiku-5.5', template: 'none', spend: '$8.30', lastRun: '3 Oct' },
]
export const platformAgentTotal = 14

export type UsageRow = { name: string; cost: string; share: number; tokens: string; calls: string; cache: string; errors: string }

export const platformUsage: Record<'org' | 'model' | 'provider', UsageRow[]> = {
  org: [
    { name: 'Platform Engineering', cost: '$328.40', share: 44, tokens: '171M', calls: '7,920', cache: '61%', errors: '2.1%' },
    { name: 'Data Science', cost: '$212.05', share: 28, tokens: '133M', calls: '4,410', cache: '49%', errors: '0.8%' },
    { name: 'Developer Experience', cost: '$140.77', share: 19, tokens: '74M', calls: '3,980', cache: '52%', errors: '1.2%' },
    { name: 'FinOps', cost: '$61.12', share: 8, tokens: '31M', calls: '1,860', cache: '84%', errors: '0.4%' },
    { name: 'Security', cost: '$8.30', share: 1, tokens: '3M', calls: '232', cache: '70%', errors: '0%' },
  ],
  model: [
    { name: 'gpt-6-luna', cost: '$301.11', share: 40, tokens: '96M', calls: '3,104', cache: '41%', errors: '1.0%' },
    { name: 'claude-sonnet-5', cost: '$214.60', share: 29, tokens: '58M', calls: '2,210', cache: '63%', errors: '0.6%' },
    { name: 'claude-haiku-5.5', cost: '$151.20', share: 20, tokens: '139M', calls: '7,700', cache: '78%', errors: '0.3%' },
    { name: 'deepseek-v4.1-flash', cost: '$83.73', share: 11, tokens: '112M', calls: '5,140', cache: '55%', errors: '3.9%' },
    { name: 'qwen3.5-coder-32b (vLLM)', cost: '$0.00', share: 0, tokens: '7M', calls: '248', cache: '—', errors: '0%' },
  ],
  provider: [
    { name: 'openrouter', cost: '$384.84', share: 51, tokens: '208M', calls: '8,244', cache: '47%', errors: '1.6%' },
    { name: 'anthropic', cost: '$365.80', share: 49, tokens: '197M', calls: '9,910', cache: '71%', errors: '0.4%' },
    { name: 'vllm-gpu-lab', cost: '$0.00', share: 0, tokens: '7M', calls: '248', cache: '—', errors: '0%' },
  ],
}

/** Install-wide daily spend for the last 56 days, USD (Platform-usage.dc.html). */
export const platformSpendByDay: number[] = [18, 22, 20, 25, 19, 9, 8, 21, 24, 26, 23, 30, 11, 9, 24, 27, 22, 25, 29, 12, 10, 26, 28, 24, 31, 27, 13, 11, 25, 29, 30, 27, 33, 14, 12, 28, 31, 26, 34, 29, 15, 12, 30, 33, 29, 36, 31, 14, 13, 38, 32, 30, 35, 33, 16, 31]

export const sharedMcpServers: { name: string; org: string; kind: 'remote' | 'catalog'; health: ConnectionState; tools: string }[] = [
  { name: 'GitHub', org: 'Platform Engineering', kind: 'remote', health: 'ok', tools: '31 tools · hash pinned' },
  { name: 'Argo CD', org: 'Platform Engineering', kind: 'catalog', health: 'ok', tools: '12 tools' },
  { name: 'Notion', org: 'Data Science', kind: 'remote', health: 'failing', tools: '18 tools · hash changed 2 d ago' },
  { name: 'Kubernetes', org: 'Developer Experience', kind: 'catalog', health: 'ok', tools: '9 tools, read-only' },
  { name: 'Jira', org: 'FinOps', kind: 'remote', health: 'untested', tools: '24 tools' },
]

// ---------------------------------------------------------------------------------------------------------------
// Helpers shared by screens

/** usd formats dollars with cents: 11.78 → "$11.78". */
export function usd(n: number): string {
  return '$' + n.toFixed(2)
}

/** pct is the whole percentage used of a limit. */
export function pct(used: number, limit: number): number {
  return limit <= 0 ? 0 : Math.round((used / limit) * 100)
}
