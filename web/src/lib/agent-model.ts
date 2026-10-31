// SPDX-License-Identifier: Apache-2.0

import { ModelSource, type GetAgentModelResponse } from '@/gen/koinon/agent/v1alpha1/agent_pb'

/** The picker's value for "no setting": follow the version, then the org default. */
export const defaultModelChoice = 'default'

/** modelSourceLabel says where the effective model comes from, in the words of the precedence. */
export function modelSourceLabel(source: ModelSource): string {
  switch (source) {
    case ModelSource.AGENT:
      return "set on this agent"
    case ModelSource.VERSION:
      return "from the agent's version (its template)"
    case ModelSource.ORG_DEFAULT:
      return "the org default (the first model the org serves with a price)"
    default:
      return 'none'
  }
}

/** modelChoices lists the picker's models: the ones the org serves with a price, plus the agent's own setting when it is no longer one of them. */
export function modelChoices(m: GetAgentModelResponse): { name: string; label: string }[] {
  const out = m.available.map((a) => ({ name: a.name, label: `${a.name} (${a.provider})` }))
  if (m.setting !== '' && !out.some((c) => c.name === m.setting)) out.push({ name: m.setting, label: `${m.setting} (not served)` })
  return out
}

/** thinkingSourceLabel says where the thinking setting comes from, in the words of the precedence: the agent's own, then the version's (its template), then the default, on. */
export function thinkingSourceLabel(setting: string, versionSetting: string): string {
  if (setting !== '') return `Set on this agent (${setting}).`
  if (versionSetting !== '') return `From the agent's version (its template): ${versionSetting}.`
  return 'On by default.'
}
