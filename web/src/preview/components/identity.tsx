// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { hueOf, initialsOf, markStyle } from '@/preview/lib/format'
import type { Hue } from '@/preview/mock/data'

/**
 * AgentAvatar is Koinon.AgentAvatar: the agent's initials on its categorical hue. Pass decorative when the name is
 * written next to it (the usual case); otherwise it is an image named after the agent.
 */
export function AgentAvatar({ name, size = 'sm', decorative, hue, className }: { name: string; size?: 'sm' | 'lg'; decorative?: boolean; hue?: Hue; className?: string }) {
  const a11y = decorative ? { 'aria-hidden': true as const } : { role: 'img', 'aria-label': name }
  return (
    <span className={cn('kn-avatar', className)} data-size={size} style={markStyle(hue ?? hueOf(name))} {...a11y}>
      {initialsOf(name)}
    </span>
  )
}

/** SpaceMark is the square mark of a space in the switcher: an org's initials, Idion's "Id", or an icon. */
export function SpaceMark({ hue, children, className }: { hue: Hue; children: ReactNode; className?: string }) {
  return (
    <span className={cn('sb-mark', className)} style={markStyle(hue)} aria-hidden="true">
      {children}
    </span>
  )
}

/**
 * Logo is Koinon.Logo: the K monogram (stem and upper arm in primary, the lower leg in logo-gold; teal for Idion)
 * with the wordmark. size is the mark's height in px.
 */
export function Logo({ product = 'koinon', layout = 'horizontal', size = 22, wordmark = true, className }: { product?: 'koinon' | 'idion'; layout?: 'horizontal' | 'stacked'; size?: number; wordmark?: boolean; className?: string }) {
  const word = product === 'idion' ? 'Idion' : 'Koinon'
  const fontSize = Math.round((size * 3) / 4)
  return (
    <span className={cn('kn-logo', className)} data-product={product} data-layout={layout} style={{ gap: Math.round(size / 4) }}>
      <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" focusable="false">
        <rect className="kn-k-stem" x="4" y="2" width="4.5" height="20" rx="1" />
        <path className="kn-k-arm" d="M8.5 13.2 16.4 2H21.6L11.6 15.4Z" />
        <path className="kn-k-leg" d="M11.4 12.6 14.5 10.4 21.8 22H16.6Z" />
      </svg>
      {wordmark ? (
        <span className="kn-wordmark" style={{ fontSize }}>
          {word}
        </span>
      ) : (
        <span className="ks-sr">{word}</span>
      )}
    </span>
  )
}
