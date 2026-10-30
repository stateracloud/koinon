// SPDX-License-Identifier: Apache-2.0

// Screen-only data for the run detail page (Run.dc.html) that the shared mock module lacks.

export const runFacts = {
  shortId: '9c41d07a',
  agentId: 'lcm-upgrader',
  model: 'gpt-6-luna',
  provider: 'OpenRouter',
  trigger: 'GitHub release',
  triggerWhat: 'jetstack/cert-manager v1.19.0',
  version: 'v7',
  digest: 'sha256:4be1…c09a',
  started: 'Today 13:02 · 18 min 40 s',
  tokens: '412k',
  tokenSplit: '301k in · 18k out · 93k cached',
  cost: '$1.38',
  agentToday: 'agent today $9.12 of $10.00',
  summaryMeta: '12 steps · 3 repeats collapsed · 1 retry',
}

export const subRuns = [
  { id: '5be2a913', state: 'failed' as const, title: 'Release Notes Scout · Read cert-manager 1.19 notes', meta: 'failed · 42 s · $0.01' },
  { id: 'c03e88f1', state: 'completed' as const, title: 'Staging smoke test · cert-manager webhook', meta: 'succeeded · 3 min 05 s · $0.06' },
]

export const timelineTicks = [
  { at: 0, label: '13:02' },
  { at: 25, label: '13:07' },
  { at: 50, label: '13:11' },
  { at: 75, label: '13:16' },
  { at: 100, label: 'now' },
]

export const rawEvents = [
  '{"seq":118,"at":"2026-10-28T13:19:52Z","type":"tool.call","tool":"github.create_pull_request","run":"9c41d07a","args":{"repo":"platform-gitops","head":"lcm/cert-manager-1.19","base":"main"},"tier":"ask"}',
  '{"seq":117,"at":"2026-10-28T13:19:40Z","type":"model.turn","model":"gpt-6-luna","tokens":{"in":18211,"out":1406,"cache_read":9120},"cost_usd":0.0712,"latency_ms":6140}',
  '{"seq":116,"at":"2026-10-28T13:18:02Z","type":"tool.result","tool":"shell","exit":0,"bytes":4211}',
  '{"seq":115,"at":"2026-10-28T13:17:51Z","type":"tool.retry","tool":"helm.template","attempt":2,"reason":"OCI registry 503"}',
]
