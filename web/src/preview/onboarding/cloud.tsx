// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties, ReactNode } from 'react'
import { cn } from '@/lib/utils'

// The mockups lay a soft cloud image behind onboarding and Idion; the image is not in the preview, so two blurred
// radial gradients stand in for it. The high-contrast theme drops it (ks-cloudy).
const cloud: CSSProperties = {
  backgroundImage: 'radial-gradient(60% 50% at 20% 0%, color-mix(in srgb, var(--teal-400) 22%, transparent), transparent), radial-gradient(50% 45% at 85% 10%, color-mix(in srgb, var(--blue-400) 16%, transparent), transparent)',
}

/** Cloudy is a soft-cloud ground for the page behind it. */
export function Cloudy({ children, className, style }: { children: ReactNode; className?: string; style?: CSSProperties }) {
  return (
    <div className={cn('ks-cloudy', className)} style={{ ...cloud, ...style }}>
      {children}
    </div>
  )
}
