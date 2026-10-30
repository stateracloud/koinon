// SPDX-License-Identifier: Apache-2.0

// Screen-only data of the agents group, seeded from NewAgent-*.dc.html, Agent.dc.html and Agent-rollback.dc.html.

export const categories = ['All', 'Lifecycle', 'Cost', 'Research', 'Operations'] as const

export const reportsToOptions = ['No agent (sits under the org)', 'LCM Upgrader', 'Researcher', 'FinOps Analyst']

export const blankReportsToOptions = ['No agent (sits under the org)', 'FinOps Analyst', 'LCM Upgrader', 'Researcher']

export const blankModelOptions = [
  'Use the org default (deepseek-v4.1-flash)',
  'claude-haiku-5.5 · Anthropic, platform',
  'gpt-6-luna · OpenRouter, org',
]

export const templateCategoryOptions = ['Operations', 'Cost', 'Lifecycle']

export type BlankTrigger = { id: string; kind: string; cron: string; text: string }

export const blankTrigger: BlankTrigger = { id: 'sched', kind: 'Schedule', cron: '0 3 * * *', text: 'every day at 03:00 Europe/Amsterdam' }

export const lcmTemplate = {
  name: 'LCM Upgrader',
  version: 'v3 · sha256:91d0…7e2b',
  description:
    'Watches releases of the Helm charts you run, prepares each upgrade on a branch, tests it in staging and opens a pull request. Nothing reaches the default branch without your approval.',
  instructions: `You keep the platform's Helm releases current.
1. When a watched chart publishes a release, read its notes (ask Release Notes Scout if it exists).
2. Render the diff against the running values. Stop and ask if a CRD changes.
3. Apply to staging only through an approved plan, then run the smoke test.
4. Open a pull request with the diff, the notes and the test result. Never merge yourself.`,
  modelOptions: ['gpt-6-luna (template default)', 'deepseek-v4.1-flash (org default)', 'claude-haiku-5.5'],
}

export type SkillRow = {
  name: string
  kind: string
  pinned: string
  status: { type: 'approved' } | { type: 'connection'; state: 'ok' }
  cost: string
  notes: { text: string; tone?: 'info' | 'warn' | 'muted'; extra?: string }
  link?: boolean
}

export const skillRows: SkillRow[] = [
  { name: 'helm-upgrade', kind: 'Skill · org', pinned: 'v4 · 3c7a…41f0', status: { type: 'approved' }, cost: '96 tokens', notes: { text: 'v5 available', tone: 'info', extra: ' · staged in v8' }, link: true },
  { name: 'release-notes', kind: 'Skill · platform', pinned: 'v2 · e019…aa73', status: { type: 'approved' }, cost: '54 tokens', notes: { text: 'Needs packages: feedparser', tone: 'warn' }, link: true },
  { name: 'GitHub', kind: 'MCP · remote', pinned: 'tools hash 77b0…', status: { type: 'connection', state: 'ok' }, cost: '1.9k tokens', notes: { text: 'Credential lives in the OpenShell provider record "github-platform"', tone: 'muted' } },
  { name: 'Argo CD', kind: 'MCP · catalog', pinned: 'v0.4.1', status: { type: 'connection', state: 'ok' }, cost: '640 tokens', notes: { text: 'Read-only tools', tone: 'muted' } },
]

export type DiffLine = { kind: 'ctx' | 'add' | 'del'; text: string }

export const diffLines: DiffLine[] = [
  { kind: 'ctx', text: '  agent: lcm-upgrader   v7 → v8' },
  { kind: 'del', text: '- model: gpt-6-luna' },
  { kind: 'add', text: '+ model: claude-haiku-5.5' },
  { kind: 'ctx', text: '  skills:' },
  { kind: 'del', text: '-   - helm-upgrade@sha256:3c7a…41f0   (v4)' },
  { kind: 'add', text: '+   - helm-upgrade@sha256:8e11…02bd   (v5, needs review: 1 medium)' },
  { kind: 'ctx', text: '    - release-notes@sha256:e019…aa73' },
]

export type VersionRow = { id: string; change: string; by: string; when: string }

export const olderVersions: VersionRow[] = [
  { id: 'v6', change: 'Daily limit $8 to $10', by: 'Andre Staats', when: '14 Oct' },
  { id: 'v5', change: 'Created from template v3', by: 'Andre Staats', when: '2 Oct' },
]

export const modelOptions = [
  { id: 'pin', label: 'gpt-6-luna · OpenRouter (org provider)', model: 'gpt-6-luna' },
  { id: 'haiku', label: 'claude-haiku-5.5 · Anthropic (platform)', model: 'claude-haiku-5.5' },
  { id: 'inherit', label: 'Inherit the org default', model: 'deepseek-v4.1-flash' },
] as const

export type ModelChoice = (typeof modelOptions)[number]['id']

export const rollback = {
  from: 'v7',
  to: 'v6',
  changes: [
    'Triggers: the external-dns release trigger is removed',
    'Model: unchanged, gpt-6-luna',
    'Skills: unchanged, pinned by digest',
    'MCP servers: unchanged',
    'Schedules follow v6: Monday 02:00',
  ],
  unchanged: ['Secrets and provider credentials', 'Memory and the home volume', "Today's spend and the daily limit", 'The org\'s "Disable schedules" switch'],
  reason: 'external-dns trigger fires on pre-releases and opened 3 noisy PRs overnight.',
}
