// SPDX-License-Identifier: Apache-2.0

// Screen-only data for the platform console (Platform-*.dc.html) that mock/data.ts lacks.

/** Share of each org's monthly limit used. Only for the "Over 80% of limit" view; the mockup names no numbers. */
export const orgLimitPercent: Record<string, number> = {
  'Platform Engineering': 86,
  FinOps: 24,
  'Developer Experience': 52,
  'Data Science': 91,
  Security: 8,
  Mobile: 0,
}

export type SharedProvider = {
  name: string
  credential: string
  models: string
  assigned: string
  overriddenBy: string
  shared: boolean
  spend: string
}

export const sharedProviders: SharedProvider[] = [
  { name: 'anthropic', credential: 'credential anthropic-platform · lives in gateway', models: 'claude-haiku-5.5, claude-sonnet-5', assigned: 'All orgs', overriddenBy: 'Platform Engineering (pending save)', shared: true, spend: '$365.80' },
  { name: 'openrouter', credential: 'credential openrouter-platform · lives in gateway', models: 'gpt-6-luna, deepseek-v4.1-flash, gemini-3-flash', assigned: 'Data Science, Developer Experience', overriddenBy: 'Platform Engineering', shared: false, spend: '$287.96' },
  { name: 'vllm-gpu-lab', credential: 'in-cluster · no credential', models: 'qwen3.5-coder-32b', assigned: 'Developer Experience, Platform Engineering', overriddenBy: 'none', shared: true, spend: '$0.00' },
]

export type PlatformTemplate = { name: string; version: string; agents: number; orgs: number; skills: string }

export const platformTemplates: PlatformTemplate[] = [
  { name: 'LCM Upgrader', version: 'v3', agents: 4, orgs: 3, skills: 'helm-upgrade, release-notes' },
  { name: 'FinOps Analyst', version: 'v2', agents: 3, orgs: 2, skills: 'cost-report' },
  { name: 'Researcher', version: 'v4', agents: 6, orgs: 5, skills: 'cite-sources' },
]

export type SettingsValues = {
  selfService: boolean
  grace: string
  ageing1: string
  ageing2: string
  ageing3: string
  capPlatform: string
  capOrg: string
  capAgent: string
  capKey: string
  gate: 'auto' | 'auto-if-clean' | 'human'
  threshold: 'high' | 'medium' | 'critical'
  osv: boolean
  browse: boolean
  idionCap: string
}

/** What is applied today; the mockup stages "Skills gate auto → auto-if-clean" on top of it. */
export const appliedSettings: SettingsValues = {
  selfService: true,
  grace: '7 days',
  ageing1: '1 d',
  ageing2: '3 d',
  ageing3: '7 d',
  capPlatform: '$200.00',
  capOrg: '$25.00',
  capAgent: '$5.00',
  capKey: '$3.00',
  gate: 'auto',
  threshold: 'high',
  osv: false,
  browse: true,
  idionCap: '$10.00',
}

export const stagedSettings: SettingsValues = { ...appliedSettings, gate: 'auto-if-clean' }

export const settingLabels: Record<keyof SettingsValues, string> = {
  selfService: 'Let people create their own org',
  grace: 'Purge deleted orgs after',
  ageing1: 'Waiting on you, first step',
  ageing2: 'Waiting on you, second step',
  ageing3: 'Waiting on you, third step',
  capPlatform: 'Platform cap, per day',
  capOrg: 'Default for a new org, per day',
  capAgent: 'Default for a new agent, per day',
  capKey: 'Default for an API key, per day',
  gate: 'Skills gate',
  threshold: 'Skills threshold',
  osv: 'Look up packages in OSV',
  browse: 'Browse skills.sh and configured catalogs',
  idionCap: 'Idion cap per personal space, per month',
}
