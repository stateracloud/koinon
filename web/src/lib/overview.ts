// SPDX-License-Identifier: Apache-2.0

/** OverviewTab is a tab of the org home; the URL carries it as ?tab= (Agents, the default, has none). */
export type OverviewTab = 'agents' | 'board'

export function parseOverviewTab(v: unknown): OverviewTab | undefined {
  return v === 'agents' || v === 'board' ? v : undefined
}
