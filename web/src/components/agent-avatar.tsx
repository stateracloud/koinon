// SPDX-License-Identifier: Apache-2.0

import { cn } from '@/lib/utils'

// ADR 0126 section 2: an agent avatar is a categorical fill. The initial uses `primary-foreground` (the dark ground in
// the dark theme, white in the light one); `web/src/lib/contrast.test.ts` checks every pair. Sky is not in the list:
// the light sky fill gives 3.3:1 with white and 5.1:1 only with the dark ground, which the light theme has no token for.
const fills = ['bg-cat-orange', 'bg-cat-gold', 'bg-cat-green', 'bg-cat-teal', 'bg-cat-blue', 'bg-cat-lavender', 'bg-cat-slate'] as const

/** avatarFill picks the fill of an agent from its id, so an agent keeps its colour on every page and every reload. */
function avatarFill(agentId: string): (typeof fills)[number] {
  let h = 0
  for (const c of agentId) h = (h * 31 + c.charCodeAt(0)) >>> 0
  return fills[h % fills.length]
}

/**
 * AgentAvatar is the round mark of an agent: its initial on its colour. It is decorative (aria-hidden): the agent's
 * name is always text next to it, so the colour and the initial are never the only signal.
 */
export function AgentAvatar({ agentId, name, className }: { agentId: string; name?: string; className?: string }) {
  const initial = (name ?? '').trim().charAt(0).toUpperCase() || '?'
  return (
    <span
      aria-hidden
      data-testid="agent-avatar"
      className={cn('text-primary-foreground inline-flex size-6 shrink-0 items-center justify-center rounded-full text-xs font-semibold', avatarFill(agentId), className)}
    >
      {initial}
    </span>
  )
}
