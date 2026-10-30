// SPDX-License-Identifier: Apache-2.0

import type { ReactNode } from 'react'
import { usePreview } from '@/preview/lib/context'
import { idion, type Hue } from '@/preview/mock/data'
import type { Space } from '@/preview/shell/nav'

/** useSpaceMeta is the switcher button's mark and two lines for the current space. */
export function useSpaceMeta(space: Space): { name: string; kind: string; hue: Hue; mark: ReactNode } {
  const { org } = usePreview()
  if (space === 'platform') return { name: 'Platform', kind: 'Platform admin', hue: 'gray', mark: 'P' }
  if (space === 'idion') return { name: idion.name, kind: 'Your own space', hue: idion.hue, mark: idion.initials }
  return { name: org.name, kind: `Organization · ${org.role}`, hue: org.hue, mark: org.initials }
}
