// SPDX-License-Identifier: Apache-2.0

import type { Server } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { OrgRef } from '@/gen/koinon/platform/v1alpha1/resources_pb'
import type { Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { healthLabel } from '@/lib/mcp-servers'
import { servingBadge } from '@/lib/providers'

/** platformOwnerName is the text of the Org column of the platform console: the org's display name, else its slug, else "Platform" for a row that no org owns (a platform template). */
export function platformOwnerName(org: OrgRef | undefined): string {
  if (!org) return 'Platform'
  return org.displayName || org.slug
}

/** providerStatusText is one word or phrase for the state of a provider, the same order of checks as the org's Providers page. */
export function providerStatusText(p: Provider): string {
  if (p.removed) return 'Removed'
  if (p.adminDisabled) return 'Held off'
  if (p.disabledReason) return 'Disabled'
  if (p.credentialLostAt) return 'Key lost'
  return servingBadge(p)?.label ?? 'Active'
}

/** serverStatusText is one word or phrase for the state of a remote MCP server. */
export function serverStatusText(s: Server): string {
  if (s.removed) return 'Removed'
  if (s.adminDisabled) return 'Held off'
  if (s.disabledReason) return 'Disabled'
  if (s.credentialLostAt) return 'Credential lost'
  return healthLabel(s.health)
}

/** matchesResourceSearch says whether a row matches the search text: a part of one of its texts, in any letter case. */
export function matchesResourceSearch(q: string, ...texts: readonly string[]): boolean {
  const t = q.trim().toLowerCase()
  return !t || texts.some((x) => x.toLowerCase().includes(t))
}
