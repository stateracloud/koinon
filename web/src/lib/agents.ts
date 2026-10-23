// SPDX-License-Identifier: Apache-2.0

import type { Grant } from '@/gen/koinon/template/v1alpha1/template_pb'

/** The GitHub connector's target pattern (connectors/github/connector.yaml), used when the connector type is not loaded. */
export const defaultRepositoryPattern = '^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}$'

/** grantNeedsTargets: a grant the user binds to a connection and targets (CreateAgentFromTemplate). */
export function grantNeedsTargets(g: Grant): boolean {
  return g.targetsMin > 0 || g.targetsMax > 0
}

/**
 * validateRepositories checks a grant's repositories against the template's
 * targets min and max and the connector's owner/name pattern. Blank entries
 * are ignored. It returns the problems, or none.
 */
export function validateRepositories(entries: readonly string[], bounds: { min: number; max: number }, pattern = defaultRepositoryPattern): string[] {
  const repos = entries.map((e) => e.trim()).filter(Boolean)
  const errors: string[] = []
  const { min, max } = bounds
  const noun = (n: number) => (n === 1 ? 'repository' : 'repositories')
  if (repos.length < min) {
    errors.push(min === max ? `Choose exactly ${min} ${noun(min)}.` : `Choose at least ${min} ${noun(min)}.`)
  }
  if (max > 0 && repos.length > max) {
    errors.push(min === max ? `Choose exactly ${max} ${noun(max)}.` : `Choose at most ${max} ${noun(max)}.`)
  }
  let re: RegExp | undefined
  try {
    re = new RegExp(pattern)
  } catch {
    re = new RegExp(defaultRepositoryPattern)
  }
  for (const r of repos) {
    if (!re.test(r)) errors.push(`"${r}" is not a repository in the form owner/name.`)
  }
  const seen = new Set<string>()
  for (const r of repos) {
    // GitHub names are case-insensitive.
    const k = r.toLowerCase()
    if (seen.has(k)) errors.push(`"${r}" is listed twice.`)
    seen.add(k)
  }
  return errors
}
