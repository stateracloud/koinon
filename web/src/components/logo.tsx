// SPDX-License-Identifier: Apache-2.0

import type { SVGProps } from 'react'
import { cn } from '@/lib/utils'

/**
 * The K monogram (ADR 0126 section 1, point 3): the stem and the upper arm use the brand colour (`primary`),
 * the lower leg uses the logo gold. The shapes are the ADR's, in a 48 x 48 box.
 */
export function KMonogram({ className, ...props }: SVGProps<SVGSVGElement>) {
  return (
    <svg viewBox="0 0 48 48" aria-hidden="true" focusable="false" className={cn('size-6 shrink-0', className)} {...props}>
      <rect x="6" y="6" width="11.5" height="36" rx="3" className="fill-primary" />
      <path d="M19.5 24L31.5 6H43.5L31.5 24Z" className="fill-primary" />
      <path d="M19.5 24H31.5L43.5 42H31.5Z" className="fill-logo-gold" />
    </svg>
  )
}

/** Logo is the monogram with the wordmark: "Koinon" in Geist 600 at -0.045em letter spacing. */
export function Logo({ className }: { className?: string }) {
  return (
    <span className={cn('inline-flex items-center gap-2', className)}>
      <KMonogram />
      <span className="text-lg font-semibold tracking-[-0.045em]">Koinon</span>
    </span>
  )
}
