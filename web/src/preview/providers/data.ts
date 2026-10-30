// SPDX-License-Identifier: Apache-2.0

// Screen-only data of the providers group (Provider-add, Prices, Spend) that the shared mock module lacks.

export const providerKinds = [
  { name: 'Anthropic', format: 'Messages API', baseUrl: 'https://api.anthropic.com' },
  { name: 'OpenAI', format: 'Responses, chat', baseUrl: 'https://api.openai.com/v1' },
  { name: 'OpenRouter', format: 'OpenAI chat', baseUrl: 'https://openrouter.ai/api/v1' },
  { name: 'Azure OpenAI', format: 'Responses, chat', baseUrl: '' },
  { name: 'Amazon Bedrock', format: 'Converse', baseUrl: '' },
  { name: 'OpenAI-compatible', format: 'vLLM, Ollama, LiteLLM', baseUrl: '' },
] as const

export type ProviderKindName = (typeof providerKinds)[number]['name']

/** Names of the platform's providers: an org provider with one of these names replaces it. */
export const platformProviderNames = ['anthropic', 'openrouter', 'vllm-gpu-lab']

export const savedCredentials = ['anthropic-platform', 'openrouter-pe', 'deepseek-pe', 'github-platform']

export type DiscoveredModel = { id: string; format: string; context: string; catalog: string | null; on: boolean }

/** What /v1/models returned for the Anthropic example in the mockup. */
export const discoveredModels: DiscoveredModel[] = [
  { id: 'claude-haiku-5.5', format: 'Anthropic Messages', context: '200k', catalog: 'Priced', on: true },
  { id: 'claude-sonnet-5', format: 'Anthropic Messages', context: '1M', catalog: 'Priced · tiered', on: true },
  { id: 'claude-opus-5', format: 'Anthropic Messages', context: '1M', catalog: 'Priced · tiered', on: false },
  { id: 'claude-internal-eval-1028', format: 'unknown', context: '?', catalog: null, on: false },
]

export type PriceTierRow = { label: string; input: number; output: number; cacheRead: number; cacheWrite: number }

/** The claude-sonnet-5 tiers shown in the mockup's drawer. */
export const sonnetTiers: PriceTierRow[] = [
  { label: 'Up to 200k', input: 3.0, output: 15.0, cacheRead: 0.3, cacheWrite: 3.75 },
  { label: 'Over 200k', input: 6.0, output: 22.5, cacheRead: 0.6, cacheWrite: 7.5 },
]

export const sonnetHistory = [
  { date: '15 Oct 2026', what: 'Over-200k tier added', by: 'catalog sync, reviewed by procoduck' },
  { date: '1 Sep 2026', what: 'First price, $3.00 / $15.00', by: 'catalog sync' },
]

export const ledgerSeries = [
  { id: 'a', name: 'LCM Upgrader' },
  { id: 'b', name: 'FinOps Analyst' },
  { id: 'c', name: 'Researcher' },
  { id: 'd', name: 'Others' },
] as const

export const spendKpis = [
  { label: 'Month to date', value: '$328.40', note: '28 of 31 days' },
  { label: 'Projected at month end', value: '$371', note: "at the last 7 days' pace, $14.20 a day" },
  { label: 'Today', value: '$11.78', note: '47% of the $25.00 org limit' },
  { label: 'Cache hit rate', value: '61%', note: 'saved about $88 this month' },
]

export const systemChecks = [
  { level: 'Pass', text: 'Every model in use has a price in the catalog' },
  { level: 'Pass', text: "Ledger matches OpenRouter's usage report within 0.4%" },
  { level: 'Warn', text: '2 runs on vllm-gpu-lab have no cost: its price is $0 by override' },
  { level: 'Fail', text: 'Provider deepseek-org rejects its key (401)', fix: '/preview/providers?test=bad' },
] as const

export const expensiveRuns = [
  { id: '66ab21f0', agent: 'LCM Upgrader', task: 'Upgrade external-dns to 0.16', model: 'gpt-6-luna', tokens: '1.02M', cache: '38%', cost: '$3.14' },
  { id: '18c0fd2e', agent: 'LCM Upgrader', task: 'Upgrade kube-prometheus-stack to 80.2', model: 'gpt-6-luna', tokens: '904k', cache: '41%', cost: '$2.86' },
  { id: 'b41a9e07', agent: 'FinOps Analyst', task: 'Weekly cost report, week 43', model: 'claude-haiku-5.5', tokens: '2.10M', cache: '84%', cost: '$1.97' },
]
